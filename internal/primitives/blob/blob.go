// Package blob provides a filesystem-backed file-content store for services
// like Drive/Dropbox/S3. Each service namespace gets its own subdirectory
// under the configured root. Blob content and metadata are stored as
// sidecar files on disk.
package blob

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// ErrNotFound is returned by Get, Stat, and Delete (wrapped) when a blob
// does not exist. Delete itself is idempotent and returns nil for missing
// blobs; Get and Stat return ErrNotFound directly.
var ErrNotFound = errors.New("blob: not found")

// validName matches filesystem-safe identifiers: an alphanumeric character
// followed by alphanumeric, dots, underscores, or dashes. This prevents path
// traversal (no separators, no leading dots) while allowing common file names.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Info holds metadata about a stored blob.
type Info struct {
	Name        string
	Size        int64
	ContentType string
	Modified    time.Time
}

// Store is a filesystem-backed blob store. Each namespace maps to a
// subdirectory under root.
type Store struct {
	root string
}

// metadata is the JSON representation stored alongside blob content.
type metadata struct {
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type,omitempty"`
	Modified    time.Time `json:"modified"`
}

// Open opens or creates a blob store rooted at root. The root directory and
// namespace subdirectories are created lazily on first Put.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("blob: mkdir root %s: %w", root, err)
	}
	return &Store{root: root}, nil
}

// Close releases any resources held by the store. Currently a no-op but
// kept for future extensibility and symmetric API usage.
func (s *Store) Close() error { return nil }

// Put writes the content from r into namespace ns under the given name.
// The returned id is derived from name (sanitised-safe). Metadata (name,
// size, optional content type) is stored alongside the content.
func (s *Store) Put(ns, name string, r io.Reader) (string, error) {
	return s.PutWith(ns, name, "", r)
}

// PutWith is like Put but also records a content type.
func (s *Store) PutWith(ns, name, contentType string, r io.Reader) (string, error) {
	if err := validateName("namespace", ns); err != nil {
		return "", err
	}
	if err := validateName("name", name); err != nil {
		return "", err
	}

	nsDir := filepath.Join(s.root, ns)
	if err := os.MkdirAll(nsDir, 0o755); err != nil {
		return "", fmt.Errorf("blob: mkdir ns %s: %w", ns, err)
	}

	id := name
	contentPath := s.contentPath(ns, id)
	metaPath := s.metaPath(ns, id)

	// Write content to a temporary file first.
	contentTmp, err := os.CreateTemp(nsDir, ".tmp-content-*")
	if err != nil {
		return "", fmt.Errorf("blob: create temp: %w", err)
	}
	contentTmpPath := contentTmp.Name()

	size, err := io.Copy(contentTmp, r)
	if err != nil {
		contentTmp.Close()
		os.Remove(contentTmpPath)
		return "", fmt.Errorf("blob: write content: %w", err)
	}
	if err := contentTmp.Close(); err != nil {
		os.Remove(contentTmpPath)
		return "", fmt.Errorf("blob: close temp: %w", err)
	}

	// Prepare metadata in a temporary file too (atomic write).
	meta := metadata{
		Name:        name,
		Size:        size,
		ContentType: contentType,
		Modified:    time.Now().UTC(),
	}
	data, err := json.Marshal(meta)
	if err != nil {
		os.Remove(contentTmpPath)
		return "", fmt.Errorf("blob: marshal meta: %w", err)
	}
	metaTmp, err := os.CreateTemp(nsDir, ".tmp-meta-*")
	if err != nil {
		os.Remove(contentTmpPath)
		return "", fmt.Errorf("blob: create meta temp: %w", err)
	}
	metaTmpPath := metaTmp.Name()
	if _, err := metaTmp.Write(data); err != nil {
		metaTmp.Close()
		os.Remove(metaTmpPath)
		os.Remove(contentTmpPath)
		return "", fmt.Errorf("blob: write meta temp: %w", err)
	}
	if err := metaTmp.Close(); err != nil {
		os.Remove(metaTmpPath)
		os.Remove(contentTmpPath)
		return "", fmt.Errorf("blob: close meta temp: %w", err)
	}

	// Rename meta first, then content. This ordering guarantees that
	// if the content file is present, the metadata file is already present
	// too (so List/Stat never miss a blob whose content exists). If the
	// content rename fails after meta was renamed, we clean up the meta
	// to avoid leaving an orphan.
	if err := os.Rename(metaTmpPath, metaPath); err != nil {
		os.Remove(metaTmpPath)
		os.Remove(contentTmpPath)
		return "", fmt.Errorf("blob: rename meta: %w", err)
	}
	if err := os.Rename(contentTmpPath, contentPath); err != nil {
		// Meta was already renamed; roll it back so we don't leave an orphan.
		os.Remove(metaPath)
		os.Remove(contentTmpPath)
		return "", fmt.Errorf("blob: rename content: %w", err)
	}

	return id, nil
}

// Get opens the blob content for reading. The caller must close the
// returned ReadCloser. Returns ErrNotFound if the blob does not exist.
func (s *Store) Get(ns, id string) (io.ReadCloser, error) {
	if err := validateName("namespace", ns); err != nil {
		return nil, err
	}
	if err := validateName("id", id); err != nil {
		return nil, err
	}
	f, err := os.Open(s.contentPath(ns, id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("blob: get %s/%s: %w", ns, id, err)
	}
	return f, nil
}

// Stat returns metadata about a blob without reading its content.
func (s *Store) Stat(ns, id string) (Info, error) {
	if err := validateName("namespace", ns); err != nil {
		return Info{}, err
	}
	if err := validateName("id", id); err != nil {
		return Info{}, err
	}
	data, err := os.ReadFile(s.metaPath(ns, id))
	if err != nil {
		if os.IsNotExist(err) {
			return Info{}, ErrNotFound
		}
		return Info{}, fmt.Errorf("blob: stat %s/%s: %w", ns, id, err)
	}
	var meta metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return Info{}, fmt.Errorf("blob: unmarshal meta: %w", err)
	}
	return Info{
		Name:        meta.Name,
		Size:        meta.Size,
		ContentType: meta.ContentType,
		Modified:    meta.Modified,
	}, nil
}

// Delete removes a blob and its metadata. It is idempotent: deleting a
// non-existent blob returns nil.
func (s *Store) Delete(ns, id string) error {
	if err := validateName("namespace", ns); err != nil {
		return err
	}
	if err := validateName("id", id); err != nil {
		return err
	}
	// Remove content; ignore not-exist.
	if err := os.Remove(s.contentPath(ns, id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("blob: delete content %s/%s: %w", ns, id, err)
	}
	// Remove metadata; ignore not-exist.
	if err := os.Remove(s.metaPath(ns, id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("blob: delete meta %s/%s: %w", ns, id, err)
	}
	return nil
}

// List returns metadata for all blobs in the namespace.
func (s *Store) List(ns string) ([]Info, error) {
	if err := validateName("namespace", ns); err != nil {
		return nil, err
	}
	nsDir := filepath.Join(s.root, ns)
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // empty namespace
		}
		return nil, fmt.Errorf("blob: list %s: %w", ns, err)
	}

	var infos []Info
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() {
			continue
		}
		_, ok := stripSuffix(name, ".meta")
		if !ok {
			continue // skip content files and temp files
		}
		data, err := os.ReadFile(filepath.Join(nsDir, name))
		if err != nil {
			continue // skip unreadable meta
		}
		var meta metadata
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		infos = append(infos, Info{
			Name:        meta.Name,
			Size:        meta.Size,
			ContentType: meta.ContentType,
			Modified:    meta.Modified,
		})
	}
	return infos, nil
}

// --- helpers ---

func validateName(label, name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("blob: invalid %s %q: must match %s", label, name, validName.String())
	}
	return nil
}

func (s *Store) contentPath(ns, id string) string {
	return filepath.Join(s.root, ns, id+".content")
}

func (s *Store) metaPath(ns, id string) string {
	return filepath.Join(s.root, ns, id+".meta")
}

// stripSuffix returns the name without suffix and true if name ends with suffix.
func stripSuffix(name, suffix string) (string, bool) {
	if len(name) < len(suffix) || name[len(name)-len(suffix):] != suffix {
		return "", false
	}
	return name[:len(name)-len(suffix)], true
}
