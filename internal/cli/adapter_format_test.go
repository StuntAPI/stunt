package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestAdapterAddRemovePreservesComments is the core test for the "adapter
// add/remove preserves manifest formatting" fix. It takes a manifest WITH
// comments + 2-space indent + flow-style mappings, runs `adapter add` then
// `adapter remove`, and asserts that comments and formatting are preserved.
//
// Known residual: yaml.v3's Node encoder normalises the *internal* whitespace
// of flow mappings ("{ a: b }" -> "{a: b}"), so a manifest using flow style
// is not byte-identical after a round-trip — but comments, indentation, and
// the flow-vs-block style ARE preserved (the original bug destroyed comments,
// changed indent 2->4, and expanded flow to block). Block-style manifests
// round-trip byte-identically.
func TestAdapterAddRemovePreservesComments(t *testing.T) {
	original := `# This is my stunt config
version: 1
rng_seed: 42
network:
  mode: port
  base_port: 8000
services:
  example:
    rules:
      # This rule handles /hello
      - match: { method: GET, path: /hello }
        respond: { status: 200, body: { inline: { message: hi } } }
`

	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "stunt.yaml")
	if err := os.WriteFile(manifestPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	cacheDir := t.TempDir()
	localDir := t.TempDir()

	// Add a service.
	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "temp-svc", false); err != nil {
		t.Fatalf("adapter add: %v", err)
	}

	// Remove the service we just added.
	if err := runAdapterRemove(&bytes.Buffer{}, manifestPath, "temp-svc"); err != nil {
		t.Fatalf("adapter remove: %v", err)
	}

	// Read the result.
	result, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	resultStr := string(result)

	// Comments must be preserved.
	if !strings.Contains(resultStr, "# This is my stunt config") {
		t.Errorf("top-level comment lost after add+remove:\n%s", resultStr)
	}
	if !strings.Contains(resultStr, "# This rule handles /hello") {
		t.Errorf("inline comment lost after add+remove:\n%s", resultStr)
	}

	// 2-space indent must be preserved (4-space indent = regression).
	for _, line := range strings.Split(resultStr, "\n") {
		// Lines that are part of the network block should have 2-space indent.
		if strings.HasPrefix(line, "    mode:") || strings.HasPrefix(line, "    base_port:") {
			t.Errorf("4-space indent detected (should be 2-space):\n%s", resultStr)
		}
	}

	// Flow-style mappings should be preserved (not expanded to block style).
	// yaml.v3 may normalize spacing inside braces ({key: val} vs { key: val }),
	// but the key thing is they stay on one line with curly braces.
	if !strings.Contains(resultStr, "match: {") {
		t.Errorf("flow-style mapping expanded to block style:\n%s", resultStr)
	}
	// Verify it's not expanded to multi-line block style.
	for _, line := range strings.Split(resultStr, "\n") {
		if strings.TrimSpace(line) == "method: GET" {
			t.Errorf("flow-style mapping was expanded to block style:\n%s", resultStr)
		}
	}
}

// TestAdapterAddWritesValidYAML verifies that the surgically-edited manifest
// is still valid YAML and the new service is present.
func TestAdapterAddWritesValidYAML(t *testing.T) {
	original := `# header comment
version: 1
network:
  mode: port
  base_port: 8000
services:
  existing:  # existing service
    rules:
      - match: { path: / }
        respond: { status: 200 }
`

	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "stunt.yaml")
	if err := os.WriteFile(manifestPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	cacheDir := t.TempDir()
	localDir := t.TempDir()

	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "new-svc", false); err != nil {
		t.Fatalf("adapter add: %v", err)
	}

	// Verify the new service is present and the existing one is untouched.
	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatalf("manifest parse after add: %v", err)
	}
	if _, ok := m.Services["existing"]; !ok {
		t.Error("existing service was lost")
	}
	if _, ok := m.Services["new-svc"]; !ok {
		t.Error("new service not added")
	}

	// Verify comment is preserved.
	result, _ := os.ReadFile(manifestPath)
	if !strings.Contains(string(result), "# header comment") {
		t.Errorf("header comment lost after add:\n%s", string(result))
	}
}

// TestAdapterRemovePreservesComments verifies that removing a service also
// preserves comments.
func TestAdapterRemovePreservesComments(t *testing.T) {
	original := `# my config
version: 1
network:
  mode: port
  base_port: 8000
services:
  keep:
    rules:
      - match: { path: / }
        respond: { status: 200 }
  remove-me:
    adapter: /some/path
`

	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "stunt.yaml")
	if err := os.WriteFile(manifestPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runAdapterRemove(&bytes.Buffer{}, manifestPath, "remove-me"); err != nil {
		t.Fatalf("adapter remove: %v", err)
	}

	result, _ := os.ReadFile(manifestPath)
	resultStr := string(result)

	if !strings.Contains(resultStr, "# my config") {
		t.Errorf("header comment lost after remove:\n%s", resultStr)
	}
	if strings.Contains(resultStr, "remove-me") {
		t.Errorf("service not removed:\n%s", resultStr)
	}
	if !strings.Contains(resultStr, "keep:") {
		t.Errorf("keep service was removed:\n%s", resultStr)
	}
}
