package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBasic(t *testing.T) {
	m, err := Load(filepath.Join("..", "..", "testdata", "manifest", "basic.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Version != 1 {
		t.Fatalf("Version = %d, want 1", m.Version)
	}
	if m.Network.Mode != "port" || m.Network.BasePort != 9000 {
		t.Fatalf("Network = %+v", m.Network)
	}
	svc, ok := m.Services["hello"]
	if !ok {
		t.Fatal("missing service hello")
	}
	if len(svc.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(svc.Rules))
	}
	if svc.Rules[1].When == nil || svc.Rules[1].When.Chance != 30 {
		t.Fatalf("rule[1].When = %+v", svc.Rules[1].When)
	}
	if svc.Rules[0].Respond.Body.Inline == nil {
		t.Fatal("rule[0] body.inline missing")
	}
}

func TestNetworkDefaults(t *testing.T) {
	t.Run("subdomain defaults tld", func(t *testing.T) {
		n := &Network{Mode: "subdomain"}
		n.Defaults()
		if n.TLD != "localhost" {
			t.Errorf("TLD = %q, want %q", n.TLD, "localhost")
		}
	})
	t.Run("subdomain keeps explicit tld", func(t *testing.T) {
		n := &Network{Mode: "subdomain", TLD: "test"}
		n.Defaults()
		if n.TLD != "test" {
			t.Errorf("TLD = %q, want %q", n.TLD, "test")
		}
	})
	t.Run("port mode no defaults", func(t *testing.T) {
		n := &Network{Mode: "port", BasePort: 8000}
		n.Defaults()
		if n.TLD != "" {
			t.Errorf("TLD = %q, want empty for port mode", n.TLD)
		}
	})
}

// --- Save ---

func TestSaveRoundTrip(t *testing.T) {
	m := &Manifest{
		Version: 1,
		Network: Network{Mode: "port", BasePort: 8000},
		Services: map[string]Service{
			"hello": {Adapter: "git:github.com/user/repo@v1.0"},
		},
	}
	path := filepath.Join(t.TempDir(), "stunt.yaml")
	if err := Save(m, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	m2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m2.Version != 1 {
		t.Errorf("Version = %d, want 1", m2.Version)
	}
	svc, ok := m2.Services["hello"]
	if !ok {
		t.Fatal("missing service hello")
	}
	if svc.Adapter != "git:github.com/user/repo@v1.0" {
		t.Errorf("Adapter = %q, want git source spec", svc.Adapter)
	}
}

func TestSaveWritesVersionFirst(t *testing.T) {
	m := &Manifest{
		Version: 1,
		Network: Network{Mode: "port", BasePort: 8000},
		Services: map[string]Service{
			"hello": {Adapter: "/local/path"},
		},
	}
	path := filepath.Join(t.TempDir(), "stunt.yaml")
	if err := Save(m, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := osReadFileManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "version:") {
		t.Errorf("expected 'version:' as first line, got: %q", lines[0])
	}
}

func osReadFileManifest(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// --- I1: atomic save ---

// TestSaveAtomicRoundTrip verifies the basic write+read roundtrip works.
func TestSaveAtomicRoundTrip(t *testing.T) {
	m := &Manifest{
		Version: 1,
		Network: Network{Mode: "port", BasePort: 9000},
		Services: map[string]Service{
			"hello": {Adapter: "git:github.com/user/repo@v1.0"},
			"world": {Adapter: "/local/path"},
		},
	}
	path := filepath.Join(t.TempDir(), "stunt.yaml")
	if err := Save(m, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	m2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m2.Version != 1 {
		t.Errorf("Version = %d, want 1", m2.Version)
	}
	if len(m2.Services) != 2 {
		t.Fatalf("Services = %d, want 2", len(m2.Services))
	}
	if m2.Services["hello"].Adapter != "git:github.com/user/repo@v1.0" {
		t.Errorf("hello adapter = %q", m2.Services["hello"].Adapter)
	}
	if m2.Services["world"].Adapter != "/local/path" {
		t.Errorf("world adapter = %q", m2.Services["world"].Adapter)
	}
}

// TestSaveDoesNotLeaveTempFile verifies that after a successful save, no
// temporary file is left behind in the directory.
func TestSaveDoesNotLeaveTempFile(t *testing.T) {
	absDir := t.TempDir()
	path := filepath.Join(absDir, "stunt.yaml")
	m := &Manifest{
		Version:  1,
		Network:  Network{Mode: "port", BasePort: 8000},
		Services: map[string]Service{"hello": {Adapter: "/local"}},
	}
	if err := Save(m, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".stunt-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// TestSaveOverwritesExistingFile verifies that saving over an existing file
// replaces it atomically (the old content is gone, new content is present).
func TestSaveOverwritesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stunt.yaml")

	// Write initial.
	m1 := &Manifest{
		Version:  1,
		Network:  Network{Mode: "port", BasePort: 8000},
		Services: map[string]Service{"svc1": {Adapter: "/old"}},
	}
	if err := Save(m1, path); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	// Overwrite.
	m2 := &Manifest{
		Version:  1,
		Network:  Network{Mode: "port", BasePort: 9000},
		Services: map[string]Service{"svc2": {Adapter: "/new"}},
	}
	if err := Save(m2, path); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, exists := loaded.Services["svc1"]; exists {
		t.Error("old service should be gone after overwrite")
	}
	if loaded.Services["svc2"].Adapter != "/new" {
		t.Errorf("svc2 adapter = %q, want /new", loaded.Services["svc2"].Adapter)
	}
	if loaded.Network.BasePort != 9000 {
		t.Errorf("BasePort = %d, want 9000", loaded.Network.BasePort)
	}
}
