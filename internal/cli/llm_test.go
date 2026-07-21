package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLLMReferenceCoversCriticalAPI verifies that `stunt llm` output (the
// in-binary reference) documents the surfaces an LLM needs to operate stunt:
// the command list, manifest fields, and the Starlark handler API. This is a
// regression guard: if a builtin or command is added but not documented here,
// an LLM operating the binary will be missing context.
func TestLLMReferenceCoversCriticalAPI(t *testing.T) {
	var out bytes.Buffer
	if _, err := runLLM(&out); err != nil {
		t.Fatalf("runLLM: %v", err)
	}
	s := out.String()

	// The reference must mention every public command.
	for _, want := range []string{"init", "plan", "up", "down", "demo", "doctor",
		"clean", "catalog", "adapter", "version", "trust", "hosts", "proxy"} {
		if !strings.Contains(s, want) {
			t.Errorf("llm reference should mention command %q", want)
		}
	}

	// The manifest schema.
	for _, want := range []string{"stunt.yaml", "services", "adapter", "rng_seed"} {
		if !strings.Contains(s, want) {
			t.Errorf("llm reference should document manifest field %q", want)
		}
	}

	// The complete Starlark builtins (the handler API). Every builtin an
	// adapter can call must be documented so an LLM authoring an adapter
	// knows the vocabulary.
	for _, want := range []string{
		"respond", "store_collection", "store_kv_set", "store_kv_get",
		"store_kv_incr", "store_blob", "identity_mint", "identity_validate",
		"events_register", "events_emit", "lib.star", "method,", "path,", "headers",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("llm reference should document builtin/concept %q", want)
		}
	}

	// It must tell the user how to get more detail.
	if !strings.Contains(s, "AGENTS.md") {
		t.Error("llm reference should point to AGENTS.md for the full guide")
	}
}

// TestCatalogSearchJSON verifies the --json flag emits valid JSON with the
// expected entry shape, so LLMs/scripts can consume catalog results reliably.
func TestCatalogSearchJSON(t *testing.T) {
	url, cleanup := writeCatalogTestServer(t)
	defer cleanup()
	var out bytes.Buffer
	if err := runCatalogSearch(&out, url, "stripe", true); err != nil {
		t.Fatalf("runCatalogSearch json: %v", err)
	}

	var got []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		GitURL      string   `json:"git_url"`
		LatestRef   string   `json:"latest_ref"`
		Tags        []string `json:"tags"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if len(got) != 1 || got[0].Name != "stripe-style" {
		t.Fatalf("got %+v, want one stripe-style entry", got)
	}
	if len(got[0].Tags) != 1 || got[0].Tags[0] != "payments" {
		t.Errorf("tags = %v, want [payments]", got[0].Tags)
	}
}

// TestPlanJSON verifies --json on plan emits valid, structured JSON an LLM
// can parse to learn service names + addresses.
func TestPlanJSON(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "stunt.yaml")
	if err := os.WriteFile(manifestPath, []byte(`version: 1
network: { mode: port, base_port: 19000 }
services:
  svc:
    rules:
      - { name: ok, match: { method: GET, path: / }, respond: { status: 200 } }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runPlan(&out, manifestPath, true); err != nil {
		t.Fatalf("runPlan json: %v", err)
	}
	var got planJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if !got.OK || got.Mode != "port" || len(got.Services) != 1 {
		t.Fatalf("got %+v, want ok/port/1-service", got)
	}
	if got.Services[0].Name != "svc" || got.Services[0].Address != "http://127.0.0.1:19000" {
		t.Errorf("service = %+v, want name=svc addr=http://127.0.0.1:19000", got.Services[0])
	}
}
