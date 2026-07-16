package contrib

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stunt-adapters/stunt/internal/adapter"
)

// expectedFiles lists every file the scaffold must create (relative to the
// adapter directory).
var expectedFiles = []string{
	"adapter.yaml",
	"README.md",
	"endpoints/hello.yaml",
	"templates/hello.json",
	"fixtures/seed.jsonl",
	"scripts/hello.star",
	"schemas/hello.schema.json",
}

func TestScaffoldCreatesFileTree(t *testing.T) {
	dir := t.TempDir()
	name := "my-api"

	if err := Scaffold(dir, name, ScaffoldOptions{}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	root := filepath.Join(dir, name)
	for _, rel := range expectedFiles {
		full := filepath.Join(root, rel)
		info, err := os.Stat(full)
		if err != nil {
			t.Errorf("expected file %s: %v", rel, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("file %s is empty", rel)
		}
	}
}

func TestScaffoldAdapterYAMLParsesViaLoad(t *testing.T) {
	dir := t.TempDir()
	name := "demo-svc"

	if err := Scaffold(dir, name, ScaffoldOptions{}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	root := filepath.Join(dir, name)
	a, err := adapter.Load(root)
	if err != nil {
		t.Fatalf("adapter.Load: %v", err)
	}

	// Metadata.
	if a.ID != name {
		t.Errorf("ID = %q, want %q", a.ID, name)
	}
	if a.Name == "" {
		t.Error("Name is empty")
	}
	if a.Version == "" {
		t.Error("Version is empty")
	}

	// At least one endpoint with a rule.
	if len(a.Endpoints) < 1 {
		t.Fatalf("Endpoints: %d, want >= 1", len(a.Endpoints))
	}
	ep := a.Endpoints[0]
	if ep.Route == "" || ep.Method == "" {
		t.Errorf("endpoint[0] missing route/method: %+v", ep)
	}
	if len(ep.Rules) == 0 {
		t.Error("endpoint[0] has no rules")
	}

	// Resources: at least one collection.
	if len(a.Resources) < 1 {
		t.Fatalf("Resources: %d, want >= 1", len(a.Resources))
	}
	r := a.Resources[0]
	if r.Kind != "collection" {
		t.Errorf("resource[0] kind = %q, want %q", r.Kind, "collection")
	}
	if r.Seed == "" {
		t.Error("resource[0] has no seed")
	}

	// Identity placeholder.
	if a.Identity == nil {
		t.Fatal("Identity is nil")
	}
	if a.Identity.TokenScheme == "" {
		t.Error("Identity.TokenScheme is empty")
	}

	// Top-level catch-all rule.
	if len(a.Rules) == 0 {
		t.Fatal("top-level rules is empty")
	}
	found404 := false
	for _, rule := range a.Rules {
		if rule.Respond.Status == 404 {
			found404 = true
			break
		}
	}
	if !found404 {
		t.Error("no top-level 404 catch-all rule found")
	}
}

func TestScaffoldSeedJSONLIsValid(t *testing.T) {
	dir := t.TempDir()
	name := "test-api"

	if err := Scaffold(dir, name, ScaffoldOptions{}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	seedPath := filepath.Join(dir, name, "fixtures", "seed.jsonl")
	data, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatalf("read seed.jsonl: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("seed.jsonl is empty")
	}
	for i, line := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Errorf("seed.jsonl line %d is not valid JSON: %v", i+1, err)
		}
	}
}

func TestScaffoldRefusesNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	name := "existing"

	// Pre-create the target with a file inside.
	target := filepath.Join(dir, name)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "oops.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Scaffold(dir, name, ScaffoldOptions{})
	if err == nil {
		t.Fatal("expected error for non-empty dir, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "exist") && !strings.Contains(strings.ToLower(err.Error()), "not empty") {
		t.Errorf("error message should mention existing/not-empty: %v", err)
	}
}

func TestScaffoldForceOverwritesNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	name := "forceable"

	// Pre-create the target with a file inside.
	target := filepath.Join(dir, name)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "stale.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Scaffold(dir, name, ScaffoldOptions{Force: true}); err != nil {
		t.Fatalf("Scaffold with Force: %v", err)
	}

	// The scaffolded files should be present.
	if _, err := os.Stat(filepath.Join(target, "adapter.yaml")); err != nil {
		t.Errorf("adapter.yaml not created: %v", err)
	}
	// The pre-existing file should still be there (we don't wipe the dir).
	if _, err := os.Stat(filepath.Join(target, "stale.txt")); err != nil {
		t.Errorf("stale.txt should still exist (force writes over, does not wipe): %v", err)
	}
}

func TestScaffoldEmptyExistingDirIsOK(t *testing.T) {
	dir := t.TempDir()
	name := "empty-ok"

	// Pre-create the target as an empty directory.
	target := filepath.Join(dir, name)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	// No error — empty dir is fine.
	if err := Scaffold(dir, name, ScaffoldOptions{}); err != nil {
		t.Fatalf("Scaffold into empty dir: %v", err)
	}
}

func TestScaffoldTemplateContainsFakerAndUUID(t *testing.T) {
	dir := t.TempDir()
	name := "tmpl-check"

	if err := Scaffold(dir, name, ScaffoldOptions{}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	tmplPath := filepath.Join(dir, name, "templates", "hello.json")
	data, err := os.ReadFile(tmplPath)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "faker") {
		t.Error("template should contain a faker reference")
	}
	if !strings.Contains(s, "uuid") {
		t.Error("template should contain a uuid reference")
	}
}

func TestScaffoldScriptReferencesStoreCollection(t *testing.T) {
	dir := t.TempDir()
	name := "script-check"

	if err := Scaffold(dir, name, ScaffoldOptions{}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	scriptPath := filepath.Join(dir, name, "scripts", "hello.star")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	if !strings.Contains(string(data), "store_collection") {
		t.Error("script should reference store_collection")
	}
}
