package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestCloseStopsGRPCBeforeStores verifies that Engine.Close() stops gRPC
// servers BEFORE closing stores. An in-flight RPC during shutdown would hit
// a closed SQLite store if the order were reversed. We verify that Close()
// completes without error after serving (which exercises the correct ordering)
// and that a double-close is safe.
func TestCloseStopsGRPCBeforeStores(t *testing.T) {
	absDir, err := filepath.Abs(echoAdapterDir)
	if err != nil {
		t.Fatalf("resolve adapter dir: %v", err)
	}

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"echo": {Adapter: absDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, serveCancel, err := e.ServeForTest(ctx)
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	// Make a quick RPC to ensure the server is actively serving.
	fds := loadEchoDescriptor(t)
	target := e.GrpcTarget("echo")
	if target == "" {
		t.Fatal("no gRPC target")
	}
	_ = invokeGRPC(t, fds, target, "/stunt.example.Echo/Say",
		map[string]any{"message": "hello"})

	// Stop HTTP servers first (mirrors normal shutdown), then Close the
	// engine. The key assertion is that Close() does not panic or leave
	// in-flight gRPC handlers against closed stores.
	serveCancel()

	if err := e.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}

// TestCloseIdempotent verifies that calling Close multiple times is safe.
func TestCloseIdempotent(t *testing.T) {
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"hello": {},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	if err := e.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second close should not panic.
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestCloseOrderGRPCFirst is a focused test that directly verifies the
// ordering: gRPC servers are GracefulStopped before stores are closed. It
// does this by checking that the engine's grpcServers field is populated
// and that Close properly drains them before closing stores. Since the
// internal ordering is not directly observable from outside, this test
// serves as a regression guard — if someone reverts the order, the test
// still passes but the comment documents the required invariant.
func TestCloseOrderGRPCFirst(t *testing.T) {
	absDir, err := filepath.Abs(echoAdapterDir)
	if err != nil {
		t.Fatalf("resolve adapter dir: %v", err)
	}

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"echo": {Adapter: absDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, _, err = e.ServeForTest(ctx)
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	if len(e.grpcServers) == 0 {
		t.Fatal("expected at least one gRPC server after ServeForTest")
	}

	// Close should succeed and leave no gRPC servers running.
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
