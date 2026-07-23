package cli

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/engine"
	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/rules"
)

// TestStartDashboard verifies the startDashboard helper binds a live,
// token-guarded admin server and returns its URL + non-empty token.
func TestStartDashboard(t *testing.T) {
	mDir := t.TempDir()
	manifestPath := filepath.Join(mDir, "stunt.yaml")
	m := &manifest.Manifest{
		Version: 1,
		Path:    manifestPath,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"hello": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/ok"}, Respond: rules.Respond{Status: 200}},
			}},
		},
	}

	e, err := engine.New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dashURL, dashToken := startDashboard(ctx, e)

	if !strings.HasPrefix(dashURL, "http://127.0.0.1:") {
		t.Fatalf("expected localhost URL, got %q", dashURL)
	}
	if len(dashToken) == 0 {
		t.Fatalf("expected non-empty token, got %q", dashToken)
	}

	client := &http.Client{}

	// With the token → 200 (guarded but live).
	authReq, err := http.NewRequest(http.MethodGet, dashURL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	authReq.Header.Set("X-Stunt-Token", dashToken)
	authRes, err := client.Do(authReq)
	if err != nil {
		t.Fatalf("GET with token: %v", err)
	}
	defer authRes.Body.Close()
	if authRes.StatusCode != http.StatusOK {
		t.Fatalf("GET with token: want 200, got %d", authRes.StatusCode)
	}

	// Without the token → 401 (proves the guard is active).
	noAuthRes, err := client.Get(dashURL)
	if err != nil {
		t.Fatalf("GET without token: %v", err)
	}
	defer noAuthRes.Body.Close()
	if noAuthRes.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET without token: want 401, got %d", noAuthRes.StatusCode)
	}
}

// TestRunUpPortWritesDashboardRuntime verifies that runUpPort writes the
// dashboard URL + token into the per-manifest runtime file (up.json), so
// `stunt requests`/`stunt ui` can auto-discover the instance with no flags.
func TestRunUpPortWritesDashboardRuntime(t *testing.T) {
	mDir := t.TempDir()
	manifestPath := filepath.Join(mDir, "stunt.yaml")
	m := &manifest.Manifest{
		Version: 1,
		Path:    manifestPath,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"hello": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/ok"}, Respond: rules.Respond{Status: 200}},
			}},
		},
	}

	e, err := engine.New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var out bytes.Buffer
	safeOut := &lockingWriter{mu: &mu, buf: &out}

	done := make(chan error, 1)
	go func() { done <- runUpPort(ctx, e, m, safeOut) }()

	// Wait for the banner (and dashboard line) to appear.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			mu.Lock()
			s := out.String()
			mu.Unlock()
			t.Fatalf("timeout waiting for banner. Output so far:\n%s", s)
		case err := <-done:
			mu.Lock()
			s := out.String()
			mu.Unlock()
			t.Fatalf("runUpPort exited early: %v. Output:\n%s", err, s)
		case <-time.After(50 * time.Millisecond):
		}
		mu.Lock()
		s := out.String()
		mu.Unlock()
		if strings.Contains(s, "Ctrl-C to stop") {
			break
		}
	}

	// Read the runtime file and assert it carries the dashboard URL + token.
	rt, err := readRuntimeFile(mDir)
	if err != nil {
		t.Fatalf("readRuntimeFile: %v", err)
	}
	if rt.DashboardURL == "" {
		t.Errorf("expected non-empty dashboard_url in runtime file, got empty")
	}
	if !strings.HasPrefix(rt.DashboardURL, "http://127.0.0.1:") {
		t.Errorf("expected localhost dashboard URL, got %q", rt.DashboardURL)
	}
	if rt.DashboardToken == "" {
		t.Errorf("expected non-empty dashboard_token in runtime file, got empty")
	}

	cancel()
	<-done
}
