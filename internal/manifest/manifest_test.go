package manifest

import (
	"path/filepath"
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
