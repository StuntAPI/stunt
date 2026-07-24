package engine

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"stuntapi.com/stunt/internal/manifest"
)

// newSnapshotTestEngine builds a real engine from a stripe-style manifest so
// the full New() path (per-service collections + kv + blob stores) is exercised.
func newSnapshotTestEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	man := `version: 1
rng_seed: 7
network: { mode: port, base_port: 0 }
services:
  stripe:
    adapter: embedded:stripe-style
`
	path := filepath.Join(dir, "stunt.yaml")
	if err := os.WriteFile(path, []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// TestSnapshotRestoreRoundTrip seeds state, snapshots, mutates, restores, and
// asserts the state came back to the pre-mutation snapshot.
func TestSnapshotRestoreRoundTrip(t *testing.T) {
	e := newSnapshotTestEngine(t)

	col, kvS, bl, ok := e.StateStores("stripe")
	if !ok {
		t.Fatal("no state for stripe")
	}
	// Seed: a collection doc + a kv pair + a blob.
	widgets, err := col.Collection("widgets")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := widgets.Insert(map[string]any{"id": "w1", "name": "widget one", "price": 100}); err != nil {
		t.Fatal(err)
	}
	if err := kvS.Set("cfg", "currency", "usd"); err != nil {
		t.Fatal(err)
	}
	if _, err := bl.PutWith("uploads", "logo.png", "image/png", bytes.NewReader([]byte("PNGDATA"))); err != nil {
		t.Fatal(err)
	}

	// Snapshot.
	var snap bytes.Buffer
	if err := Snapshot(e, "stunt.yaml", &snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	t.Logf("snapshot size: %d bytes", snap.Len())

	// Mutate state (simulate a test run that dirties state).
	if _, err := widgets.Insert(map[string]any{"id": "w2", "name": "transient"}); err != nil {
		t.Fatal(err)
	}
	_ = kvS.Set("cfg", "currency", "eur")
	_ = bl.Delete("uploads", "logo.png")

	// Restore.
	hdr, err := Restore(e, bytes.NewReader(snap.Bytes()))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if hdr.Version != snapshotVersion {
		t.Errorf("header version = %d, want %d", hdr.Version, snapshotVersion)
	}

	// Assert state matches the pre-mutation snapshot.
	widgets2, _ := col.Collection("widgets")
	docs, _ := widgets2.List()
	if len(docs) != 1 || docs[0]["id"] != "w1" {
		t.Fatalf("after restore, widgets = %+v (want only w1)", docs)
	}
	got, _ := kvS.Get("cfg", "currency")
	if got != "usd" {
		t.Errorf("after restore, kv cfg/currency = %q, want usd", got)
	}
	infos, _ := bl.List("uploads")
	if len(infos) != 1 || infos[0].Name != "logo.png" {
		t.Fatalf("after restore, blobs = %+v (want logo.png)", infos)
	}
	rc, _ := bl.Get("uploads", "logo.png")
	content, _ := io.ReadAll(rc)
	rc.Close()
	if string(content) != "PNGDATA" {
		t.Errorf("blob content = %q, want PNGDATA", string(content))
	}
}

// TestSnapshotEmpty ensures snapshot works on a fresh engine (no seeded state).
func TestSnapshotEmpty(t *testing.T) {
	e := newSnapshotTestEngine(t)
	var snap bytes.Buffer
	if err := Snapshot(e, "stunt.yaml", &snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// A restore of an empty snapshot should leave state empty and not error.
	if _, err := Restore(e, bytes.NewReader(snap.Bytes())); err != nil {
		t.Fatalf("restore empty: %v", err)
	}
}
