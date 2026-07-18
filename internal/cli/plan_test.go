package cli

import (
	"bytes"
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
