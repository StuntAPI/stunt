package sdkmap

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"stuntapi.com/stunt/internal/adapter"
)

// confDir is the conformance module root (tests live one level below it).
var confDir = ".."

// TestGoogleDiscoveryTables smoke-tests the discovery walker against the
// pinned google-api-go-client module. Skipped only when the module cache
// has not been populated (genmatrix itself still fails loud there).
func TestGoogleDiscoveryTables(t *testing.T) {
	if _, err := goModuleDir(confDir, "google.golang.org/api"); err != nil {
		t.Skipf("google module not downloaded: %v", err)
	}
	cases := []struct {
		service   string
		min       int
		knownReqs []string // "VERB path" strings that must be present
	}{
		{"gmail/v1", 70, []string{"GET /gmail/v1/users/{userId}/messages", "POST /gmail/v1/users/{userId}/messages/send"}},
		{"calendar/v3", 30, []string{"GET /calendar/v3/calendars/{calendarId}", "GET /calendar/v3/users/me/calendarList"}}, // old-style doc: no flatPath
		{"drive/v3", 50, []string{"GET /drive/v3/files/{fileId}", "DELETE /drive/v3/files/{fileId}"}},                      // version lives in baseUrl
		{"sheets/v4", 15, []string{"GET /v4/spreadsheets/{spreadsheetId}"}},
		{"admin/directory/v1", 100, []string{"GET /admin/directory/v1/users"}},
	}
	for _, c := range cases {
		routes, err := Extract("google", c.service, confDir)
		if err != nil {
			t.Errorf("%s: %v", c.service, err)
			continue
		}
		if len(routes) < c.min {
			t.Errorf("%s: %d routes, want >= %d", c.service, len(routes), c.min)
		}
		have := map[string]bool{}
		for _, r := range routes {
			have[r.Method+" "+r.Path] = true
		}
		for _, k := range c.knownReqs {
			if !have[k] {
				t.Errorf("%s: missing known route %q", c.service, k)
			}
		}
	}
}

// TestNodeTables smoke-tests every node walker against the installed
// node_modules, asserting the documented table sizes so an upstream layout
// change fails here instead of silently emptying the derived coverage.
func TestNodeTables(t *testing.T) {
	if _, err := os.Stat(nodeModulesDir(confDir)); err != nil {
		t.Skip("conformance/node/node_modules absent — run `just conformance-node` first")
	}
	cases := []struct {
		pkg       string
		min       int
		knownReqs []string
	}{
		{"@octokit/plugin-rest-endpoint-methods", 900, []string{"GET /repos/{owner}/{repo}", "POST /repos/{owner}/{repo}/issues"}},
		{"stripe", 150, []string{"GET /v1/charges", "POST /v1/charges", "GET /v1/charges/{id}"}},
		{"twilio", 100, []string{"GET /2010-04-01/Accounts/{accountSid}/Messages.json", "POST /2010-04-01/Accounts/{accountSid}/Messages.json", "DELETE /2010-04-01/Accounts/{accountSid}/Messages/{sid}.json"}},
		{"@slack/web-api", 200, []string{"POST /api/conversations.list", "POST /api/chat.postMessage"}},
		{"jira.js", 150, []string{"GET /rest/api/3/issue/{issueIdOrKey}", "POST /rest/api/3/issue"}},
		{"@hubspot/api-client", 400, []string{"GET /crm/v3/objects/contacts"}},
		{"plaid", 250, []string{"POST /accounts/balance/get", "POST /transactions/sync"}},
		{"square", 250, []string{"POST /v2/payments", "GET /v2/payments"}},
		{"openai", 50, []string{"POST /v1/chat/completions", "GET /v1/models"}},
		{"resend", 5, []string{"POST /emails"}},
	}
	for _, c := range cases {
		routes, err := Extract("node", c.pkg, confDir)
		if err != nil {
			t.Errorf("%s: %v", c.pkg, err)
			continue
		}
		if len(routes) < c.min {
			t.Errorf("%s: %d routes, want >= %d", c.pkg, len(routes), c.min)
		}
		have := map[string]bool{}
		for _, r := range routes {
			have[r.Method+" "+r.Path] = true
		}
		for _, k := range c.knownReqs {
			if !have[k] {
				t.Errorf("%s: missing known route %q", c.pkg, k)
			}
		}
	}
}

// TestGmailAdapterVsTable is the end-to-end check: the gmail adapter's
// manifest routes diffed against the real Gmail discovery table.
func TestGmailAdapterVsTable(t *testing.T) {
	if _, err := goModuleDir(confDir, "google.golang.org/api"); err != nil {
		t.Skipf("google module not downloaded: %v", err)
	}
	a, err := adapter.Load(filepath.Join("..", "..", "adapters", "gmail-style"))
	if err != nil {
		t.Fatal(err)
	}
	var adapterRoutes []AdapterRoute
	for _, ep := range a.Endpoints {
		adapterRoutes = append(adapterRoutes, AdapterRoute{Method: ep.Method, Path: ep.Route})
	}
	table, err := Extract("google", "gmail/v1", confDir)
	if err != nil {
		t.Fatal(err)
	}
	res := Diff(table, adapterRoutes)
	if res.Provider < 70 {
		t.Fatalf("table too small: %d", res.Provider)
	}
	if res.Covered < 12 {
		t.Errorf("only %d of %d gmail routes covered — manifest changed shape?", res.Covered, res.Provider)
	}
	have := map[string]bool{}
	for _, r := range table {
		if r.Method+" "+r.Path == "GET /gmail/v1/users/{userId}/messages" {
			have["messages.list"] = true
		}
	}
	if !have["messages.list"] {
		t.Error("table lacks messages.list")
	}
	fmt.Printf("gmail-style: %d/%d covered (%d missing)\n", res.Covered, res.Provider, len(res.Missing))
}
