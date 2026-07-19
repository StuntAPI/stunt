package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlanSubdomainShowsHTTPForTLSFalse verifies that `stunt plan` shows
// http:// URLs when the manifest has tls: false (dog1 finding "tls: false
// silently ignored").
func TestPlanSubdomainShowsHTTPForTLSFalse(t *testing.T) {
	dir := t.TempDir()
	mPath := filepath.Join(dir, "stunt.yaml")
	content := `version: 1
network:
  mode: subdomain
  tld: localhost
  tls: false
services:
  example:
    rules:
      - match: { path: /hello }
        respond: { status: 200 }
`
	if err := os.WriteFile(mPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetArgs([]string{"plan", "--manifest", mPath})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("plan failed: %v", err)
	}

	out := buf.String()
	// Should show http:// (not https://) because tls: false.
	if strings.Contains(out, "https://") {
		t.Errorf("plan should show http:// for tls:false, got https://. Output:\n%s", out)
	}
	if !strings.Contains(out, "http://example.localhost") {
		t.Errorf("plan should show http:// URL. Output:\n%s", out)
	}
}

// TestPlanSubdomainShowsHTTPSForTLSOmitted verifies that `stunt plan` shows
// https:// URLs when tls is omitted (default is TLS on).
func TestPlanSubdomainShowsHTTPSForTLSOmitted(t *testing.T) {
	dir := t.TempDir()
	mPath := filepath.Join(dir, "stunt.yaml")
	content := `version: 1
network:
  mode: subdomain
  tld: localhost
services:
  example:
    rules:
      - match: { path: /hello }
        respond: { status: 200 }
`
	if err := os.WriteFile(mPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetArgs([]string{"plan", "--manifest", mPath})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("plan failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "https://example.localhost") {
		t.Errorf("plan should show https:// for omitted tls (default on). Output:\n%s", out)
	}
}

// TestPlanSubdomainShowsHTTPSForTLSTrue verifies that `stunt plan` shows
// https:// URLs when tls is explicitly true.
func TestPlanSubdomainShowsHTTPSForTLSTrue(t *testing.T) {
	dir := t.TempDir()
	mPath := filepath.Join(dir, "stunt.yaml")
	content := `version: 1
network:
  mode: subdomain
  tld: localhost
  tls: true
services:
  example:
    rules:
      - match: { path: /hello }
        respond: { status: 200 }
`
	if err := os.WriteFile(mPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetArgs([]string{"plan", "--manifest", mPath})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("plan failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "https://example.localhost") {
		t.Errorf("plan should show https:// for tls:true. Output:\n%s", out)
	}
}
