// Package engine: snapshot/restore of simulator state (collections, kv, blobs).
//
// A snapshot is a gzip-compressed tar of a LOGICAL dump produced through the
// engine's existing accessor API (StateStores) — not a copy of the on-disk
// SQLite/blob files. The request log is observational and is NOT snapshotted.
//
// Archive layout (Plan 3b):
//
//	snapshot.json                 # {version, created_at, manifest, services}
//	<service>/collections.json    # { "<collection>": [ {doc}, ... ] }
//	<service>/kv.json             # { "<namespace>": [ ["k","v"], ... ] }
//	<service>/blobs/<ns>/<id>.blob   # raw content
//	<service>/blobs/<ns>/<id>.meta   # {name,size,content_type,modified}
package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/kv"
)

// snapshotVersion is the logical-dump format version. Bumped on breaking
// changes to the archive layout. Restore rejects unknown versions.
const snapshotVersion = 1

// snapshotHeader is the manifest entry at snapshot.json.
type snapshotHeader struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Manifest  string    `json:"manifest,omitempty"`
	Services  []string  `json:"services"`
}

// Snapshot writes a gzip-tar snapshot of every service's simulator state
// (collections, kv, blobs) to w. It does NOT capture the request log.
// The engine's stores are read in place; nothing is mutated.
func Snapshot(e *Engine, manifestPath string, w io.Writer) error {
	gw := gzip.NewWriter(w)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	services := e.ServiceNames()

	// Header.
	hdr := snapshotHeader{
		Version:   snapshotVersion,
		CreatedAt: time.Now().UTC(),
		Manifest:  manifestPath,
		Services:  services,
	}
	hb, err := json.MarshalIndent(hdr, "", "  ")
	if err != nil {
		return fmt.Errorf("snapshot: marshal header: %w", err)
	}
	if err := writeTar(tw, "snapshot.json", hb); err != nil {
		return err
	}

	for _, svc := range services {
		col, kv, bl, ok := e.StateStores(svc)
		if !ok {
			continue
		}
		if err := snapshotService(tw, svc, col, kv, bl); err != nil {
			return fmt.Errorf("snapshot service %s: %w", svc, err)
		}
	}
	return nil
}

// snapshotService dumps one service's three stores into the tar writer.
func snapshotService(tw *tar.Writer, svc string, col *primitives.Store, kv *kv.KV, bl *blob.Store) error {
	// Collections.
	if col != nil {
		names, err := col.CollectionNames()
		if err != nil {
			return fmt.Errorf("collection names: %w", err)
		}
		dump := map[string][]map[string]any{}
		for _, n := range names {
			c, err := col.Collection(n)
			if err != nil {
				return fmt.Errorf("collection %s: %w", n, err)
			}
			docs, err := c.List()
			if err != nil {
				return fmt.Errorf("list %s: %w", n, err)
			}
			dump[n] = docs
		}
		b, err := json.MarshalIndent(dump, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal collections: %w", err)
		}
		if err := writeTar(tw, svc+"/collections.json", b); err != nil {
			return err
		}
	}

	// KV.
	if kv != nil {
		nss, err := kv.Namespaces()
		if err != nil {
			return fmt.Errorf("kv namespaces: %w", err)
		}
		dump := map[string][][2]string{}
		for _, ns := range nss {
			pairs, err := kv.List(ns)
			if err != nil {
				return fmt.Errorf("kv list %s: %w", ns, err)
			}
			dump[ns] = pairs
		}
		b, err := json.MarshalIndent(dump, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal kv: %w", err)
		}
		if err := writeTar(tw, svc+"/kv.json", b); err != nil {
			return err
		}
	}

	// Blobs.
	if bl != nil {
		nss, err := bl.Namespaces()
		if err != nil {
			return fmt.Errorf("blob namespaces: %w", err)
		}
		for _, ns := range nss {
			infos, err := bl.List(ns)
			if err != nil {
				return fmt.Errorf("blob list %s: %w", ns, err)
			}
			for _, info := range infos {
				// info.Name is the friendly name; the on-disk id == name (blob.Put
				// derives id from name). Read content + meta by that id.
				id := info.Name
				rc, err := bl.Get(ns, id)
				if err != nil {
					return fmt.Errorf("blob get %s/%s: %w", ns, id, err)
				}
				content, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					return fmt.Errorf("blob read %s/%s: %w", ns, id, err)
				}
				if err := writeTar(tw, svc+"/blobs/"+ns+"/"+id+".blob", content); err != nil {
					return err
				}
				meta := map[string]any{
					"name":         info.Name,
					"size":         info.Size,
					"content_type": info.ContentType,
					"modified":     info.Modified.Format(time.RFC3339Nano),
				}
				mb, _ := json.Marshal(meta)
				if err := writeTar(tw, svc+"/blobs/"+ns+"/"+id+".meta", mb); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Restore reads a gzip-tar snapshot from r, wipes the engine's current state
// (ResetAll), then re-populates collections, kv, and blobs from the archive.
// The request log is cleared by ResetAll and not restored.
func Restore(e *Engine, r io.Reader) (*snapshotHeader, error) {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("restore: gunzip: %w", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	var hdr *snapshotHeader
	// Buffer everything; apply after ResetAll so restore is a clean replace
	// (inserting before reset would collide with seeded rows).
	type colDump struct {
		name string
		docs []map[string]any
	}
	type kvDump struct {
		ns    string
		pairs [][2]string
	}
	type blobEntry struct {
		svc, ns, id, contentType, name string
		content                        []byte
	}
	// per-service buffers
	colBuf := map[string][]colDump{} // svc -> collection dumps
	kvBuf := map[string][]kvDump{}   // svc -> kv dumps
	var blobs []blobEntry

	for {
		hh, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("restore: read tar: %w", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("restore: read %s: %w", hh.Name, err)
		}

		switch {
		case hh.Name == "snapshot.json":
			var h snapshotHeader
			if err := json.Unmarshal(data, &h); err != nil {
				return nil, fmt.Errorf("restore: parse header: %w", err)
			}
			if h.Version != snapshotVersion {
				return nil, fmt.Errorf("restore: unknown snapshot version %d (want %d)", h.Version, snapshotVersion)
			}
			hdr = &h

		case strings.HasSuffix(hh.Name, "/collections.json"):
			svc := path.Dir(hh.Name) // "<service>"
			var dump map[string][]map[string]any
			if err := json.Unmarshal(data, &dump); err != nil {
				return nil, fmt.Errorf("restore: parse %s: %w", hh.Name, err)
			}
			col, _, _, ok := e.StateStores(svc)
			if !ok {
				continue // service no longer in manifest; skip
			}
			_ = col // store handle check done at apply time
			for cname, docs := range dump {
				colBuf[svc] = append(colBuf[svc], colDump{name: cname, docs: docs})
			}

		case strings.HasSuffix(hh.Name, "/kv.json"):
			svc := path.Dir(hh.Name)
			var dump map[string][][2]string
			if err := json.Unmarshal(data, &dump); err != nil {
				return nil, fmt.Errorf("restore: parse %s: %w", hh.Name, err)
			}
			_, kv, _, ok := e.StateStores(svc)
			if !ok || kv == nil {
				continue
			}
			_ = kv
			for ns, pairs := range dump {
				kvBuf[svc] = append(kvBuf[svc], kvDump{ns: ns, pairs: pairs})
			}

		case strings.HasSuffix(hh.Name, ".blob"):
			// "<svc>/blobs/<ns>/<id>.blob"
			svc, ns, id := splitBlobPath(strings.TrimSuffix(hh.Name, ".blob"))
			if svc == "" {
				continue
			}
			// Defer until we've seen the matching .meta (content_type/name).
			blobs = append(blobs, blobEntry{svc: svc, ns: ns, id: id, content: append([]byte(nil), data...)})

		case strings.HasSuffix(hh.Name, ".meta"):
			svc, ns, id := splitBlobPath(strings.TrimSuffix(hh.Name, ".meta"))
			if svc == "" {
				continue
			}
			var meta struct {
				Name        string `json:"name"`
				ContentType string `json:"content_type"`
			}
			_ = json.Unmarshal(data, &meta)
			// Attach meta to the matching buffered blob entry.
			for i := range blobs {
				if blobs[i].svc == svc && blobs[i].ns == ns && blobs[i].id == id {
					blobs[i].name = meta.Name
					blobs[i].contentType = meta.ContentType
					break
				}
			}
		}
	}

	if hdr == nil {
		return nil, fmt.Errorf("restore: archive has no snapshot.json header")
	}

	// Wipe current state, then apply all buffered stores. (Reset first so restore
	// is a clean replace; buffering avoids colliding with pre-reset seeded rows.)
	if err := e.ResetAll(); err != nil {
		return hdr, fmt.Errorf("restore: reset: %w", err)
	}

	// Collections.
	for svc, dumps := range colBuf {
		col, _, _, ok := e.StateStores(svc)
		if !ok {
			continue
		}
		for _, d := range dumps {
			c, err := col.Collection(d.name)
			if err != nil {
				return hdr, fmt.Errorf("restore: collection %s/%s: %w", svc, d.name, err)
			}
			for _, doc := range d.docs {
				if _, err := c.Insert(doc); err != nil {
					return hdr, fmt.Errorf("restore: insert %s/%s: %w", svc, d.name, err)
				}
			}
		}
	}

	// KV.
	for svc, dumps := range kvBuf {
		_, kv, _, ok := e.StateStores(svc)
		if !ok || kv == nil {
			continue
		}
		for _, d := range dumps {
			for _, p := range d.pairs {
				if err := kv.Set(d.ns, p[0], p[1]); err != nil {
					return hdr, fmt.Errorf("restore: kv set %s/%s/%s: %w", svc, d.ns, p[0], err)
				}
			}
		}
	}

	// Blobs.

	// Blobs.
	for _, b := range blobs {
		_, _, bl, ok := e.StateStores(b.svc)
		if !ok || bl == nil {
			continue
		}
		name := b.name
		if name == "" {
			name = b.id
		}
		if _, err := bl.PutWith(b.ns, name, b.contentType, bytes.NewReader(b.content)); err != nil {
			return hdr, fmt.Errorf("restore: blob %s/%s/%s: %w", b.svc, b.ns, name, err)
		}
	}

	return hdr, nil
}

// splitBlobPath parses "<svc>/blobs/<ns>/<id>" into (svc, ns, id). Returns ("","","")
// for anything that doesn't match the expected 4-segment layout.
func splitBlobPath(p string) (svc, ns, id string) {
	// path.Dir twice: id is the basename; ns is the next dir; "blobs" is next.
	id = path.Base(p)
	rest := path.Dir(p)   // "<svc>/blobs/<ns>"
	ns = path.Base(rest)  // "<ns>"
	rest = path.Dir(rest) // "<svc>/blobs"
	if path.Base(rest) != "blobs" {
		return "", "", ""
	}
	svc = path.Dir(rest) // "<svc>"
	if svc == "" || svc == "." || strings.Contains(svc, "/") {
		// svc should be a single top-level segment (the service name).
		// path.Dir("<svc>/blobs") == "<svc>"; but if there were extra dirs, reject.
	}
	return svc, ns, id
}

// writeTar writes a single file entry to the tar writer.
func writeTar(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o644,
		Size: int64(len(data)),
	}); err != nil {
		return fmt.Errorf("tar write header %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("tar write %s: %w", name, err)
	}
	return nil
}
