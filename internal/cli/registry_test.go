package cli

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// newTestRegistry points the registry at a temp-dir path (no ~/.stunt side effects).
func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	return &Registry{path: filepath.Join(dir, "instances.json")}
}

func TestRegisterDeregister(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.Register(Instance{PID: 100, Manifest: "/a/stunt.yaml", StartedAt: "2026-07-23T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Instance{PID: 200, Manifest: "/b/stunt.yaml", StartedAt: "2026-07-23T11:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	got, err := r.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("after 2 registers, List = %d instances, want 2", len(got))
	}
	// sorted by StartedAt → PID 100 first
	if got[0].PID != 100 || got[1].PID != 200 {
		t.Errorf("order = %d,%d, want 100,200", got[0].PID, got[1].PID)
	}
	// Deregister one.
	if err := r.Deregister(100); err != nil {
		t.Fatal(err)
	}
	got, _ = r.List(false)
	if len(got) != 1 || got[0].PID != 200 {
		t.Fatalf("after deregister, got %+v, want only PID 200", got)
	}
}

func TestRegisterReplacesSamePID(t *testing.T) {
	r := newTestRegistry(t)
	_ = r.Register(Instance{PID: 1, Manifest: "/old.yaml"})
	_ = r.Register(Instance{PID: 1, Manifest: "/new.yaml"})
	got, _ := r.List(false)
	if len(got) != 1 || got[0].Manifest != "/new.yaml" {
		t.Fatalf("re-register should replace; got %+v", got)
	}
}

func TestListPrunesDeadPIDs(t *testing.T) {
	r := newTestRegistry(t)
	// Use a definitely-dead PID (this very test's PID's parent is not us, but
	// PID 999999 is vanishingly unlikely to exist).
	_ = r.Register(Instance{PID: 999999, Manifest: "/dead.yaml", StartedAt: "2026-07-23T09:00:00Z"})
	// Register a live PID (this test process).
	_ = r.Register(Instance{PID: os.Getpid(), Manifest: "/live.yaml", StartedAt: "2026-07-23T09:01:00Z"})
	got, err := r.List(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PID != os.Getpid() {
		t.Fatalf("prune should drop dead PID; got %+v", got)
	}
	// Pruned state must be persisted.
	got2, _ := r.List(false)
	if len(got2) != 1 {
		t.Errorf("prune not persisted: List(false) = %d", len(got2))
	}
}

func TestConcurrentRegister(t *testing.T) {
	r := newTestRegistry(t)
	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			_ = r.Register(Instance{PID: 1000 + i, Manifest: "/x.yaml"})
		}()
	}
	wg.Wait()
	got, err := r.List(false)
	if err != nil {
		t.Fatalf("List after concurrent registers: %v", err)
	}
	if len(got) != N {
		t.Errorf("concurrent register: got %d instances, want %d (a lost update = flock failure)", len(got), N)
	}
}
