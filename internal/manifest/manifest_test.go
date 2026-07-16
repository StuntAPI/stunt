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
