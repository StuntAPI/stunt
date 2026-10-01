package engine

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/rules"
)

// Real S3 sends user-metadata headers lowercase on the wire
// (x-amz-meta-<suffix, lowercased>). The AWS SDK for .NET preserves the
// suffix case after stripping the prefix, so a canonicalized wire form
// (X-Amz-Meta-Kind) surfaces as "Kind" instead of "kind".
func TestHeaderCaseApplyDecisionPreservesLowercase(t *testing.T) {
	d := rules.Decision{
		Matched:   true,
		Status:    200,
		Headers:   map[string]string{"x-amz-meta-kind": "sample"},
		BodyBytes: []byte(`{}`),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	applyDecision(rec, req, d)

	h := rec.Header()
	vals, ok := h["x-amz-meta-kind"]
	if !ok {
		t.Fatalf("expected exact lowercase key %q in header map, got %v", "x-amz-meta-kind", h)
	}
	if len(vals) == 0 || vals[0] != "sample" {
		t.Fatalf("x-amz-meta-kind = %v, want %q", vals, "sample")
	}
	if _, ok := h["X-Amz-Meta-Kind"]; ok {
		t.Fatalf("header map must not contain canonicalized key %q, got %v", "X-Amz-Meta-Kind", h)
	}
}

func TestHeaderCaseRunHandlerPreservesLowercase(t *testing.T) {
	adapterDir := t.TempDir()
	scriptsDir := filepath.Join(adapterDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	adapterYAML := "id: test-header-case\n" +
		"name: header case test\n" +
		"version: \"0.1.0\"\n" +
		"endpoints:\n" +
		"  - route: /obj\n" +
		"    method: GET\n" +
		"    handler: scripts/h.star#on_get\n"
	if err := os.WriteFile(filepath.Join(adapterDir, "adapter.yaml"), []byte(adapterYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	handlerSrc := "def on_get(req):\n" +
		"    return respond(200, {\"ok\": True}, {\"x-amz-meta-kind\": \"sample\"})\n"
	if err := os.WriteFile(filepath.Join(scriptsDir, "h.star"), []byte(handlerSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"svc": {Adapter: adapterDir},
		},
	}
	e, err := newEngine(m, t.TempDir())
	if err != nil {
		t.Fatalf("newEngine: %v", err)
	}
	defer e.Close()

	st, ok := e.states["svc"]
	if !ok {
		t.Fatalf("service state missing: loadErrors=%v", e.loadErrors)
	}
	ep := st.adapter.Endpoints[0]

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/obj", nil)
	e.runHandler(rec, req, st, ep, nil, map[string]string{})

	h := rec.Header()
	vals, ok := h["x-amz-meta-kind"]
	if !ok {
		t.Fatalf("expected exact lowercase key %q in header map, got %v", "x-amz-meta-kind", h)
	}
	if len(vals) == 0 || vals[0] != "sample" {
		t.Fatalf("x-amz-meta-kind = %v, want %q", vals, "sample")
	}
	if _, ok := h["X-Amz-Meta-Kind"]; ok {
		t.Fatalf("header map must not contain canonicalized key %q, got %v", "X-Amz-Meta-Kind", h)
	}
}
