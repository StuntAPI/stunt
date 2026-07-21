package adapters

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/adapter"
)

// repoAdaptersDir locates the adapters/ directory relative to this test file
// so we can compare the embedded set against what's on disk. Returns "" if
// not found.
func repoAdaptersDir(t *testing.T) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	dir := filepath.Dir(filepath.Dir(file))
	candidate := filepath.Join(dir, "adapters")
	if st, err := os.Stat(candidate); err == nil && st.IsDir() {
		return candidate
	}
	return ""
}

// TestEmbeddedAdaptersMatchDisk verifies that every adapter directory on disk
// is present in the embedded binary tree. This catches a go:embed pattern that
// silently misses adapters.
func TestEmbeddedAdaptersMatchDisk(t *testing.T) {
	adaptersDir := repoAdaptersDir(t)
	if adaptersDir == "" {
		t.Skip("adapters/ directory not found relative to test")
	}
	entries, err := os.ReadDir(adaptersDir)
	if err != nil {
		t.Skipf("cannot read %s: %v", adaptersDir, err)
	}
	var want []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasSuffix(e.Name(), "-style") {
			continue
		}
		if _, err := os.Stat(filepath.Join(adaptersDir, e.Name(), "adapter.yaml")); err == nil {
			want = append(want, e.Name())
		}
	}

	got := Names()
	if len(got) != len(want) {
		t.Fatalf("embedded adapter count = %d, want %d (disk)\nembedded: %v\nwant:     %v",
			len(got), len(want), got, want)
	}
	have := make(map[string]bool, len(got))
	for _, n := range got {
		have[n] = true
	}
	for _, n := range want {
		if !have[n] {
			t.Errorf("adapter %q is on disk but NOT embedded", n)
		}
	}
}

// TestExtractProducesLoadableAdapter verifies that an extracted embedded
// adapter loads successfully through the real adapter loader (proving the
// extraction preserves the layout adapter.Load expects).
func TestExtractProducesLoadableAdapter(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Skip("no embedded adapters")
	}
	// Spot-check stripe-style (the flagship reference adapter).
	const name = "stripe-style"
	if !Has(name) {
		t.Fatalf("%q not embedded", name)
	}
	dst := t.TempDir()
	target := filepath.Join(dst, name)
	if err := Extract(name, target); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// adapter.yaml must be at the root of the extracted dir.
	if _, err := os.Stat(filepath.Join(target, "adapter.yaml")); err != nil {
		t.Fatalf("adapter.yaml missing after extract: %v", err)
	}
	// The extracted adapter must load.
	a, err := adapter.Load(target)
	if err != nil {
		t.Fatalf("adapter.Load(extracted): %v", err)
	}
	if a.ID != name {
		t.Errorf("loaded ID = %q, want %q", a.ID, name)
	}
}

// TestIndexCoversAll verifies the generated catalog index has one entry per
// embedded adapter, each with a non-empty description.
func TestIndexCoversAll(t *testing.T) {
	names := Names()
	idx := Index()
	if len(idx) != len(names) {
		t.Fatalf("Index has %d entries, want %d (one per adapter)", len(idx), len(names))
	}
	seen := make(map[string]bool, len(idx))
	for _, e := range idx {
		if e.Description == "" {
			t.Errorf("entry %q has empty description", e.Name)
		}
		seen[e.Name] = true
	}
	for _, n := range names {
		if !seen[n] {
			t.Errorf("adapter %q missing from Index", n)
		}
	}
}
