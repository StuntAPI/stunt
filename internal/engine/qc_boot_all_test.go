package engine

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestQCBootAllReferenceAdapters loads every adapter under adapters/,
// mounts it through the real engine (adapter.Load + Starlark runtime +
// state setup), and confirms it boots without error. This is the
// definitive "do the adapters actually work" gate. It skips gracefully
// when the adapters/ directory is absent (e.g. in clones that don't ship
// the reference catalog), so it is safe to keep as a permanent regression
// guard.
func TestQCBootAllReferenceAdapters(t *testing.T) {
	adaptersDir := repoAdaptersDir(t)
	if adaptersDir == "" {
		t.Skip("adapters/ directory not found — skipping reference-adapter boot QC")
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
	sort.Strings(dirs)

	if len(dirs) == 0 {
		t.Skip("no *-style adapter directories found")
	}

	t.Logf("QC: booting %d reference adapters through the engine", len(dirs))

	for _, name := range dirs {
		name := name
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(adaptersDir, name)
			tmp := t.TempDir()
			m := &manifest.Manifest{
				Path:    filepath.Join(tmp, "stunt.yaml"),
				Network: manifest.Network{BasePort: 0},
				Services: map[string]manifest.Service{
					"svc": {Adapter: dir},
				},
			}
			// newEngine(m, cacheRoot) derives the state dir from m.Path, so
			// it lands under tmp — never touching ~/.stunt.
			e, err := newEngine(m, t.TempDir())
			if err != nil {
				t.Fatalf("engine.New: %v", err)
			}
			defer e.Close()

			// Boot on free ports and fire a probe request.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			addrs, stop, err := e.ServeForTest(ctx)
			if err != nil {
				t.Fatalf("ServeForTest: %v", err)
			}
			defer stop()

			addr, ok := addrs["svc"]
			if !ok {
				t.Fatalf("no address for service svc; got %v", addrs)
			}

			// Fire a probe: a GET to the root. We only require that the
			// server RESPONDS (any status). A connection refused or panic
			// is the failure signal we care about.
			resp, err := http.Get(addr + "/")
			if err != nil {
				t.Fatalf("probe GET / failed: %v", err)
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			// Any response (including 404) means the server is alive and
			// the adapter booted. The Starlark handlers + state are live.
			if resp.StatusCode == 0 {
				t.Fatalf("no HTTP status returned")
			}
		})
	}
}

// repoAdaptersDir locates the adapters/ directory relative to the test
// source file. Returns "" if not found.
func repoAdaptersDir(t *testing.T) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	// internal/engine/qc_boot_all_test.go -> ../../adapters
	dir := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	candidate := filepath.Join(dir, "adapters")
	if st, err := os.Stat(candidate); err == nil && st.IsDir() {
		return candidate
	}
	return ""
}
