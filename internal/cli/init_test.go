package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitWritesManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stunt.yaml")
	if err := writeSampleManifest(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Fatalf("manifest not written (stat err=%v)", err)
	}
	m, err := loadForInit(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if m.Network.Mode != "port" || len(m.Services) == 0 {
		t.Fatalf("unexpected manifest: %+v", m)
	}
}
