package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/engine"
	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/rules"
)

// TestRunUpPortCleanShutdown verifies that when `stunt up` (port mode) is
// stopped via SIGTERM, it prints "stopped." instead of "context canceled"
// (dog1 finding "context canceled printed on shutdown").
func TestRunUpPortCleanShutdown(t *testing.T) {
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
		t.Fatal(err)
	}
	defer e.Close()

	var mu sync.Mutex
	var out bytes.Buffer
	safeOut := &lockingWriter{mu: &mu, buf: &out}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- runUpPort(ctx, e, m, safeOut) }()

	// Wait for banner.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for banner")
		case err := <-done:
			t.Fatalf("runUpPort exited early: %v", err)
		case <-time.After(50 * time.Millisecond):
			mu.Lock()
			s := out.String()
			mu.Unlock()
			if strings.Contains(s, "Ctrl-C") {
				// Cancel the context (simulates SIGTERM).
				cancel()
				err := <-done
				mu.Lock()
				finalOut := out.String()
				mu.Unlock()
				if err != nil {
					t.Errorf("runUpPort returned error on graceful shutdown: %v", err)
				}
				if strings.Contains(finalOut, "context canceled") {
					t.Errorf("should not print 'context canceled' on shutdown:\n%s", finalOut)
				}
				if !strings.Contains(finalOut, "stopped.") {
					t.Errorf("should print 'stopped.' on shutdown:\n%s", finalOut)
				}
				return
			}
		}
	}
}

// Ensure syscall import is used (referenced in other tests in the package).
var _ = syscall.SIGTERM
