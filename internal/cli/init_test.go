package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitWritesManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stunt.yaml")
	if err := writeSampleManifest(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Fatalf("manifest not written (stat err=%v)", err)
	}
	m, err := loadForInit(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if m.Network.Mode != "port" || len(m.Services) == 0 {
		t.Fatalf("unexpected manifest: %+v", m)
	}
}

// TestInitPrintsNextSteps verifies that `stunt init` prints next-step
// guidance (plan and up commands) after writing the manifest.
func TestInitPrintsNextSteps(t *testing.T) {
	dir := t.TempDir()
	mPath := filepath.Join(dir, "stunt.yaml")

	root := NewRootCmd()
	root.SetArgs([]string{"init", "--manifest", mPath})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if !strings.Contains(out, "wrote "+mPath) {
		t.Errorf("missing 'wrote' message in output:\n%s", out)
	}
	if !strings.Contains(out, "Next steps") {
		t.Errorf("missing 'Next steps' in output:\n%s", out)
	}
	if !strings.Contains(out, "stunt plan") {
		t.Errorf("missing 'stunt plan' hint:\n%s", out)
	}
	if !strings.Contains(out, "stunt up") {
		t.Errorf("missing 'stunt up' hint:\n%s", out)
	}
	if !strings.Contains(out, "http://127.0.0.1:8000") {
		t.Errorf("missing default URL hint:\n%s", out)
	}
}

// TestInitRuleOrderChanceFirst verifies that the probabilistic error rule
// comes BEFORE the unconditional success rule so it actually fires.
func TestInitRuleOrderChanceFirst(t *testing.T) {
	if !strings.Contains(sampleManifest, "occasional-error") {
		t.Fatal("sample manifest missing 'occasional-error' rule")
	}
	chanceIdx := strings.Index(sampleManifest, "occasional-error")
	successIdx := strings.Index(sampleManifest, "name: success")
	if chanceIdx < 0 || successIdx < 0 {
		t.Fatalf("missing rules in sample manifest (chance=%d, success=%d)", chanceIdx, successIdx)
	}
	if chanceIdx > successIdx {
		t.Errorf("probabilistic rule (occasional-error at %d) should come BEFORE success rule (at %d) — first-match-wins means an earlier unconditional rule shadows it", chanceIdx, successIdx)
	}
}
