package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.starlark.net/syntax"
)

// TestQCAllAdapterScriptsParse parses every reference adapter's Starlark
// scripts. This complements TestQCBootAllReferenceAdapters, which validates
// manifests and handler references but parses handler scripts lazily at request
// time. That laziness means a Starlark syntax error (e.g. a Python-ism like
// try/except or an f-string) survives boot and surfaces only as a 500 when a
// handler is first invoked. This guard catches such errors upfront. It is a
// permanent regression guard — adapters must parse.
func TestQCAllAdapterScriptsParse(t *testing.T) {
	adaptersDir := repoAdaptersDir(t)
	if adaptersDir == "" {
		t.Skip("adapters/ directory not found — skipping adapter script parse QC")
	}
	entries, err := os.ReadDir(adaptersDir)
	if err != nil {
		t.Skipf("cannot read adapters dir %s: %v", adaptersDir, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), "-style") {
			dirs = append(dirs, e.Name())
		}
	}
	for _, name := range dirs {
		name := name
		t.Run(name, func(t *testing.T) {
			scripts := filepath.Join(adaptersDir, name, "scripts")
			fs, err := os.ReadDir(scripts)
			if err != nil {
				return // adapter has no scripts/ directory
			}
			for _, f := range fs {
				if !strings.HasSuffix(f.Name(), ".star") {
					continue
				}
				path := filepath.Join(scripts, f.Name())
				src, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%s: read: %v", f.Name(), err)
				}
				if _, err := syntax.Parse(path, src, 0); err != nil {
					t.Errorf("%s: %v", f.Name(), err)
				}
			}
		})
	}
}
