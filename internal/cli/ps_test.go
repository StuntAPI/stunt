package cli

import (
	"bytes"
	"os"
	"strings"
	"strconv"
	"testing"
)

func TestPsEmptyAndNonEmpty(t *testing.T) {
	// Point the registry at a temp path by constructing directly.
	dir := t.TempDir()
	// We can't easily inject the path into runPs (it calls OpenRegistry → ~/.stunt).
	// Instead test runPs against a real registry via the public path, but only if
	// we can isolate it. Simplest: test runPs output shape with a registry we
	// build + register into the SAME path OpenRegistry uses.
	// To avoid polluting ~/.stunt, skip if HOME can't be redirected.
	t.Setenv("HOME", dir) // OpenRegistry uses os.UserHomeDir → now points at temp
	var out bytes.Buffer
	if err := runPs(&out, false); err != nil {
		t.Fatalf("ps empty: %v", err)
	}
	if !strings.Contains(out.String(), "no running") {
		t.Errorf("ps empty: %q", out.String())
	}

	// Register a fake instance with THIS test's PID so prune keeps it.
	reg, err := OpenRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(Instance{
		PID: os.Getpid(), Manifest: dir + "/stunt.yaml", Mode: "port",
		Services: []string{"stripe"}, DashboardURL: "http://127.0.0.1:9999",
		StartedAt: "2026-07-23T10:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runPs(&out, false); err != nil {
		t.Fatalf("ps non-empty: %v", err)
	}
	if !strings.Contains(out.String(), strconv.Itoa(os.Getpid())) {
		t.Errorf("ps should list our PID: %q", out.String())
	}

	// JSON mode.
	out.Reset()
	if err := runPs(&out, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"pid":`) {
		t.Errorf("ps --json missing pid: %q", out.String())
	}
}
