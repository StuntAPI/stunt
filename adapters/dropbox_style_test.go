package adapters

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// Drives the dropbox-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock. The API is RPC-style: all eight
// routes are POST /2/{files|users}/{action} with the arguments in the JSON
// body (no path params, no query strings), and every handler here is called
// exactly that way. Covered: both upload request shapes (the documented
// JSON {path, content} convenience form and the real Dropbox-API-Arg +
// raw-body form), the Dropbox content_hash scheme under the virtual clock,
// the path_prefix subtree listing with body-cursor paging, raw octet-stream
// downloads by path and id, the 409 path/* and 401 access-token error
// envelopes, and the permanent cascading folder delete audited through the
// internal trash tombstones.

const (
	dropboxHost  = "api.dropbox.test"
	dropboxToken = "sl.test_token_mock" // README's seeded static mock token
)

type dropboxFixture struct {
	t     *testing.T
	vc    *clock.Clock
	vms   map[string]*starlark.VM
	store *primitives.Store
	kv    *kv.KV
	host  string
}

func newDropboxFixture(t *testing.T, start time.Time) *dropboxFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "dropbox-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	tmp := t.TempDir()
	store, err := primitives.Open(filepath.Join(tmp, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	kvStore, err := kv.Open(filepath.Join(tmp, "s.kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kvStore.Close() })
	blobStore, err := blob.Open(filepath.Join(tmp, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: events.NewEmitter(),
	})

	// Seed the entries collection like the engine does on boot (the one
	// synthetic seed folder the adapter ships).
	entries, err := store.Collection("entries")
	if err != nil {
		t.Fatalf("entries collection: %v", err)
	}
	if err := entries.Seed(filepath.Join(root, "fixtures", "entries.jsonl")); err != nil {
		t.Fatalf("seed entries: %v", err)
	}
	if _, err := store.Collection("trash"); err != nil {
		t.Fatalf("trash collection: %v", err)
	}

	load := func(script string) *starlark.VM {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(root, "scripts", script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
		if err != nil {
			t.Fatalf("LoadWithLib %s: %v", script, err)
		}
		return vm
	}
	return &dropboxFixture{
		t: t, vc: vc, store: store, kv: kvStore, host: dropboxHost,
		vms: map[string]*starlark.VM{"files": load("files.star"), "users": load("users.star")},
	}
}

// call invokes one of the eight POST /2/... RPC handlers. Every argument an
// RPC handler reads lives in the body (round-tripped through JSON so numbers
// arrive as floats, the wire shape); query is only ever ignored. auth is the
// exact Authorization header value ("" omits the header).
func (f *dropboxFixture) call(group, handler, route string, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	var wire map[string]any
	if body != nil {
		wire = wireBody(f.t, body)
	}
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: "POST", Path: route, Host: f.host, Headers: headers,
		Body: wire, Params: map[string]string{}, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, route, err)
	}
	return resp
}

// uploadRaw drives on_upload in the real request shape: the file arguments
// in the Dropbox-API-Arg header (a JSON string) and the raw file bytes as
// the octet-stream request body. apiArg is used verbatim, so malformed
// values exercise the never-raise path.
func (f *dropboxFixture) uploadRaw(apiArg, raw, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{
		"Content-Type":    "application/octet-stream",
		"Dropbox-API-Arg": apiArg,
	}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms["files"].Call("on_upload", starlark.Request{
		Method: "POST", Path: "/2/files/upload", Host: f.host, Headers: headers,
		Params: map[string]string{}, RawBody: raw,
	})
	if err != nil {
		f.t.Fatalf("on_upload raw: %v", err)
	}
	return resp
}

// dropboxNum coerces a response number to float64: freshly built docs carry
// int64 while collection round-trips (get_metadata, listings) come back as
// JSON floats.
func dropboxNum(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	default:
		t.Fatalf("value %v (%T) is not a number", v, v)
		return 0
	}
}

// dropboxPaths returns the path_lower of every entry in a list_folder
// response, in server order.
func dropboxPaths(t *testing.T, r starlark.Response) []string {
	t.Helper()
	entries, ok := r.Body["entries"].([]any)
	if !ok {
		t.Fatalf("entries = %v (%T), want list", r.Body["entries"], r.Body["entries"])
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		p, _ := e.(map[string]any)["path_lower"].(string)
		out = append(out, p)
	}
	return out
}

// dropboxWantErr asserts the simplified Dropbox error envelope: an
// error_summary of "<tag>/.." and a nested error .tag of exactly "<tag>".
func dropboxWantErr(t *testing.T, r starlark.Response, status int, tag string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status = %d, want %d (body %v)", r.Status, status, r.Body)
	}
	if r.Body["error_summary"] != tag+"/.." {
		t.Fatalf("error_summary = %v, want %q", r.Body["error_summary"], tag+"/..")
	}
	errObj, ok := r.Body["error"].(map[string]any)
	if !ok || errObj[".tag"] != tag {
		t.Fatalf("error = %v, want nested {\".tag\": %q}", r.Body["error"], tag)
	}
}

// dropboxStyleContentHash mirrors the adapter's content hash: Dropbox's own
// scheme of SHA-256 over the concatenated per-4MiB-block SHA-256 digests.
func dropboxStyleContentHash(b []byte) string {
	const block = 4 << 20
	h := sha256.New()
	for i := 0; i == 0 || i < len(b); i += block {
		end := i + block
		if end > len(b) {
			end = len(b)
		}
		d := sha256.Sum256(b[i:end])
		h.Write(d[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestDropboxUploadForms: the documented JSON {path, content} convenience
// deviation, the real Dropbox-API-Arg + raw-body upload form, and the
// mode:"add" conflict an existing path now answers with.
func TestDropboxUploadForms(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newDropboxFixture(t, base)

	// ===== upload takes the JSON {path, content} convenience body (documented deviation) =====
	// Real /2/files/upload posts raw bytes with the arguments in the
	// Dropbox-API-Arg header; the mock also accepts this JSON form (see
	// conformance/matrix.yaml) — asserted here as-is.
	content := "dropbox-style vm suite notes"
	up := f.call("files", "on_upload", "/2/files/upload", nil, map[string]any{
		"path":            "/VM Suite/notes.txt",
		"content":         content,
		"client_modified": "2020-02-03T04:05:06Z",
	}, "")
	if up.Status != 200 {
		t.Fatalf("JSON-form upload -> %d: %v", up.Status, up.Body)
	}
	if up.Body[".tag"] != "file" || up.Body["id"] != "id_1" {
		t.Fatalf("upload doc = %v, want .tag file / id_1", up.Body)
	}
	if up.Body["name"] != "notes.txt" || up.Body["path_display"] != "/VM Suite/notes.txt" ||
		up.Body["path_lower"] != "/vm suite/notes.txt" {
		t.Fatalf("upload paths = %v", up.Body)
	}
	if got := dropboxNum(t, up.Body["size"]); got != float64(len(content)) {
		t.Fatalf("size = %v, want %d", up.Body["size"], len(content))
	}
	if up.Body["client_modified"] != "2020-02-03T04:05:06Z" {
		t.Fatalf("client_modified = %v, want the client-declared value echoed", up.Body["client_modified"])
	}
	if up.Body["server_modified"] != base.Format(time.RFC3339) {
		t.Fatalf("server_modified = %v, want the live (virtual) clock %s",
			up.Body["server_modified"], base.Format(time.RFC3339))
	}
	if up.Body["content_hash"] != dropboxStyleContentHash([]byte(content)) {
		t.Fatalf("content_hash = %v, want the Go-computed Dropbox-scheme hash %s",
			up.Body["content_hash"], dropboxStyleContentHash([]byte(content)))
	}

	// ===== the real RPC upload (Dropbox-API-Arg header + raw octet-stream body) lands identically =====
	raw := "raw-binary-payload"
	argUp := f.uploadRaw(`{"path": "/VM Suite/raw.bin", "client_modified": "2021-06-07T08:09:10Z"}`, raw, "")
	if argUp.Status != 200 {
		t.Fatalf("RPC-form upload -> %d: %v", argUp.Status, argUp.Body)
	}
	if argUp.Body["id"] != "id_2" || argUp.Body["name"] != "raw.bin" {
		t.Fatalf("RPC-form upload doc = %v, want id_2 / raw.bin", argUp.Body)
	}
	if got := dropboxNum(t, argUp.Body["size"]); got != float64(len(raw)) {
		t.Fatalf("RPC-form size = %v, want %d", argUp.Body["size"], len(raw))
	}
	if argUp.Body["content_hash"] != dropboxStyleContentHash([]byte(raw)) {
		t.Fatalf("RPC-form content_hash = %v, want %s", argUp.Body["content_hash"], dropboxStyleContentHash([]byte(raw)))
	}
	// The stored bytes download back verbatim under the octet-stream type.
	dl := f.call("files", "on_download", "/2/files/download", nil, map[string]any{"path": "/VM Suite/raw.bin"}, "")
	if dl.Status != 200 || dl.RawBody != raw {
		t.Fatalf("download after RPC-form upload -> %d %q, want the raw bytes", dl.Status, dl.RawBody)
	}
	if ct := dl.Headers["Content-Type"]; ct != "application/octet-stream" {
		t.Fatalf("download Content-Type = %q, want application/octet-stream", ct)
	}
	// A malformed Dropbox-API-Arg is untrusted header input: it must never
	// raise, and with no path anywhere the flat path error answers.
	badArg := f.uploadRaw("{not-json", "ignored-bytes", "")
	dropboxWantErr(t, badArg, 409, "path")

	// ===== re-uploading an existing path answers the real mode:"add" conflict =====
	// Real upload defaults to mode "add" + autorename:false: an existing
	// path is a conflict. The mock used to insert a second row at the same
	// path, leaving the older row ahead of every later path read.
	dup := f.call("files", "on_upload", "/2/files/upload", nil, map[string]any{
		"path": "/VM Suite/notes.txt", "content": "different bytes entirely",
	}, "")
	dropboxWantErr(t, dup, 409, "path/conflict")
	// Dropbox paths are case-insensitive (path_lower is the canonical key),
	// so a differently-cased path is the same conflict.
	ci := f.call("files", "on_upload", "/2/files/upload", nil, map[string]any{
		"path": "/VM SUITE/NOTES.TXT", "content": "x",
	}, "")
	dropboxWantErr(t, ci, 409, "path/conflict")
	// The original entry is untouched: same id, and exactly one notes row.
	meta := f.call("files", "on_get_metadata", "/2/files/get_metadata", nil, map[string]any{"path": "/VM Suite/notes.txt"}, "")
	if meta.Status != 200 || meta.Body["id"] != "id_1" {
		t.Fatalf("metadata after conflicting upload -> %d %v, want the original id_1", meta.Status, meta.Body)
	}
	root := f.call("files", "on_list_folder", "/2/files/list_folder", nil, map[string]any{"path": ""}, "")
	notes := 0
	for _, p := range dropboxPaths(t, root) {
		if p == "/vm suite/notes.txt" {
			notes++
		}
	}
	if notes != 1 || len(dropboxPaths(t, root)) != 3 { // seed folder + notes + raw.bin
		t.Fatalf("root after conflicting upload = %v, want exactly one notes row of 3", dropboxPaths(t, root))
	}

	// ===== mode overwrite replaces in place; autorename forks a suffixed path =====
	// The default conflict is mode-specific: SDK clients re-upload via
	// WriteMode.overwrite, and autorename is the escape hatch for adds.
	over := f.call("files", "on_upload", "/2/files/upload", nil, map[string]any{
		"path": "/VM Suite/notes.txt", "content": "replaced", "mode": map[string]any{".tag": "overwrite"},
	}, "")
	if over.Status != 200 || over.Body["id"] != "id_1" {
		t.Fatalf("overwrite upload -> %d %v, want 200 replacing id_1 in place", over.Status, over.Body)
	}
	if got := f.call("files", "on_download", "/2/files/download", nil, map[string]any{"path": "/VM Suite/notes.txt"}, ""); got.Status != 200 || got.RawBody != "replaced" {
		t.Fatalf("download after overwrite -> %d %q, want the replaced bytes", got.Status, got.RawBody)
	}
	auto := f.call("files", "on_upload", "/2/files/upload", nil, map[string]any{
		"path": "/VM Suite/notes.txt", "content": "forked", "autorename": true,
	}, "")
	if auto.Status != 200 || auto.Body["path_display"] != "/VM Suite/notes (1).txt" {
		t.Fatalf("autorename upload -> %d %v, want the (1)-suffixed path", auto.Status, auto.Body)
	}
}

// TestDropboxListFolderSubtreeAndPaging: the whole-subtree listing deviation,
// the root/missing/file-path listing errors, and body-cursor paging applied
// after the path filter.
func TestDropboxListFolderSubtreeAndPaging(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newDropboxFixture(t, base)

	must200 := func(handler, route string, body map[string]any) starlark.Response {
		t.Helper()
		r := f.call("files", handler, route, nil, body, "")
		if r.Status != 200 {
			t.Fatalf("%s %v -> %d: %v", handler, body, r.Status, r.Body)
		}
		return r
	}
	must200("on_create_folder", "/2/files/create_folder", map[string]any{"path": "/Projects"})         // id_1
	must200("on_create_folder", "/2/files/create_folder", map[string]any{"path": "/Projects/Archive"}) // id_2
	must200("on_upload", "/2/files/upload", map[string]any{"path": "/Projects/plan.txt", "content": "cascade plan"})
	must200("on_upload", "/2/files/upload", map[string]any{"path": "/Projects/Archive/2019.txt", "content": "old stuff"})
	must200("on_upload", "/2/files/upload", map[string]any{"path": "/root.txt", "content": "root level"})

	// ===== list_folder returns the whole path-prefix subtree, not one level =====
	// Real list_folder returns only the direct children; the mock returns
	// the entire subtree at once (documented deviation) — including the
	// folder itself and depth-2 entries.
	sub := must200("on_list_folder", "/2/files/list_folder", map[string]any{"path": "/Projects"})
	got := dropboxPaths(t, sub)
	want := []string{"/projects", "/projects/archive", "/projects/plan.txt", "/projects/archive/2019.txt"}
	if len(got) != len(want) {
		t.Fatalf("subtree = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("subtree[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	if sub.Body["has_more"] != false || sub.Body["cursor"] != "" {
		t.Fatalf("unpaged envelope = %v %v, want has_more false / empty cursor", sub.Body["has_more"], sub.Body["cursor"])
	}
	// The path lookup is case-insensitive, like path_lower itself.
	subCI := must200("on_list_folder", "/2/files/list_folder", map[string]any{"path": "/projects"})
	if p := dropboxPaths(t, subCI); len(p) != 4 || p[3] != "/projects/archive/2019.txt" {
		t.Fatalf("case-insensitive subtree = %v, want the same 4 entries", p)
	}

	// ===== the root listing spans everything; unknown and file paths carry distinct 409 tags =====
	root := must200("on_list_folder", "/2/files/list_folder", map[string]any{"path": ""})
	if p := dropboxPaths(t, root); len(p) != 6 { // seed + 5 created
		t.Fatalf("root listing = %v, want 6 entries", p)
	}
	slash := must200("on_list_folder", "/2/files/list_folder", map[string]any{"path": "/"})
	if p := dropboxPaths(t, slash); len(p) != 6 {
		t.Fatalf("\"/\" listing = %v, want the same 6 entries as \"\"", p)
	}
	missing := f.call("files", "on_list_folder", "/2/files/list_folder", nil, map[string]any{"path": "/nope"}, "")
	dropboxWantErr(t, missing, 409, "path/not_found")
	fileArg := f.call("files", "on_list_folder", "/2/files/list_folder", nil, map[string]any{"path": "/root.txt"}, "")
	dropboxWantErr(t, fileArg, 409, "path/not_folder")

	// ===== paging slices the filtered subtree by body cursor, ignoring query strings =====
	// The /2/ RPC style reads its arguments from the JSON body: a REST-style
	// ?limit=1 query is not even looked at.
	qOnly := f.call("files", "on_list_folder", "/2/files/list_folder",
		map[string]string{"limit": "1"}, map[string]any{"path": "/Projects"}, "")
	if p := dropboxPaths(t, qOnly); len(p) != 4 {
		t.Fatalf("query-string limit was honored (%v) — paging must read the body", p)
	}
	p1 := must200("on_list_folder", "/2/files/list_folder", map[string]any{"path": "/Projects", "limit": 2})
	if p := dropboxPaths(t, p1); len(p) != 2 || p[0] != "/projects" || p[1] != "/projects/archive" {
		t.Fatalf("page 1 = %v, want the first two subtree entries", p)
	}
	if p1.Body["has_more"] != true || p1.Body["cursor"] != "2" {
		t.Fatalf("page 1 envelope = %v %v, want has_more true / cursor \"2\"", p1.Body["has_more"], p1.Body["cursor"])
	}
	p2 := must200("on_list_folder", "/2/files/list_folder",
		map[string]any{"path": "/Projects", "limit": 2, "cursor": "2"})
	if p := dropboxPaths(t, p2); len(p) != 2 || p[0] != "/projects/plan.txt" || p[1] != "/projects/archive/2019.txt" {
		t.Fatalf("page 2 = %v, want the last two subtree entries", p)
	}
	if p2.Body["has_more"] != false || p2.Body["cursor"] != "" {
		t.Fatalf("final page envelope = %v %v, want has_more false / empty cursor", p2.Body["has_more"], p2.Body["cursor"])
	}
	// The two pages cover the subtree exactly once.
	seen := map[string]int{}
	for _, p := range append(dropboxPaths(t, p1), dropboxPaths(t, p2)...) {
		seen[p]++
	}
	if len(seen) != 4 {
		t.Fatalf("paged union = %v, want the 4 distinct subtree paths", seen)
	}
	for _, n := range seen {
		if n != 1 {
			t.Fatalf("paged union repeats a path: %v", seen)
		}
	}
	// A garbage cursor gets the real 400 invalid_cursor envelope (same
	// summary+tag shape as the 409s).
	badCursor := f.call("files", "on_list_folder", "/2/files/list_folder", nil,
		map[string]any{"path": "/Projects", "limit": 2, "cursor": "abc"}, "")
	dropboxWantErr(t, badCursor, 400, "invalid_cursor")
}

// TestDropboxReadsAndErrorEnvelopes: octet-stream downloads by path and id,
// the 409 path envelope across declined reads, and the synthetic temp link.
func TestDropboxReadsAndErrorEnvelopes(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newDropboxFixture(t, base)

	content := "quarterly numbers"
	up := f.call("files", "on_upload", "/2/files/upload", nil, map[string]any{
		"path": "/reports/q3.txt", "content": content,
	}, "")
	if up.Status != 200 {
		t.Fatalf("upload -> %d: %v", up.Status, up.Body)
	}
	fileID, _ := up.Body["id"].(string)

	// ===== download streams raw bytes by path and by id, with metadata alongside =====
	byPath := f.call("files", "on_download", "/2/files/download", nil, map[string]any{"path": "/reports/q3.txt"}, "")
	if byPath.Status != 200 || byPath.RawBody != content {
		t.Fatalf("download by path -> %d %q, want the raw content", byPath.Status, byPath.RawBody)
	}
	if ct := byPath.Headers["Content-Type"]; ct != "application/octet-stream" {
		t.Fatalf("download Content-Type = %q, want application/octet-stream (no JSON envelope)", ct)
	}
	byID := f.call("files", "on_download", "/2/files/download", nil, map[string]any{"id": fileID}, "")
	if byID.Status != 200 || byID.RawBody != content {
		t.Fatalf("download by id -> %d %q, want the same raw content", byID.Status, byID.RawBody)
	}
	// Metadata persists through the collection with the upload's fields.
	meta := f.call("files", "on_get_metadata", "/2/files/get_metadata", nil, map[string]any{"path": "/reports/q3.txt"}, "")
	if meta.Status != 200 || meta.Body["id"] != fileID || meta.Body["name"] != "q3.txt" {
		t.Fatalf("get_metadata -> %d %v, want the persisted file doc", meta.Status, meta.Body)
	}
	if got := dropboxNum(t, meta.Body["size"]); got != float64(len(content)) {
		t.Fatalf("persisted size = %v, want %d", meta.Body["size"], len(content))
	}
	if meta.Body["content_hash"] != dropboxStyleContentHash([]byte(content)) {
		t.Fatalf("persisted content_hash = %v", meta.Body["content_hash"])
	}

	// ===== folders and unknown paths decline under the 409 path envelope =====
	folder := f.call("files", "on_create_folder", "/2/files/create_folder", nil, map[string]any{"path": "/Dossier"}, "")
	if folder.Status != 200 || folder.Body[".tag"] != "folder" {
		t.Fatalf("create folder -> %d %v", folder.Status, folder.Body)
	}
	dlFolder := f.call("files", "on_download", "/2/files/download", nil, map[string]any{"path": "/Dossier"}, "")
	dropboxWantErr(t, dlFolder, 409, "path/disallowed")
	dlMissing := f.call("files", "on_download", "/2/files/download", nil, map[string]any{"path": "/gone.txt"}, "")
	dropboxWantErr(t, dlMissing, 409, "path/not_found")
	metaMissing := f.call("files", "on_get_metadata", "/2/files/get_metadata", nil, map[string]any{"path": "/gone.txt"}, "")
	dropboxWantErr(t, metaMissing, 409, "path/not_found")
	// A request with no path at all gets the bare "path" tag, not a crash.
	noPath := f.call("files", "on_upload", "/2/files/upload", nil, map[string]any{}, "")
	dropboxWantErr(t, noPath, 409, "path")
	noFolderPath := f.call("files", "on_create_folder", "/2/files/create_folder", nil, map[string]any{}, "")
	dropboxWantErr(t, noFolderPath, 409, "path")
	delMissing := f.call("files", "on_delete", "/2/files/delete", nil, map[string]any{"path": "/gone.txt"}, "")
	dropboxWantErr(t, delMissing, 409, "path/not_found")

	// ===== get_temporary_link pairs the file's metadata with the synthetic link =====
	link := f.call("files", "on_get_temporary_link", "/2/files/get_temporary_link", nil, map[string]any{"path": "/reports/q3.txt"}, "")
	if link.Status != 200 {
		t.Fatalf("get_temporary_link -> %d: %v", link.Status, link.Body)
	}
	// The URL is synthetic and does not serve the content (documented
	// deviation) — asserted verbatim.
	if link.Body["link"] != "https://dl.dropboxusercontent.com/synthetic-temporary-link" {
		t.Fatalf("link = %v, want the synthetic URL", link.Body["link"])
	}
	if md, ok := link.Body["metadata"].(map[string]any); !ok || md["id"] != fileID {
		t.Fatalf("link metadata = %v, want the full file doc for %s", link.Body["metadata"], fileID)
	}
	linkFolder := f.call("files", "on_get_temporary_link", "/2/files/get_temporary_link", nil, map[string]any{"path": "/Dossier"}, "")
	dropboxWantErr(t, linkFolder, 409, "path/disallowed")
	linkMissing := f.call("files", "on_get_temporary_link", "/2/files/get_temporary_link", nil, map[string]any{"path": "/gone.txt"}, "")
	dropboxWantErr(t, linkMissing, 409, "path/not_found")
}

// TestDropboxAuthGate: the bearer validation the adapter applies only when a
// token is presented, and the documented no-header openness.
func TestDropboxAuthGate(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newDropboxFixture(t, base)

	// ===== a presented bearer must be registered: unknown and expired tokens get distinct 401 tags =====
	// An unknown token is invalid on every surface, in Dropbox's envelope.
	unknown := f.call("files", "on_upload", "/2/files/upload", nil, map[string]any{
		"path": "/x.txt", "content": "x",
	}, "Bearer sl.not_a_real_token")
	dropboxWantErr(t, unknown, 401, "invalid_access_token")
	unknownAcct := f.call("users", "on_get_current_account", "/2/users/get_current_account", nil,
		map[string]any{}, "Bearer sl.not_a_real_token")
	dropboxWantErr(t, unknownAcct, 401, "invalid_access_token")
	// A registered but past-expiry token flips to the expired tag. Tokens
	// live in the KV store as token_<tok> -> unix-seconds expiry.
	if err := f.kv.Set("dropbox", "token_sl.stale_token", "1000"); err != nil {
		t.Fatalf("seed stale token: %v", err)
	}
	expired := f.call("files", "on_upload", "/2/files/upload", nil, map[string]any{
		"path": "/x.txt", "content": "x",
	}, "Bearer sl.stale_token")
	dropboxWantErr(t, expired, 401, "expired_access_token")
	// The README's static mock token is seeded on first use and passes.
	static := f.call("files", "on_upload", "/2/files/upload", nil, map[string]any{
		"path": "/authed.txt", "content": "authed",
	}, "Bearer "+dropboxToken)
	if static.Status != 200 {
		t.Fatalf("static mock token -> %d: %v", static.Status, static.Body)
	}

	// ===== an absent Authorization header stays open (documented deviation) =====
	// Real Dropbox requires auth on every route; the mock accepts requests
	// with no Authorization header at all (documented deviation, kept for
	// the shared engine test helpers).
	open := f.call("users", "on_get_current_account", "/2/users/get_current_account", nil, map[string]any{}, "")
	if open.Status != 200 || open.Body["account_id"] == nil {
		t.Fatalf("no-header account -> %d %v, want 200", open.Status, open.Body)
	}
	openUp := f.call("files", "on_upload", "/2/files/upload", nil, map[string]any{
		"path": "/open.txt", "content": "no auth header",
	}, "")
	if openUp.Status != 200 {
		t.Fatalf("no-header upload -> %d: %v", openUp.Status, openUp.Body)
	}
}

// TestDropboxFolderAndAccount: folder creation with the case-insensitive
// conflict, and the synthetic /2/users account snapshot.
func TestDropboxFolderAndAccount(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newDropboxFixture(t, base)

	// ===== create_folder mints folder metadata and conflicts case-insensitively =====
	cf := f.call("files", "on_create_folder", "/2/files/create_folder", nil, map[string]any{"path": "/Reports"}, "")
	if cf.Status != 200 {
		t.Fatalf("create_folder -> %d: %v", cf.Status, cf.Body)
	}
	if cf.Body[".tag"] != "folder" || cf.Body["id"] != "id_1" || cf.Body["name"] != "Reports" {
		t.Fatalf("folder doc = %v, want .tag folder / id_1 / Reports", cf.Body)
	}
	if cf.Body["path_lower"] != "/reports" || cf.Body["path_display"] != "/Reports" {
		t.Fatalf("folder paths = %v", cf.Body)
	}
	if cf.Body["server_modified"] != base.Format(time.RFC3339) {
		t.Fatalf("folder server_modified = %v, want the clock's %s",
			cf.Body["server_modified"], base.Format(time.RFC3339))
	}
	// Folders carry no file fields.
	for _, k := range []string{"size", "content_hash", "client_modified"} {
		if _, has := cf.Body[k]; has {
			t.Fatalf("folder doc carries file field %q: %v", k, cf.Body)
		}
	}
	dup := f.call("files", "on_create_folder", "/2/files/create_folder", nil, map[string]any{"path": "/Reports"}, "")
	dropboxWantErr(t, dup, 409, "path/conflict")
	// The case-insensitive namespace: /REPORTS is the same folder.
	ci := f.call("files", "on_create_folder", "/2/files/create_folder", nil, map[string]any{"path": "/REPORTS"}, "")
	dropboxWantErr(t, ci, 409, "path/conflict")

	// ===== get_current_account returns the synthetic /2/users snapshot =====
	acct := f.call("users", "on_get_current_account", "/2/users/get_current_account", nil, map[string]any{}, "")
	if acct.Status != 200 {
		t.Fatalf("get_current_account -> %d: %v", acct.Status, acct.Body)
	}
	if acct.Body["account_id"] != "dbid:synthetic-local-test-account" ||
		acct.Body["email"] != "test-user@example.local" ||
		acct.Body["email_verified"] != true || acct.Body["country"] != "US" || acct.Body["locale"] != "en" {
		t.Fatalf("account = %v, want the synthetic snapshot", acct.Body)
	}
	name, ok := acct.Body["name"].(map[string]any)
	if !ok || name["given_name"] != "Local" || name["surname"] != "Test User" ||
		name["display_name"] != "Local Test User" || name["abbreviated_name"] != "LT" {
		t.Fatalf("account name = %v, want all five name fields", acct.Body["name"])
	}
}

// TestDropboxCascadeDeleteAndTrash: the permanent, cascading folder delete —
// visible removal from every read path, the trash tombstone audit trail, and
// the no-restore freshness of re-created paths.
func TestDropboxCascadeDeleteAndTrash(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newDropboxFixture(t, base)

	must200 := func(handler, route string, body map[string]any) map[string]any {
		t.Helper()
		r := f.call("files", handler, route, nil, body, "")
		if r.Status != 200 {
			t.Fatalf("%s %v -> %d: %v", handler, body, r.Status, r.Body)
		}
		return r.Body
	}
	proj := must200("on_create_folder", "/2/files/create_folder", map[string]any{"path": "/Projects"})            // id_1
	archive := must200("on_create_folder", "/2/files/create_folder", map[string]any{"path": "/Projects/Archive"}) // id_2
	plan := must200("on_upload", "/2/files/upload", map[string]any{"path": "/Projects/plan.txt", "content": "cascade plan"})
	old := must200("on_upload", "/2/files/upload", map[string]any{"path": "/Projects/Archive/2019.txt", "content": "old stuff"})
	keep := must200("on_upload", "/2/files/upload", map[string]any{"path": "/keep-me.txt", "content": "survivor"})

	// The delete lands at a known later clock time so the tombstone stamps
	// are assertable.
	f.vc.Advance(2 * time.Hour)
	delAt := base.Add(2 * time.Hour).Format(time.RFC3339)

	// ===== deleting a folder removes its entire subtree from every read path =====
	del := f.call("files", "on_delete", "/2/files/delete", nil, map[string]any{"path": "/Projects"}, "")
	if del.Status != 200 || del.Body["id"] != proj["id"] || del.Body[".tag"] != "folder" {
		t.Fatalf("delete -> %d %v, want 200 echoing the folder doc", del.Status, del.Body)
	}
	for _, path := range []string{
		"/Projects", "/Projects/Archive", "/Projects/plan.txt", "/Projects/Archive/2019.txt",
	} {
		r := f.call("files", "on_get_metadata", "/2/files/get_metadata", nil, map[string]any{"path": path}, "")
		dropboxWantErr(t, r, 409, "path/not_found")
	}
	for _, id := range []any{plan["id"], old["id"]} {
		r := f.call("files", "on_download", "/2/files/download", nil, map[string]any{"id": id.(string)}, "")
		dropboxWantErr(t, r, 409, "path/not_found")
	}
	// The deleted folder itself cannot be listed anymore.
	listGone := f.call("files", "on_list_folder", "/2/files/list_folder", nil, map[string]any{"path": "/Projects"}, "")
	dropboxWantErr(t, listGone, 409, "path/not_found")
	// The root keeps the seed folder and the sibling — nothing else leaks.
	root := f.call("files", "on_list_folder", "/2/files/list_folder", nil, map[string]any{"path": ""}, "")
	if p := dropboxPaths(t, root); len(p) != 2 || p[0] != "/seed folder" || p[1] != "/keep-me.txt" {
		t.Fatalf("root after cascade = %v, want only the seed folder and /keep-me.txt", p)
	}
	_ = archive

	// ===== trash tombstones audit the exact cascade batch =====
	// Every removed row lands in the internal trash collection exactly once,
	// stamped with the delete time and the batch root that took it out.
	trashCol, err := f.store.Collection("trash")
	if err != nil {
		t.Fatalf("trash collection: %v", err)
	}
	tombs, err := trashCol.List()
	if err != nil {
		t.Fatalf("trash list: %v", err)
	}
	if len(tombs) != 4 {
		t.Fatalf("trash has %d tombstones, want the 4 cascaded entries", len(tombs))
	}
	wantIDs := map[string]bool{
		proj["id"].(string): true, archive["id"].(string): true,
		plan["id"].(string): true, old["id"].(string): true,
	}
	for _, tomb := range tombs {
		id, _ := tomb["id"].(string)
		if !wantIDs[id] {
			t.Fatalf("tombstone for unexpected id %q (want %v)", id, wantIDs)
		}
		delete(wantIDs, id)
		if tomb["_deleted_at"] != delAt {
			t.Fatalf("tombstone %s _deleted_at = %v, want %s", id, tomb["_deleted_at"], delAt)
		}
		if tomb["_batch_root"] != proj["id"] {
			t.Fatalf("tombstone %s _batch_root = %v, want the deleted root %v",
				id, tomb["_batch_root"], proj["id"])
		}
	}
	if len(wantIDs) != 0 {
		t.Fatalf("cascade missed entries: %v", wantIDs)
	}
	// The entries collection itself holds only the seed folder and the
	// survivor — no orphans left under the deleted parent.
	entriesCol, err := f.store.Collection("entries")
	if err != nil {
		t.Fatalf("entries collection: %v", err)
	}
	n, err := entriesCol.Count()
	if err != nil {
		t.Fatalf("entries count: %v", err)
	}
	if n != 2 {
		t.Fatalf("entries count after cascade = %d, want 2 (seed + /keep-me.txt)", n)
	}
	_ = keep

	// ===== delete is permanent: a re-created path is a brand-new entry =====
	fresh := f.call("files", "on_upload", "/2/files/upload", nil, map[string]any{
		"path": "/Projects/plan.txt", "content": "new plan",
	}, "")
	if fresh.Status != 200 {
		t.Fatalf("re-upload at deleted path -> %d: %v", fresh.Status, fresh.Body)
	}
	if fresh.Body["id"] == plan["id"] || fresh.Body["content_hash"] == plan["content_hash"] {
		t.Fatalf("re-upload reused the deleted entry: %v", fresh.Body)
	}
	// A single-file delete still works standalone, and stays deleted.
	single := f.call("files", "on_delete", "/2/files/delete", nil, map[string]any{"path": "/keep-me.txt"}, "")
	if single.Status != 200 || single.Body["id"] != keep["id"] {
		t.Fatalf("single delete -> %d %v", single.Status, single.Body)
	}
	after := f.call("files", "on_get_metadata", "/2/files/get_metadata", nil, map[string]any{"path": "/keep-me.txt"}, "")
	dropboxWantErr(t, after, 409, "path/not_found")
}
