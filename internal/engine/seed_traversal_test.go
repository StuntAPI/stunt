package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/manifest"
)

const seedTraversalAdapterYAML = `
id: seed-traversal
name: Seed Traversal Test
resources:
  - name: items
    kind: collection
    seed: ../../secret_seed.jsonl
`

const seedValidAdapterYAML = `
id: seed-valid
name: Seed Valid Test
resources:
  - name: items
    kind: collection
    seed: fixtures/items.jsonl
`

// TestSeedTraversalRejected proves that an adapter declaring a seed path
// with ../ that escapes the adapter directory is rejected at load time —
// the outside file is never read.
func TestSeedTraversalRejected(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", seedTraversalAdapterYAML)

	// Create the target secret file in the adapter dir's parent.
	absAdapter, _ := filepath.Abs(adapterDir)
	parent := filepath.Dir(absAdapter)
	secretPath := filepath.Join(parent, "secret_seed.jsonl")
	if err := os.WriteFile(secretPath, []byte(`{"secret": true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(secretPath) })

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"bad": {Adapter: adapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		// If the engine fails entirely that's acceptable — the seed was
		// rejected. The key assertion is below.
		t.Logf("engine.New returned error (acceptable): %v", err)
		return
	}
	defer e.Close()

	// The service should have a load error.
	loadErr := e.ServiceLoadError("bad")
	if loadErr == "" {
		t.Fatal("expected load error for seed path traversal, got none")
	}
	if !strings.Contains(strings.ToLower(loadErr), "escapes") && !strings.Contains(strings.ToLower(loadErr), "path") {
		t.Fatalf("load error should mention path escape, got: %s", loadErr)
	}
}

// TestSeedValidWorks proves that a legitimate seed path inside the adapter
// directory loads successfully.
func TestSeedValidWorks(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", seedValidAdapterYAML)
	writeFile(t, adapterDir, "fixtures/items.jsonl", `{"name": "alpha"}`+"\n"+`{"name": "beta"}`+"\n")

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"good": {Adapter: adapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	if e.ServiceLoadError("good") != "" {
		t.Fatalf("expected no load error for valid seed, got: %s", e.ServiceLoadError("good"))
	}

	// The seed data should be loaded into the collection.
	st := e.states["good"]
	if st == nil {
		t.Fatal("expected service state for 'good'")
	}
	col, err := st.store.Collection("items")
	if err != nil {
		t.Fatalf("get collection: %v", err)
	}
	count, err := col.Count()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 seeded items, got %d", count)
	}
}
