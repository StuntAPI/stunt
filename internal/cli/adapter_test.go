package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stunt-adapters/stunt/internal/adapter"
)

func TestNewAdapterNewCmd(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer

	if err := runAdapterNew(&out, dir, "my-api", false); err != nil {
		t.Fatalf("runAdapterNew: %v", err)
	}

	// Output should mention the adapter name and target.
	outStr := out.String()
	if !strings.Contains(outStr, "my-api") {
		t.Errorf("output should mention adapter name: %q", outStr)
	}

	// The adapter directory should be created.
	root := filepath.Join(dir, "my-api")
	if _, err := os.Stat(filepath.Join(root, "adapter.yaml")); err != nil {
		t.Errorf("adapter.yaml not created: %v", err)
	}

	// Round-trip via adapter.Load.
	a, err := adapter.Load(root)
	if err != nil {
		t.Fatalf("adapter.Load: %v", err)
	}
	if a.ID != "my-api" {
		t.Errorf("ID = %q, want %q", a.ID, "my-api")
	}
}

func TestNewAdapterNewCmdRefusesNonEmpty(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "taken")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "stale.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runAdapterNew(&out, dir, "taken", false)
	if err == nil {
		t.Fatal("expected error for non-empty dir")
	}
}

func TestNewAdapterNewCmdForce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "forced")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "stale.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterNew(&out, dir, "forced", true); err != nil {
		t.Fatalf("runAdapterNew with force: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "adapter.yaml")); err != nil {
		t.Errorf("adapter.yaml not created with force: %v", err)
	}
}

func TestAdapterImportOpenapiCmd(t *testing.T) {
	dir := t.TempDir()

	// Scaffold an adapter first.
	if err := runAdapterNew(&bytes.Buffer{}, dir, "myapi", false); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	adapterDir := filepath.Join(dir, "myapi")

	// Write a small OpenAPI spec.
	specPath := filepath.Join(dir, "spec.json")
	spec := `{"openapi":"3.0.0","paths":{"/users":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"type":"object","properties":{"name":{"type":"string"}}}}}}}}}}}`
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runImportOpenapi(&out, specPath, adapterDir); err != nil {
		t.Fatalf("runImportOpenapi: %v", err)
	}

	// Endpoint and template files should exist.
	if _, err := os.Stat(filepath.Join(adapterDir, "endpoints", "get_users.yaml")); err != nil {
		t.Errorf("endpoint file not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(adapterDir, "templates", "get_users.json")); err != nil {
		t.Errorf("template file not created: %v", err)
	}

	// adapter.yaml should load with the imported endpoint.
	a, err := adapter.Load(adapterDir)
	if err != nil {
		t.Fatalf("adapter.Load: %v", err)
	}
	found := false
	for _, ep := range a.Endpoints {
		if ep.Route == "/users" {
			found = true
			break
		}
	}
	if !found {
		t.Error("imported endpoint /users not found in adapter.yaml")
	}
}

func TestAdapterImportSubcommandRegistered(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"adapter", "import", "openapi"})
	if err != nil {
		t.Fatalf("could not find 'adapter import openapi': %v", err)
	}
	if cmd.Name() != "openapi" {
		t.Fatalf("command name = %q, want %q", cmd.Name(), "openapi")
	}
}

func TestAdapterImportHarCmd(t *testing.T) {
	dir := t.TempDir()

	// Scaffold an adapter first.
	if err := runAdapterNew(&bytes.Buffer{}, dir, "myapi", false); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	adapterDir := filepath.Join(dir, "myapi")

	// Write a minimal HAR file.
	harPath := filepath.Join(dir, "capture.har")
	harData := `{"log":{"entries":[{"request":{"method":"GET","url":"https://api.example.com/items"},"response":{"status":200,"content":{"mimeType":"application/json","text":"{\"name\":\"Real Product\"}"}}}]}}`
	if err := os.WriteFile(harPath, []byte(harData), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runImportHar(&out, harPath, adapterDir); err != nil {
		t.Fatalf("runImportHar: %v", err)
	}

	// Endpoint and template files should exist.
	if _, err := os.Stat(filepath.Join(adapterDir, "endpoints", "get_items.yaml")); err != nil {
		t.Errorf("endpoint file not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(adapterDir, "templates", "get_items.json")); err != nil {
		t.Errorf("template file not created: %v", err)
	}

	// Verify no real data leaked.
	tmpl, err := os.ReadFile(filepath.Join(adapterDir, "templates", "get_items.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(tmpl, []byte("Real Product")) {
		t.Error("real data leaked into template")
	}

	// adapter.yaml should load with the imported endpoint.
	a, err := adapter.Load(adapterDir)
	if err != nil {
		t.Fatalf("adapter.Load: %v", err)
	}
	found := false
	for _, ep := range a.Endpoints {
		if ep.Route == "/items" {
			found = true
			break
		}
	}
	if !found {
		t.Error("imported endpoint /items not found in adapter.yaml")
	}
}

func TestAdapterImportHarSubcommandRegistered(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"adapter", "import", "har"})
	if err != nil {
		t.Fatalf("could not find 'adapter import har': %v", err)
	}
	if cmd.Name() != "har" {
		t.Fatalf("command name = %q, want %q", cmd.Name(), "har")
	}
}

func TestAdapterParentCommandHasSubcommands(t *testing.T) {
	root := NewRootCmd()
	adapterCmd, _, err := root.Find([]string{"adapter"})
	if err != nil {
		t.Fatalf("could not find 'adapter' command: %v", err)
	}
	if adapterCmd.Name() != "adapter" {
		t.Fatalf("command name = %q, want %q", adapterCmd.Name(), "adapter")
	}

	// The "new" subcommand should be registered.
	newCmd, _, err := root.Find([]string{"adapter", "new"})
	if err != nil {
		t.Fatalf("could not find 'adapter new': %v", err)
	}
	if newCmd.Name() != "new" {
		t.Fatalf("subcommand name = %q, want %q", newCmd.Name(), "new")
	}
}

func TestAdapterLintSubcommandRegistered(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"adapter", "lint"})
	if err != nil {
		t.Fatalf("could not find 'adapter lint': %v", err)
	}
	if cmd.Name() != "lint" {
		t.Fatalf("command name = %q, want %q", cmd.Name(), "lint")
	}
}

func TestAdapterTestSubcommandRegistered(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"adapter", "test"})
	if err != nil {
		t.Fatalf("could not find 'adapter test': %v", err)
	}
	if cmd.Name() != "test" {
		t.Fatalf("command name = %q, want %q", cmd.Name(), "test")
	}
}

func TestRunAdapterTestHighScore(t *testing.T) {
	// Scaffold a clean adapter.
	root := filepath.Join(t.TempDir(), "conf")
	if err := runAdapterNew(&bytes.Buffer{}, filepath.Dir(root), filepath.Base(root), false); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	// Write traces that match the scaffolded /hello endpoint.
	tracesPath := filepath.Join(t.TempDir(), "traces.jsonl")
	traces := `{"request":{"method":"GET","path":"/hello"},"response":{"status":200,"body":{"message":"hello from stunt"}}}` + "\n"
	if err := os.WriteFile(tracesPath, []byte(traces), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterTest(&out, root, tracesPath, false); err != nil {
		t.Fatalf("runAdapterTest: %v", err)
	}
	outStr := out.String()
	if !strings.Contains(outStr, "100%") {
		t.Errorf("expected 100%% score, got: %q", outStr)
	}
}

func TestRunAdapterTestLowScore(t *testing.T) {
	// Scaffold a clean adapter.
	root := filepath.Join(t.TempDir(), "conf")
	if err := runAdapterNew(&bytes.Buffer{}, filepath.Dir(root), filepath.Base(root), false); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	// Write a trace that hits an unimplemented endpoint.
	tracesPath := filepath.Join(t.TempDir(), "traces.jsonl")
	traces := `{"request":{"method":"GET","path":"/missing"},"response":{"status":200,"body":{"data":"ok"}}}` + "\n"
	if err := os.WriteFile(tracesPath, []byte(traces), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterTest(&out, root, tracesPath, false); err != nil {
		t.Fatalf("runAdapterTest: %v", err)
	}
	outStr := out.String()
	if !strings.Contains(outStr, "0%") {
		t.Errorf("expected 0%% score, got: %q", outStr)
	}
}

func TestRunAdapterLintClean(t *testing.T) {
	// Scaffold a clean adapter.
	root := filepath.Join(t.TempDir(), "clean")
	if err := runAdapterNew(&bytes.Buffer{}, filepath.Dir(root), filepath.Base(root), false); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	var out bytes.Buffer
	if err := runAdapterLint(&out, root); err != nil {
		t.Fatalf("runAdapterLint on clean adapter: %v", err)
	}
	outStr := out.String()
	if !strings.Contains(outStr, "no findings") {
		t.Errorf("expected 'no findings' message, got: %q", outStr)
	}
}

func TestRunAdapterLintDirty(t *testing.T) {
	// Scaffold a clean adapter then inject a real-looking email.
	root := filepath.Join(t.TempDir(), "dirty")
	if err := runAdapterNew(&bytes.Buffer{}, filepath.Dir(root), filepath.Base(root), false); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "fixtures", "leaked.jsonl"),
		[]byte(`{"email":"john.doe@acme-corp.com"}`+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runAdapterLint(&out, root)
	if err == nil {
		t.Fatal("expected error from lint on dirty adapter, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "real data") {
		t.Errorf("error should mention real data: %v", err)
	}
	outStr := out.String()
	if !strings.Contains(outStr, "email") {
		t.Errorf("output should mention the email finding: %q", outStr)
	}
}

func TestAdapterNewRequiresName(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"adapter", "new"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for 'adapter new' without name")
	}
}

