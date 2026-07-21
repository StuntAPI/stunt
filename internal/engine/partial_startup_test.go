package engine

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/rules"
)

// TestPartialStartupOneBrokenServiceDoesNotKillUp verifies that when a
// manifest has one good (rules-only) service and one broken (nonexistent
// adapter) service, the engine still starts and serves the good service.
// The broken service should be logged as an error but not prevent serving.
func TestPartialStartupOneBrokenServiceDoesNotKillUp(t *testing.T) {
	mDir := t.TempDir()
	mPath := filepath.Join(mDir, "stunt.yaml")
	m := &manifest.Manifest{
		Version: 1,
		Path:    mPath,
		Network: manifest.Network{Mode: "port", BasePort: 18080},
		Services: map[string]manifest.Service{
			"good": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/hello"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"msg": "hi"}}}},
			}},
			"bad": {Adapter: filepath.Join(mDir, "does-not-exist")},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New should not fail with one broken service: %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrs, shutdown, err := e.Start(ctx)
	if err != nil {
		t.Fatalf("engine.Start should succeed with one broken service: %v", err)
	}
	defer shutdown()

	goodAddr, ok := addrs["good"]
	if !ok {
		t.Fatal("good service should have an address")
	}

	// Verify the good service is actually serving.
	resp, err := http.Get(goodAddr + "/hello")
	if err != nil {
		t.Fatalf("GET good service: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Errorf("good service status = %d, want 200", resp.StatusCode)
	}
	if string(body) != `{"msg":"hi"}` {
		t.Errorf("good service body = %q, want {\"msg\":\"hi\"}", string(body))
	}

	// The broken service should have a load error stored.
	if e.ServiceLoadError("bad") == "" {
		t.Error("expected a load error for the broken service")
	}
}

// TestPartialStartupAllBrokenServicesFails verifies that if ALL services are
// broken (no valid adapter, no rules-only fallback), the engine returns an
// error. There must be at least one servable service.
func TestPartialStartupAllBrokenServicesFails(t *testing.T) {
	mDir := t.TempDir()
	mPath := filepath.Join(mDir, "stunt.yaml")
	m := &manifest.Manifest{
		Version: 1,
		Path:    mPath,
		Network: manifest.Network{Mode: "port", BasePort: 18090},
		Services: map[string]manifest.Service{
			"bad1": {Adapter: filepath.Join(mDir, "nope-1")},
			"bad2": {Adapter: filepath.Join(mDir, "nope-2")},
		},
	}

	_, err := New(m)
	if err == nil {
		t.Fatal("engine.New should fail when all services are broken")
	}
}

// TestPartialStartupBrokenServiceReturns404 verifies that a broken service
// is still reachable (it just returns an error response, not a crash).
func TestPartialStartupBrokenServiceReturns404(t *testing.T) {
	mDir := t.TempDir()
	mPath := filepath.Join(mDir, "stunt.yaml")
	m := &manifest.Manifest{
		Version: 1,
		Path:    mPath,
		Network: manifest.Network{Mode: "port", BasePort: 18091},
		Services: map[string]manifest.Service{
			"good": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/ok"}, Respond: rules.Respond{Status: 200}},
			}},
			"bad": {Adapter: filepath.Join(mDir, "does-not-exist")},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrs, shutdown, err := e.Start(ctx)
	if err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	defer shutdown()

	// The broken service should still have an address (it serves 404).
	badAddr, ok := addrs["bad"]
	if !ok {
		t.Fatal("broken service should still have an address")
	}

	// Curling it should return an error status, not a connection refused.
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(badAddr + "/anything")
	if err != nil {
		t.Fatalf("GET broken service should not connection-refuse: %v", err)
	}
	defer resp.Body.Close()
	// It should be a 503 (service unavailable for broken adapter) or 404/500.
	if resp.StatusCode != http.StatusServiceUnavailable && resp.StatusCode != 404 && resp.StatusCode != 500 {
		t.Errorf("broken service status = %d, want 503/404/500", resp.StatusCode)
	}
}
