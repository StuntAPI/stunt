package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stunt-adapters/stunt/internal/contrib"
	"github.com/stunt-adapters/stunt/internal/netutil"
)

func TestBuildDoctor_NoCA(t *testing.T) {
	dir := t.TempDir()
	r := BuildDoctor(dir, filepath.Join(dir, "stunt.yaml"))
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
	r := BuildDoctor(dir, filepath.Join(dir, "stunt.yaml"))
	if !r.CAExists {
		t.Error("CAExists should be true")
	}
	if r.CAError != "" {
		t.Errorf("CAError = %q, want empty", r.CAError)
	}
	if r.CADir != dir {
		t.Errorf("CADir = %q, want %q", r.CADir, dir)
	}
	_ = ca
}

func TestBuildDoctor_CorruptCA(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(filepath.Join(dir, "ca.pem"), "not a valid cert"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dir, "ca-key.pem"), "not a valid key"); err != nil {
		t.Fatal(err)
	}
	r := BuildDoctor(dir, filepath.Join(dir, "stunt.yaml"))
	if r.CAExists {
		t.Error("CAExists should be false for corrupt CA")
	}
	if r.CAError == "" {
		t.Error("CAError should be non-empty for corrupt CA")
	}
}

func TestPrintDoctor(t *testing.T) {
	dir := t.TempDir()
	r := BuildDoctor(dir, filepath.Join(dir, "stunt.yaml"))
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

// TestDoctorManifestChecks verifies that doctor reports manifest presence,
// parse validity, and service count.
func TestDoctorManifestChecks(t *testing.T) {
	dir := t.TempDir()

	// No manifest yet.
	r := BuildDoctor(caPath(dir), filepath.Join(dir, "stunt.yaml"))
	if r.ManifestFound {
		t.Error("ManifestFound should be false when no stunt.yaml exists")
	}

	// Write a valid manifest with a rules-only service.
	mPath := filepath.Join(dir, "stunt.yaml")
	content := `version: 1
network:
  mode: port
  base_port: 9100
services:
  api:
    rules:
      - match: { method: GET, path: /hello }
        respond: { status: 200 }
`
	if err := writeFile(mPath, content); err != nil {
		t.Fatal(err)
	}

	r = BuildDoctor(caPath(dir), mPath)
	if !r.ManifestFound {
		t.Error("ManifestFound should be true")
	}
	if r.ManifestError != "" {
		t.Errorf("ManifestError = %q, want empty", r.ManifestError)
	}
	if r.ServiceCount != 1 {
		t.Errorf("ServiceCount = %d, want 1", r.ServiceCount)
	}
	if len(r.ServiceChecks) != 1 {
		t.Fatalf("ServiceChecks = %d, want 1", len(r.ServiceChecks))
	}
	ds := r.ServiceChecks[0]
	if ds.Name != "api" {
		t.Errorf("service name = %q, want api", ds.Name)
	}
	if ds.AdapterSpec != "" {
		t.Errorf("rules-only service should have empty adapter spec")
	}
}

// TestDoctorAdapterLoadability verifies that doctor reports whether each
// service's adapter is loadable.
func TestDoctorAdapterLoadability(t *testing.T) {
	dir := t.TempDir()
	if err := contrib.Scaffold(dir, "myapi", contrib.ScaffoldOptions{}); err != nil {
		t.Fatal(err)
	}

	mPath := filepath.Join(dir, "stunt.yaml")
	content := `version: 1
network:
  mode: port
  base_port: 9200
services:
  good:
    adapter: ./myapi
  bad:
    adapter: ./nonexistent
`
	if err := writeFile(mPath, content); err != nil {
		t.Fatal(err)
	}

	r := BuildDoctor(caPath(dir), mPath)
	if r.ManifestError != "" {
		t.Fatalf("ManifestError = %q", r.ManifestError)
	}
	if len(r.ServiceChecks) != 2 {
		t.Fatalf("ServiceChecks = %d, want 2", len(r.ServiceChecks))
	}

	// "bad" comes before "good" alphabetically.
	bad := r.ServiceChecks[0]
	if bad.Name != "bad" {
		t.Fatalf("first service = %q, want bad", bad.Name)
	}
	if bad.AdapterOK {
		t.Error("bad adapter should not load")
	}
	if bad.AdapterError == "" {
		t.Error("bad adapter should have an error")
	}

	good := r.ServiceChecks[1]
	if good.Name != "good" {
		t.Fatalf("second service = %q, want good", good.Name)
	}
	if !good.AdapterOK {
		t.Errorf("good adapter should load, error: %s", good.AdapterError)
	}
}

// TestDoctorPortCheck verifies that doctor reports port status (free when
// nothing is listening, in-use when something is).
func TestDoctorPortCheck(t *testing.T) {
	dir := t.TempDir()
	mPath := filepath.Join(dir, "stunt.yaml")
	content := `version: 1
network:
  mode: port
  base_port: 9300
services:
  api:
    rules:
      - match: { method: GET, path: /hello }
        respond: { status: 200 }
`
	if err := writeFile(mPath, content); err != nil {
		t.Fatal(err)
	}

	// Without anything listening, port should be free.
	r := BuildDoctor(caPath(dir), mPath)
	if len(r.ServiceChecks) != 1 {
		t.Fatalf("ServiceChecks = %d, want 1", len(r.ServiceChecks))
	}
	if r.ServiceChecks[0].PortInUse {
		t.Error("port 9300 should be free")
	}
}

// TestPrintDoctorFullReport verifies the printed output contains all
// expected sections.
func TestPrintDoctorFullReport(t *testing.T) {
	dir := t.TempDir()
	if err := contrib.Scaffold(dir, "myapi", contrib.ScaffoldOptions{}); err != nil {
		t.Fatal(err)
	}
	mPath := filepath.Join(dir, "stunt.yaml")
	content := `version: 1
network:
  mode: port
  base_port: 9400
services:
  api:
    adapter: ./myapi
`
	if err := writeFile(mPath, content); err != nil {
		t.Fatal(err)
	}

	r := BuildDoctor(caPath(dir), mPath)
	var buf bytes.Buffer
	PrintDoctor(&buf, r)
	out := buf.String()

	for _, want := range []string{"platform:", "ca:", "manifest:", "services:", "adapter"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in doctor output:\n%s", want, out)
		}
	}
}

// writeFile is a test helper.
func writeFile(path, content string) error {
	return osWriteFile(path, []byte(content), 0o644)
}
