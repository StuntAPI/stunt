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

func TestAdapterNewRequiresName(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"adapter", "new"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for 'adapter new' without name")
	}
}

