package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/contrib"
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

// TestPlanWarnsOnMissingHandlerScript verifies that plan emits a WARNING
// when an adapter endpoint references a handler script file that does not
// exist on disk.
func TestPlanWarnsOnMissingHandlerScript(t *testing.T) {
	dir := t.TempDir()
	if err := contrib.Scaffold(dir, "myapi", contrib.ScaffoldOptions{}); err != nil {
		t.Fatal(err)
	}
	// Overwrite adapter.yaml so the endpoint points at a non-existent script.
	adapterYAML := `id: myapi
name: MyAPI
version: "0.1.0"
endpoints:
  - route: /hello
    method: GET
    handler: scripts/missing.star#on_get
`
	if err := os.WriteFile(filepath.Join(dir, "myapi", "adapter.yaml"), []byte(adapterYAML), 0o644); err != nil {
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
		t.Fatalf("plan should not error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "WARNING") {
		t.Errorf("expected WARNING for missing handler script, got:\n%s", out)
	}
	if !strings.Contains(out, "missing.star") {
		t.Errorf("expected 'missing.star' in warning, got:\n%s", out)
	}
}

// TestPlanWarnsOnUndefinedHandlerFunction verifies that plan emits a WARNING
// when a handler script exists but does not define the referenced function.
func TestPlanWarnsOnUndefinedHandlerFunction(t *testing.T) {
	dir := t.TempDir()
	if err := contrib.Scaffold(dir, "myapi", contrib.ScaffoldOptions{}); err != nil {
		t.Fatal(err)
	}
	// Overwrite adapter.yaml to reference a function that isn't defined.
	adapterYAML := `id: myapi
name: MyAPI
version: "0.1.0"
endpoints:
  - route: /hello
    method: GET
    handler: scripts/hello.star#nonexistent
`
	if err := os.WriteFile(filepath.Join(dir, "myapi", "adapter.yaml"), []byte(adapterYAML), 0o644); err != nil {
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
		t.Fatalf("plan should not error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "WARNING") {
		t.Errorf("expected WARNING for undefined handler function, got:\n%s", out)
	}
	if !strings.Contains(out, `"nonexistent"`) {
		t.Errorf("expected 'nonexistent' in warning, got:\n%s", out)
	}
}

// TestPlanWarnsOnSyntaxError verifies that plan emits a WARNING when a
// handler script has a Starlark syntax error.
func TestPlanWarnsOnSyntaxError(t *testing.T) {
	dir := t.TempDir()
	if err := contrib.Scaffold(dir, "myapi", contrib.ScaffoldOptions{}); err != nil {
		t.Fatal(err)
	}
	// Overwrite adapter.yaml so the endpoint references the handler script.
	adapterYAML := `id: myapi
name: MyAPI
version: "0.1.0"
endpoints:
  - route: /hello
    method: GET
    handler: scripts/hello.star#on_get
`
	if err := os.WriteFile(filepath.Join(dir, "myapi", "adapter.yaml"), []byte(adapterYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	// Write a script with a syntax error (unbalanced parenthesis).
	badScript := `def on_get(req):
    return respond(200, {"msg": broken
`
	if err := os.WriteFile(filepath.Join(dir, "myapi", "scripts", "hello.star"), []byte(badScript), 0o644); err != nil {
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
		t.Fatalf("plan should not error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "WARNING") {
		t.Errorf("expected WARNING for syntax error, got:\n%s", out)
	}
	if !strings.Contains(out, "hello.star") {
		t.Errorf("expected 'hello.star' in warning, got:\n%s", out)
	}
}

// TestPlanNoWarningForCleanAdapter verifies that a clean adapter with valid
// handler scripts produces no WARNING lines.
func TestPlanNoWarningForCleanAdapter(t *testing.T) {
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
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "WARNING") {
			t.Errorf("clean adapter should not produce WARNING, got line: %s\nFull output:\n%s", line, out)
		}
	}
}

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
