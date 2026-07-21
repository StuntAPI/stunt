package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/catalog"
)

func jsonMarshalCatalog(entries []catalog.Entry) ([]byte, error) {
	return json.Marshal(entries)
}

func handler200(data []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})
}

func writeCatalogTestServer(t *testing.T) (string, func()) {
	t.Helper()
	entries := []catalog.Entry{
		{Name: "stripe-style", Description: "Stripe-style payment API", GitURL: "https://github.com/stunt-adapters/stripe-style", LatestRef: "v1.0.0", Tags: []string{"payments"}},
		{Name: "github", Description: "GitHub API", GitURL: "https://github.com/stunt-adapters/github", LatestRef: "v1.0.0", Tags: []string{"devtools"}},
	}
	data, err := jsonMarshalCatalog(entries)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler200(data))
	return srv.URL, srv.Close
}

func TestCatalogSearchCmd(t *testing.T) {
	url, cleanup := writeCatalogTestServer(t)
	defer cleanup()

	var out bytes.Buffer
	if err := runCatalogSearch(&out, url, "stripe"); err != nil {
		t.Fatalf("runCatalogSearch: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "stripe-style") {
		t.Errorf("output should contain 'stripe-style': %q", s)
	}
	if !strings.Contains(s, "Stripe-style payment API") {
		t.Errorf("output should contain description: %q", s)
	}
	if !strings.Contains(s, "github.com/stunt-adapters/stripe-style") {
		t.Errorf("output should contain git URL: %q", s)
	}
}

func TestCatalogSearchCmdFallsBackToBundled(t *testing.T) {
	var out bytes.Buffer
	// Unreachable URL → falls back to bundled index, which has "stripe-style".
	if err := runCatalogSearch(&out, "http://127.0.0.1:1", "stripe"); err != nil {
		t.Fatalf("runCatalogSearch: %v", err)
	}
	if !strings.Contains(out.String(), "stripe-style") {
		t.Errorf("bundled fallback should contain 'stripe-style': %q", out.String())
	}
}

func TestCatalogShowCmd(t *testing.T) {
	url, cleanup := writeCatalogTestServer(t)
	defer cleanup()

	var out bytes.Buffer
	if err := runCatalogShow(&out, url, "stripe-style"); err != nil {
		t.Fatalf("runCatalogShow: %v", err)
	}
	s := out.String()
	for _, want := range []string{"stripe-style", "Stripe-style payment API", "v1.0.0", "payments", "github.com/stunt-adapters/stripe-style"} {
		if !strings.Contains(s, want) {
			t.Errorf("output should contain %q: %q", want, s)
		}
	}
	// Should include usage guidance.
	if !strings.Contains(s, "Usage:") {
		t.Errorf("output should contain 'Usage:' section: %q", s)
	}
	if !strings.Contains(s, "stunt adapter add stripe-style") {
		t.Errorf("output should suggest 'stunt adapter add stripe-style': %q", s)
	}
}

func TestCatalogShowCmdNotFound(t *testing.T) {
	url, cleanup := writeCatalogTestServer(t)
	defer cleanup()

	var out bytes.Buffer
	err := runCatalogShow(&out, url, "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown adapter")
	}
}

func TestCatalogSubcommandsRegistered(t *testing.T) {
	root := NewRootCmd()

	searchCmd, _, err := root.Find([]string{"catalog", "search"})
	if err != nil {
		t.Fatalf("could not find 'catalog search': %v", err)
	}
	if searchCmd.Name() != "search" {
		t.Fatalf("command name = %q, want %q", searchCmd.Name(), "search")
	}

	showCmd, _, err := root.Find([]string{"catalog", "show"})
	if err != nil {
		t.Fatalf("could not find 'catalog show': %v", err)
	}
	if showCmd.Name() != "show" {
		t.Fatalf("command name = %q, want %q", showCmd.Name(), "show")
	}
}

func TestCatalogParentCommandRegistered(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"catalog"})
	if err != nil {
		t.Fatalf("could not find 'catalog' command: %v", err)
	}
	if cmd.Name() != "catalog" {
		t.Fatalf("command name = %q, want %q", cmd.Name(), "catalog")
	}
}
