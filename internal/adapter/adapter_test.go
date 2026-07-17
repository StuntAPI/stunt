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
		t.Fatal("expected error for empty id")
	}
}

// --- gRPC spec ---

const grpcAdapterYAML = `
id: greeter
name: Greeter
endpoints:
  - route: /health
    method: GET
    handler: scripts/greeter.star#on_health
grpc:
  service: stunt.test.Greeter
  descriptor: schemas/greeter.desc
  methods:
    - name: SayHello
      handler: scripts/greeter.star#on_say_hello
`

func TestLoadGrpcSpec(t *testing.T) {
	dir := t.TempDir()
	writeAdapter(t, dir, map[string]string{
		"adapter.yaml":           grpcAdapterYAML,
		"scripts/greeter.star":   "def on_say_hello(req):\n    return respond(200, {})\n",
		"schemas/greeter.desc":   "\x0a\x00", // minimal placeholder
	})

	a, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if a.Grpc == nil {
		t.Fatal("Grpc is nil")
	}
	if a.Grpc.Service != "stunt.test.Greeter" {
		t.Errorf("Service = %q", a.Grpc.Service)
	}

	// Descriptor path should be resolved to absolute.
	if !filepath.IsAbs(a.Grpc.Descriptor) {
		t.Errorf("Descriptor not absolute: %q", a.Grpc.Descriptor)
	}
	if !strings.HasSuffix(a.Grpc.Descriptor, "schemas/greeter.desc") {
		t.Errorf("Descriptor path unexpected: %q", a.Grpc.Descriptor)
	}

	// Method handler paths should be resolved to absolute.
	if len(a.Grpc.Methods) != 1 {
		t.Fatalf("Methods: %d, want 1", len(a.Grpc.Methods))
	}
	m := a.Grpc.Methods[0]
	if m.Name != "SayHello" {
		t.Errorf("method name = %q", m.Name)
	}
	scriptPath := strings.SplitN(m.Handler, "#", 2)[0]
	if !filepath.IsAbs(scriptPath) {
		t.Errorf("handler script not absolute: %q", m.Handler)
	}
	if _, err := os.Stat(scriptPath); err != nil {
		t.Errorf("handler script does not exist: %v", err)
	}
	if !strings.HasSuffix(m.Handler, "#on_say_hello") {
		t.Errorf("handler missing #on_say_hello: %q", m.Handler)
	}
}

func TestLoadGrpcValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		err  string
	}{
		{
			name: "missing service",
			yaml: `id: g
name: G
grpc:
  descriptor: schemas/x.desc
  methods:
    - name: M
      handler: scripts/x.star#f
`,
			err: "grpc.service",
		},
		{
			name: "missing descriptor",
			yaml: `id: g
name: G
grpc:
  service: pkg.Svc
  methods:
    - name: M
      handler: scripts/x.star#f
`,
			err: "grpc.descriptor",
		},
		{
			name: "missing method name",
			yaml: `id: g
name: G
grpc:
  service: pkg.Svc
  descriptor: schemas/x.desc
  methods:
    - handler: scripts/x.star#f
`,
			err: "grpc.methods[0].name",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeAdapter(t, dir, map[string]string{"adapter.yaml": c.yaml})
			_, err := Load(dir)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.err) {
				t.Errorf("error %q does not contain %q", err.Error(), c.err)
			}
		})
	}
}

func TestDescriptorBytes(t *testing.T) {
	dir := t.TempDir()
	writeAdapter(t, dir, map[string]string{
		"adapter.yaml":         grpcAdapterYAML,
		"scripts/greeter.star": "def on_say_hello(req):\n    return respond(200, {})\n",
		"schemas/greeter.desc": "descriptor-bytes",
	})

	a, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	data, err := a.DescriptorBytes()
	if err != nil {
		t.Fatalf("DescriptorBytes: %v", err)
	}
	if string(data) != "descriptor-bytes" {
		t.Errorf("got %q, want %q", data, "descriptor-bytes")
	}
}

func TestDescriptorBytesNoGrpc(t *testing.T) {
	dir := t.TempDir()
	writeAdapter(t, dir, map[string]string{
		"adapter.yaml": "id: plain\nname: Plain\n",
	})

	a, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	_, err = a.DescriptorBytes()
	if err == nil {
		t.Fatal("expected error when no grpc configured")
	}
}

// TestDescriptorRejectsTraversal verifies that a gRPC descriptor path using
// ".." traversal is rejected by Load (and DescriptorBytes) with the same
// containment check used by ReadFile.
func TestDescriptorRejectsTraversal(t *testing.T) {
	// Create a secret file outside the adapter directory.
	parent := t.TempDir()
	secretPath := filepath.Join(parent, "secret.desc")
	if err := os.WriteFile(secretPath, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// Adapter dir is a subdirectory of parent.
	adapterDir := filepath.Join(parent, "adapter")
	writeAdapter(t, adapterDir, map[string]string{
		"adapter.yaml": `
id: traversal-test
name: Traversal Test
grpc:
  service: pkg.Svc
  descriptor: ../secret.desc
  methods:
    - name: M
      handler: scripts/x.star#f
`,
		"scripts/x.star": "def f(req):\n    return respond(200, {})\n",
	})

	_, err := Load(adapterDir)
	if err == nil {
		t.Fatal("expected Load to reject descriptor path with .. traversal")
	}
	if !strings.Contains(err.Error(), "escapes adapter directory") {
		t.Errorf("error should mention directory escape, got: %v", err)
	}
}

// TestHandlerScriptRejectsTraversal verifies that a handler script path using
// ".." traversal is rejected by Load with the same containment check.
func TestHandlerScriptRejectsTraversal(t *testing.T) {
	parent := t.TempDir()
	secretPath := filepath.Join(parent, "evil.star")
	if err := os.WriteFile(secretPath, []byte("def f(req):\n    return respond(200, {})\n"), 0o644); err != nil {
		t.Fatalf("write evil.star: %v", err)
	}

	adapterDir := filepath.Join(parent, "adapter")
	writeAdapter(t, adapterDir, map[string]string{
		"adapter.yaml": `
id: traversal-test
name: Traversal Test
endpoints:
  - route: /x
    handler: ../evil.star#f
`,
	})

	_, err := Load(adapterDir)
	if err == nil {
		t.Fatal("expected Load to reject handler script path with .. traversal")
	}
	if !strings.Contains(err.Error(), "escapes adapter directory") {
		t.Errorf("error should mention directory escape, got: %v", err)
	}
}
