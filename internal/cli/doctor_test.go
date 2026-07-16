package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stunt-adapters/stunt/internal/netutil"
)

func TestBuildDoctor_NoCA(t *testing.T) {
	dir := t.TempDir()
	r := BuildDoctor(dir)
	if r.CAExists {
		t.Error("CAExists should be false for empty dir")
	}
	if r.CAError != "" {
		t.Errorf("CAError = %q, want empty", r.CAError)
	}
	if r.Platform == "" {
		t.Error("Platform should not be empty")
	}
}

func TestBuildDoctor_WithCA(t *testing.T) {
	dir := t.TempDir()
	ca, err := netutil.EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := BuildDoctor(dir)
	if !r.CAExists {
		t.Error("CAExists should be true")
	}
	if r.CAError != "" {
		t.Errorf("CAError = %q, want empty", r.CAError)
	}
	// Report dir should match
	if r.CADir != dir {
		t.Errorf("CADir = %q, want %q", r.CADir, dir)
	}
	_ = ca
}

func TestBuildDoctor_CorruptCA(t *testing.T) {
	dir := t.TempDir()
	// Write invalid cert/key files.
	if err := writeFile(filepath.Join(dir, "ca.pem"), "not a valid cert"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dir, "ca-key.pem"), "not a valid key"); err != nil {
		t.Fatal(err)
	}
	r := BuildDoctor(dir)
	if r.CAExists {
		t.Error("CAExists should be false for corrupt CA")
	}
	if r.CAError == "" {
		t.Error("CAError should be non-empty for corrupt CA")
	}
}

func TestPrintDoctor(t *testing.T) {
	dir := t.TempDir()
	r := BuildDoctor(dir)
	var buf bytes.Buffer
	PrintDoctor(&buf, r)
	out := buf.String()
	if !strings.Contains(out, "platform:") {
		t.Errorf("missing 'platform:' in output:\n%s", out)
	}
	if !strings.Contains(out, "ca:") {
		t.Errorf("missing 'ca:' in output:\n%s", out)
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("should show 'not found' for missing CA:\n%s", out)
	}
}

// writeFile is a test helper.
func writeFile(path, content string) error {
	return osWriteFile(path, []byte(content), 0o644)
}
