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
