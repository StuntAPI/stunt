package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stunt-adapters/stunt/internal/netutil"
)

func TestRunClean(t *testing.T) {
	mdir := t.TempDir()

	// Create state dir with a dummy file.
	sp := statePath(mdir)
	if err := os.MkdirAll(sp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sp, "dummy.db"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create CA.
	caDir := caPath(mdir)
	_, err := netutil.EnsureCA(caDir)
	if err != nil {
		t.Fatal(err)
	}

	// Create hosts file with managed block.
	hp := filepath.Join(t.TempDir(), "hosts")
	os.WriteFile(hp, []byte("127.0.0.1 localhost\n"), 0o644)
	if err := netutil.SyncHosts(hp, []netutil.HostEntry{{Host: "svc.localhost"}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runClean(&out, mdir, hp); err != nil {
		t.Fatal(err)
	}

	// State dir removed.
	if _, err := os.Stat(sp); !os.IsNotExist(err) {
		t.Error("state dir should have been removed")
	}

	// CA dir removed.
	if _, err := os.Stat(caDir); !os.IsNotExist(err) {
		t.Error("CA dir should have been removed")
	}

	// Hosts block removed.
	content, _ := os.ReadFile(hp)
	if strings.Contains(string(content), "svc.localhost") {
		t.Error("hosts block should have been removed")
	}
	if !strings.Contains(string(content), "127.0.0.1 localhost") {
		t.Error("original hosts content should be preserved")
	}

	// Output mentions trust note.
	if !strings.Contains(out.String(), "trust-store") {
		t.Errorf("output should mention trust-store note:\n%s", out.String())
	}
}

func TestRunClean_NothingToClean(t *testing.T) {
	mdir := t.TempDir()
	hp := filepath.Join(t.TempDir(), "hosts")
	os.WriteFile(hp, []byte("127.0.0.1 localhost\n"), 0o644)

	var out bytes.Buffer
	if err := runClean(&out, mdir, hp); err != nil {
		t.Fatal(err)
	}
	// When nothing exists, output should NOT claim removal.
	if strings.Contains(out.String(), "removed state") {
		t.Errorf("should not claim state removal when nothing exists: %q", out.String())
	}
	if strings.Contains(out.String(), "removed CA") {
		t.Errorf("should not claim CA removal when nothing exists: %q", out.String())
	}
	if strings.Contains(out.String(), "cleaned hosts") {
		t.Errorf("should not claim hosts cleaning when nothing exists: %q", out.String())
	}
}
