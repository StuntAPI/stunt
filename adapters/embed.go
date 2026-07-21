// Package adapters embeds the bundled reference adapters into the stunt
// binary so that a `brew install stunt` / `go install stuntapi.com/stunt/cmd/stunt`
// user gets all reference adapters with zero cloning.
//
// At build time the entire adapters/ tree is embedded via go:embed. At
// runtime the adapter list is discovered by walking the embedded tree, and a
// specific adapter can be extracted to a real directory (adapter.Load needs a
// filesystem path because adapters open SQLite state stores).
//
// The embedded adapters also seed the catalog fallback index (see
// internal/catalog), so `stunt catalog search` works fully offline and lists
// all bundled adapters.
package adapters

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// allFS embeds the full adapters/ tree. Each top-level directory under it is
// one adapter (e.g. "stripe-style"). The non-adapter files at the adapters/
// root (embed.go, README.md, DISCLAIMER.template) are filtered out at
// discovery time by requiring an adapter.yaml inside each candidate dir.
//
//go:embed *
var allFS embed.FS

// has reports whether an embedded adapter directory named exists. It checks
// for the presence of an adapter.yaml file to distinguish real adapters from
// the package's own files (README.md, DISCLAIMER.template, this file).
func Has(name string) bool {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return false
	}
	info, err := fs.Stat(allFS, name+"/adapter.yaml")
	return err == nil && !info.IsDir()
}

// Names returns the sorted list of embedded adapter names (the 91 reference
// adapters). Directories without an adapter.yaml (package files) are excluded.
func Names() []string {
	entries, err := fs.ReadDir(allFS, ".")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if Has(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// Extract writes the embedded adapter named to dstDir (created if needed).
// The directory will contain the adapter's files with their relative layout
// preserved (adapter.yaml at the root of dstDir).
func Extract(name, dstDir string) error {
	if !Has(name) {
		return fmt.Errorf("adapters: %q is not an embedded adapter", name)
	}
	return copyEmbedDir(allFS, name, dstDir)
}

// AdapterMeta is the per-adapter metadata exposed by Index. It is kept in
// this package (not internal/catalog) to avoid an import cycle: catalog
// imports adapters, so adapters cannot import catalog's Entry type.
type AdapterMeta struct {
	Name        string // adapter directory name, e.g. "stripe-style"
	Description string // one-line description derived from the manifest
}

// Index builds the metadata for all embedded adapters by parsing each
// adapter.yaml. The result is memoized: the embedded tree never changes at
// runtime.
var (
	indexOnce  bool
	indexCache []AdapterMeta
)

func Index() []AdapterMeta {
	if indexOnce {
		return indexCache
	}
	indexOnce = true
	for _, name := range Names() {
		m, ok := metaFor(name)
		if !ok {
			continue
		}
		indexCache = append(indexCache, m)
	}
	return indexCache
}

// metaFor parses one embedded adapter.yaml into an AdapterMeta.
func metaFor(name string) (AdapterMeta, bool) {
	data, err := fs.ReadFile(allFS, name+"/adapter.yaml")
	if err != nil {
		return AdapterMeta{}, false
	}
	var meta struct {
		ID   string `yaml:"id"`
		Name string `yaml:"name"`
		API  *struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		} `yaml:"api"`
	}
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return AdapterMeta{}, false
	}
	desc := name
	if meta.API != nil && meta.API.Name != "" {
		desc = meta.API.Name
		if meta.API.Version != "" {
			desc += " " + meta.API.Version
		}
	}
	return AdapterMeta{
		Name:        name,
		Description: desc + " — local simulator (embedded)",
	}, true
}

// copyEmbedDir recursively copies all files under srcRoot in the embed.FS
// to dstDir on disk. (Lifted from internal/cli/demo.go so the embed package
// is self-contained.)
func copyEmbedDir(fsys embed.FS, srcRoot, dstDir string) error {
	return fs.WalkDir(fsys, srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fsys.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		return nil
	})
}
