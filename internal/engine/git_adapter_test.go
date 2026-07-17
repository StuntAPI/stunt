package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/rules"
)

// requireGit skips the test if git is not on PATH.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH — skipping live git adapter test")
	}
}

// runGit runs a git command in dir, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// makeGitAdapterRepo creates a local git repository containing a tiny adapter
// (adapter.yaml + a Starlark handler script) and returns the file:// clone URL.
// The repo has a single commit tagged "v1.0".
func makeGitAdapterRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	writeFile(t, dir, "adapter.yaml", `
id: git-fixture
name: Git Fixture
endpoints:
  - route: /ping
    method: GET
    handler: scripts/ping.star#on_get
`)

	writeFile(t, dir, "scripts/ping.star", `
def on_get(req):
    return respond(200, {"pong": True})
`)

	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial")
	runGit(t, dir, "tag", "v1.0")

	return "file://" + dir
}

// TestResolveGitAdapter builds an Engine over a manifest whose service has a
// git source spec (git:file:///<local-repo>@v1.0), sets the cache root to a
// temp dir, and asserts the engine clones, loads, and serves the adapter.
// It also includes a local-path adapter service and an inline-rules service
// as regressions.
//
// This test is fully offline: no network, no ~/.stunt.
func TestResolveGitAdapter(t *testing.T) {
	requireGit(t)

	// --- build a fixture git adapter repo ---
	cloneURL := makeGitAdapterRepo(t)

	// --- build a local-path adapter for regression ---
	localAdapterDir := t.TempDir()
	writeFile(t, localAdapterDir, "adapter.yaml", `
id: local-fixture
name: Local Fixture
endpoints:
  - route: /hello
    method: GET
    handler: scripts/hello.star#on_get
`)
	writeFile(t, localAdapterDir, "scripts/hello.star", `
def on_get(req):
    return respond(200, {"msg": "local-ok"})
`)

	// --- temp cache root (NOT ~/.stunt) ---
	cacheRoot := t.TempDir()

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			// Git-ref adapter: should be cloned into cache and loaded.
			"git-svc": {Adapter: "git:" + cloneURL + "@v1.0"},
			// Local-path adapter: should work as before.
			"local-svc": {Adapter: localAdapterDir},
			// Inline-rules: should work as before (no adapter).
			"inline-svc": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/ok"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"msg": "inline-ok"}}}},
			}},
		},
	}

	e, err := newEngine(m, cacheRoot)
	if err != nil {
		t.Fatalf("newEngine: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(30 * time.Millisecond)

	// --- git adapter: GET /ping → 200 {"pong": true} ---
	body, status := get2(t, addrs["git-svc"]+"/ping")
	if status != 200 {
		t.Fatalf("GET /ping (git-svc) -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, `"pong":true`) {
		t.Fatalf("GET /ping body = %s, want pong:true", body)
	}

	// --- local adapter: GET /hello → 200 {"msg":"local-ok"} ---
	body, status = get2(t, addrs["local-svc"]+"/hello")
	if status != 200 {
		t.Fatalf("GET /hello (local-svc) -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, `"local-ok"`) {
		t.Fatalf("GET /hello body = %s, want local-ok", body)
	}

	// --- inline rules: GET /ok → 200 {"msg":"inline-ok"} ---
	body, status = get2(t, addrs["inline-svc"]+"/ok")
	if status != 200 {
		t.Fatalf("GET /ok (inline-svc) -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, `"inline-ok"`) {
		t.Fatalf("GET /ok body = %s, want inline-ok", body)
	}

	// --- verify the cache directory was actually populated ---
	cacheGitDir := filepath.Join(cacheRoot, "git")
	entries, err := os.ReadDir(filepath.Join(cacheGitDir, "localhost"))
	if err != nil {
		t.Fatalf("expected cache dir under %s: %v", cacheGitDir, err)
	}
	if len(entries) == 0 {
		t.Fatalf("cache dir %s/localhost is empty", cacheGitDir)
	}
}

// TestResolveGitAdapterFetchError verifies that a git spec pointing at a
// nonexistent repo returns a clear error from the constructor.
func TestResolveGitAdapterFetchError(t *testing.T) {
	requireGit(t)

	cacheRoot := t.TempDir()
	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"bad": {Adapter: "git:file:///nonexistent/repo@v1.0"},
		},
	}

	_, err := newEngine(m, cacheRoot)
	if err == nil {
		t.Fatal("expected error for unresolvable git adapter, got nil")
	}
	if !strings.Contains(err.Error(), "fetch adapter") {
		t.Fatalf("error should mention 'fetch adapter', got: %v", err)
	}
}

// TestResolveLocalPathRelative verifies that a relative local adapter path is
// resolved relative to the manifest directory.
func TestResolveLocalPathRelative(t *testing.T) {
	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	// Create the adapter directory relative to the manifest directory.
	writeFile(t, stateDir, "my-adapter/adapter.yaml", `
id: rel-fixture
name: Relative Fixture
endpoints:
  - route: /rel
    method: GET
    handler: scripts/rel.star#on_get
`)
	writeFile(t, stateDir, "my-adapter/scripts/rel.star", `
def on_get(req):
    return respond(200, {"msg": "relative-ok"})
`)

	cacheRoot := t.TempDir()

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"rel": {Adapter: "./my-adapter"},
		},
	}

	e, err := newEngine(m, cacheRoot)
	if err != nil {
		t.Fatalf("newEngine: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(30 * time.Millisecond)

	body, status := get2(t, addrs["rel"]+"/rel")
	if status != 200 {
		t.Fatalf("GET /rel -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, `"relative-ok"`) {
		t.Fatalf("GET /rel body = %s, want relative-ok", body)
	}
}

// TestResolveLocalPathRegression ensures that a local-path adapter still works
// when the STUNT_ADAPTER_CACHE env var is set (it should only affect git
// sources, not local paths).
func TestResolveLocalPathWithEnvCache(t *testing.T) {
	localAdapterDir := t.TempDir()
	writeFile(t, localAdapterDir, "adapter.yaml", `
id: env-cache-fixture
name: Env Cache Fixture
endpoints:
  - route: /env
    method: GET
    handler: scripts/env.star#on_get
`)
	writeFile(t, localAdapterDir, "scripts/env.star", `
def on_get(req):
    return respond(200, {"msg": "env-ok"})
`)

	// Set a temp cache dir via env to ensure it doesn't interfere with local paths.
	cacheDir := t.TempDir()
	t.Setenv("STUNT_ADAPTER_CACHE", cacheDir)

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"svc": {Adapter: localAdapterDir},
		},
	}

	e, err := New(m) // Uses the env-set cache root.
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(30 * time.Millisecond)

	body, status := get2(t, addrs["svc"]+"/env")
	if status != 200 {
		t.Fatalf("GET /env -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, `"env-ok"`) {
		t.Fatalf("GET /env body = %s, want env-ok", body)
	}
}
