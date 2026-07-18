package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stunt-adapters/stunt/internal/contrib"
)

// TestPlanWarnsOnNonExistentAdapter verifies that `stunt plan` prints a
// WARNING for a service pointing at a non-existent adapter directory,
// instead of silently printing "OK".
func TestPlanWarnsOnNonExistentAdapter(t *testing.T) {
	dir := t.TempDir()
	mPath := filepath.Join(dir, "stunt.yaml")
	content := `version: 1
network:
  mode: port
  base_port: 8000
services:
  broken:
    adapter: ./does-not-exist
`
	if err := os.WriteFile(mPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetArgs([]string{"plan", "--manifest", mPath})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("plan should not error on non-loadable adapter: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "WARNING") {
		t.Errorf("expected WARNING for non-existent adapter, got:\n%s", out)
	}
	if !strings.Contains(out, "NOT LOADABLE") {
		t.Errorf("expected 'NOT LOADABLE' marker, got:\n%s", out)
	}
}

// TestPlanShowsEndpointCountForAdapterService verifies that `stunt plan`
// shows the endpoint count for an adapter-backed service (instead of
// misleadingly "0 rules").
func TestPlanShowsEndpointCountForAdapterService(t *testing.T) {
	dir := t.TempDir()
	if err := contrib.Scaffold(dir, "myapi", contrib.ScaffoldOptions{}); err != nil {
		t.Fatal(err)
	}

	mPath := filepath.Join(dir, "stunt.yaml")
	content := `version: 1
network:
  mode: port
  base_port: 8000
services:
  api:
    adapter: ./myapi
`
	if err := os.WriteFile(mPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetArgs([]string{"plan", "--manifest", mPath})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("plan failed: %v", err)
	}

	out := buf.String()
	// The scaffolded adapter has 1 inline endpoint (/hello).
	if !strings.Contains(out, "1 endpoints") {
		t.Errorf("expected '1 endpoints' in plan output, got:\n%s", out)
	}
}

// TestPlanDoesNotShowEndpointCountForRulesOnly verifies that rules-only
// services still show just "(N rules)" without endpoint count.
func TestPlanDoesNotShowEndpointCountForRulesOnly(t *testing.T) {
	dir := t.TempDir()
	mPath := filepath.Join(dir, "stunt.yaml")
	content := `version: 1
network:
  mode: port
  base_port: 8000
services:
  example:
    rules:
      - match: { method: GET, path: /hello }
        respond: { status: 200 }
`
	if err := os.WriteFile(mPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetArgs([]string{"plan", "--manifest", mPath})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if strings.Contains(out, "endpoints") {
		t.Errorf("rules-only service should not show 'endpoints', got:\n%s", out)
	}
	if !strings.Contains(out, "1 rules") {
		t.Errorf("expected '1 rules' for rules-only service, got:\n%s", out)
	}
}

// TestPlanShowsGrpcAndWsCounts verifies that `stunt plan` shows gRPC method
// and WebSocket route counts for an adapter with a gRPC section.
func TestPlanShowsGrpcAndWsCounts(t *testing.T) {
	// Use the echo-style reference adapter which has 4 gRPC methods and
	// 1 WebSocket route.
	repoRoot := repoRoot(t)
	echoDir := filepath.Join(repoRoot, "adapters", "echo-style")
	if _, err := os.Stat(filepath.Join(echoDir, "adapter.yaml")); err != nil {
		t.Skipf("echo-style adapter not found at %s", echoDir)
	}

	mPath := filepath.Join(t.TempDir(), "stunt.yaml")
	content := fmt.Sprintf(`version: 1
network:
  mode: port
  base_port: 8000
services:
  echo:
    adapter: %s
`, echoDir)
	if err := os.WriteFile(mPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetArgs([]string{"plan", "--manifest", mPath})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("plan failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "4 grpc methods") {
		t.Errorf("expected '4 grpc methods' in plan output, got:\n%s", out)
	}
	if !strings.Contains(out, "1 ws route") {
		t.Errorf("expected '1 ws route' in plan output, got:\n%s", out)
	}
}

// repoRoot returns the git repository root for test setup. It searches
// upward from the test source file for a go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	// The test binary runs from the package directory. Walk up to find go.mod.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(cwd, "go.mod")); err == nil {
			return cwd
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			t.Fatal("could not find go.mod walking up from " + cwd)
		}
		cwd = parent
	}
}
