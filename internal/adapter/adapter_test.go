package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAdapter is a test helper that lays out an adapter directory on disk
// from a set of named files (relative paths) and their contents.
func writeAdapter(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

const validAdapterYAML = `
id: stripe-charges
name: Stripe Charges
version: "2024-06-20"
real_hosts:
  - api.stripe.com
endpoints:
  - route: /v1/charges
    method: GET
    rules:
      - name: list-ok
        match: { method: GET, path: /v1/charges }
        respond: { status: 200, body: { inline: { data: [] } } }
  - route: /v1/charges
    method: POST
    handler: scripts/charges.star#on_post
resources:
  - name: charges
    kind: collection
    seed: fixtures/charges.jsonl
identity:
  token_scheme: bearer
rules:
  - name: catchall-404
    match: { path: "/**" }
    respond: { status: 404, body: { inline: { error: not_found } } }
`

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	writeAdapter(t, dir, map[string]string{
		"adapter.yaml":       validAdapterYAML,
		"scripts/charges.star": "def on_post(req):\n    return respond(201, {})\n",
		"fixtures/charges.jsonl": `{"id":"ch_1","amount":1000}` + "\n" + `{"id":"ch_2","amount":2000}` + "\n",
	})

	a, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// --- metadata ---
	if a.ID != "stripe-charges" {
		t.Fatalf("ID = %q", a.ID)
	}
	if a.Name != "Stripe Charges" {
		t.Fatalf("Name = %q", a.Name)
	}
	if a.Version != "2024-06-20" {
		t.Fatalf("Version = %q", a.Version)
	}
	if len(a.RealHosts) != 1 || a.RealHosts[0] != "api.stripe.com" {
		t.Fatalf("RealHosts = %v", a.RealHosts)
	}

	// --- Dir set ---
	if a.Dir != dir {
		t.Fatalf("Dir = %q, want %q", a.Dir, dir)
	}

	// --- endpoints ---
	if len(a.Endpoints) != 2 {
		t.Fatalf("Endpoints: got %d, want 2", len(a.Endpoints))
	}
	get := a.Endpoints[0]
	if get.Route != "/v1/charges" || get.Method != "GET" {
		t.Fatalf("endpoint[0] = %+v", get)
	}
	if len(get.Rules) != 1 {
		t.Fatalf("endpoint[0] rules: %d", len(get.Rules))
	}
	if get.Rules[0].Name != "list-ok" {
		t.Fatalf("endpoint[0] rule name = %q", get.Rules[0].Name)
	}

	post := a.Endpoints[1]
	if post.Handler == "" {
		t.Fatal("endpoint[1] handler is empty")
	}
	// Handler path should now be absolute and include the #fragment.
	if !filepath.IsAbs(strings.SplitN(post.Handler, "#", 2)[0]) {
		t.Fatalf("handler path not absolute: %q", post.Handler)
	}
	if !strings.HasSuffix(post.Handler, "#on_post") {
		t.Fatalf("handler missing #on_post suffix: %q", post.Handler)
	}
	// The resolved path should actually exist on disk.
	scriptPath := strings.SplitN(post.Handler, "#", 2)[0]
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("resolved handler script does not exist: %v", err)
	}

	// --- resources ---
	if len(a.Resources) != 1 {
		t.Fatalf("Resources: %d", len(a.Resources))
	}
	if a.Resources[0].Name != "charges" || a.Resources[0].Kind != "collection" {
		t.Fatalf("resource[0] = %+v", a.Resources[0])
	}
	if a.Resources[0].Seed != "fixtures/charges.jsonl" {
		t.Fatalf("resource seed = %q", a.Resources[0].Seed)
	}

	// --- identity ---
	if a.Identity == nil || a.Identity.TokenScheme != "bearer" {
		t.Fatalf("Identity = %+v", a.Identity)
	}

	// --- top-level rules ---
	if len(a.Rules) != 1 {
		t.Fatalf("top-level rules: %d", len(a.Rules))
	}
	if a.Rules[0].Name != "catchall-404" {
		t.Fatalf("top-level rule name = %q", a.Rules[0].Name)
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	writeAdapter(t, dir, map[string]string{
		"adapter.yaml":          "id: test\n",
		"fixtures/charges.jsonl": "{\"id\":\"ch_1\"}\n",
	})
	a, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	data, err := a.ReadFile("fixtures/charges.jsonl")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "ch_1") {
		t.Fatalf("ReadFile content = %q", data)
	}
}

// --- I4: path traversal protection ---

func TestReadFileRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	writeAdapter(t, dir, map[string]string{
		"adapter.yaml": "id: test\n",
	})
	a, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	traversalPaths := []string{
		"../../etc/passwd",
		"../../../etc/shadow",
		"fixtures/../../../etc/passwd",
		"..\\..\\..\\etc\\passwd",
	}
	for _, p := range traversalPaths {
		_, err := a.ReadFile(p)
		if err == nil {
			t.Errorf("ReadFile(%q) should have been rejected", p)
		}
	}
}

func TestReadFileAllowsSubdirs(t *testing.T) {
	dir := t.TempDir()
	writeAdapter(t, dir, map[string]string{
		"adapter.yaml": "id: test\n",
		"templates/email.tmpl": "Hello {{.name}}\n",
	})
	a, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	data, err := a.ReadFile("templates/email.tmpl")
	if err != nil {
		t.Fatalf("ReadFile(subdir) should work: %v", err)
	}
	if !strings.Contains(string(data), "Hello") {
		t.Fatalf("unexpected content: %q", data)
	}
}

// --- error cases ---

func TestLoadMissingAdapterYAML(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for missing adapter.yaml")
	}
}

func TestLoadEndpointsDirWithoutAdapterYAML(t *testing.T) {
	dir := t.TempDir()
	writeAdapter(t, dir, map[string]string{
		// adapter.yaml intentionally absent
		"endpoints/charges.yaml": "route: /v1/charges\n",
	})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error when endpoints/ exists without adapter.yaml")
	}
}

func TestLoadMalformedYAML(t *testing.T) {
	dir := t.TempDir()
	writeAdapter(t, dir, map[string]string{
		"adapter.yaml": "id: [unclosed\n",
	})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for malformed yaml")
	}
}

func TestLoadEmptyID(t *testing.T) {
	dir := t.TempDir()
	writeAdapter(t, dir, map[string]string{
		"adapter.yaml": "name: Some Name\n",
	})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for empty ID")
	}
}
