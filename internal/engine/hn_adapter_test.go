package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestHNStyleAdapter exercises the Hacker News Firebase-style reference
// adapter end-to-end through the full read + submit flow:
//
//   - topstories/newstories → array of ids
//   - GET item/<id>.json → item JSON with standard HN shape
//   - GET item/missing.json → literal "null" (Firebase convention)
//   - GET user/<id>.json → user JSON with karma/submitted
//   - login → 302 + Set-Cookie (session)
//   - submit (with cookie) → 302 redirect; new story appears in lists
//   - submit (no cookie) → error
func TestHNStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "hn-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"hn": {Adapter: absAdapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	base := addrs["hn"]

	// ===== Story lists =====

	// GET /v0/topstories.json → array of ints
	body, status := hnGet(t, base+"/v0/topstories.json")
	if status != 200 {
		t.Fatalf("topstories -> status %d, want 200; body %s", status, body)
	}
	var topIDs []any
	if err := json.Unmarshal([]byte(body), &topIDs); err != nil {
		t.Fatalf("unmarshal topstories: %v (body %s)", err, body)
	}
	if len(topIDs) == 0 {
		t.Fatal("topstories is empty, want seeded items")
	}

	// GET /v0/newstories.json → also returns stories
	body, status = hnGet(t, base+"/v0/newstories.json")
	if status != 200 {
		t.Fatalf("newstories -> status %d, want 200; body %s", status, body)
	}
	var newIDs []any
	if err := json.Unmarshal([]byte(body), &newIDs); err != nil {
		t.Fatalf("unmarshal newstories: %v (body %s)", err, body)
	}
	if len(newIDs) == 0 {
		t.Fatal("newstories is empty, want seeded items")
	}

	// ===== Item retrieval =====

	// GET /v0/item/1001.json → story item
	body, status = hnGet(t, base+"/v0/item/1001.json")
	if status != 200 {
		t.Fatalf("item 1001 -> status %d, want 200; body %s", status, body)
	}
	var item map[string]any
	if err := json.Unmarshal([]byte(body), &item); err != nil {
		t.Fatalf("unmarshal item 1001: %v (body %s)", err, body)
	}
	if item["id"] != float64(1001) {
		t.Fatalf("item id = %v, want 1001", item["id"])
	}
	if item["type"] != "story" {
		t.Fatalf("item type = %v, want story", item["type"])
	}
	if item["by"] != "alice" {
		t.Fatalf("item by = %v, want alice", item["by"])
	}
	if _, ok := item["title"].(string); !ok {
		t.Fatalf("item title = %v, want string", item["title"])
	}
	if _, ok := item["time"].(float64); !ok {
		t.Fatalf("item time = %v, want number", item["time"])
	}
	if _, ok := item["score"].(float64); !ok {
		t.Fatalf("item score = %v, want number", item["score"])
	}
	kids, ok := item["kids"].([]any)
	if !ok {
		t.Fatalf("item kids = %v, want array", item["kids"])
	}
	if len(kids) != 2 {
		t.Fatalf("item kids len = %d, want 2", len(kids))
	}

	// GET /v0/item/99999.json → literal "null" (Firebase convention)
	body, status = hnGet(t, base+"/v0/item/99999.json")
	if status != 200 {
		t.Fatalf("missing item -> status %d, want 200", status)
	}
	if strings.TrimSpace(body) != "null" {
		t.Fatalf("missing item body = %q, want 'null'", body)
	}

	// GET a comment item
	body, status = hnGet(t, base+"/v0/item/1002.json")
	if status != 200 {
		t.Fatalf("item 1002 -> status %d, want 200; body %s", status, body)
	}
	var comment map[string]any
	if err := json.Unmarshal([]byte(body), &comment); err != nil {
		t.Fatalf("unmarshal item 1002: %v (body %s)", err, body)
	}
	if comment["type"] != "comment" {
		t.Fatalf("comment type = %v, want comment", comment["type"])
	}
	if comment["parent"] != float64(1001) {
		t.Fatalf("comment parent = %v, want 1001", comment["parent"])
	}

	// ===== User retrieval =====

	// GET /v0/user/alice.json → user
	body, status = hnGet(t, base+"/v0/user/alice.json")
	if status != 200 {
		t.Fatalf("user alice -> status %d, want 200; body %s", status, body)
	}
	var user map[string]any
	if err := json.Unmarshal([]byte(body), &user); err != nil {
		t.Fatalf("unmarshal user alice: %v (body %s)", err, body)
	}
	if user["id"] != "alice" {
		t.Fatalf("user id = %v, want alice", user["id"])
	}
	if _, ok := user["karma"].(float64); !ok {
		t.Fatalf("user karma = %v, want number", user["karma"])
	}
	submitted, ok := user["submitted"].([]any)
	if !ok {
		t.Fatalf("user submitted = %v, want array", user["submitted"])
	}
	if len(submitted) == 0 {
		t.Fatal("user submitted is empty, want seeded items")
	}

	// GET /v0/user/nobody.json → literal "null"
	body, status = hnGet(t, base+"/v0/user/nobody.json")
	if status != 200 {
		t.Fatalf("missing user -> status %d, want 200", status)
	}
	if strings.TrimSpace(body) != "null" {
		t.Fatalf("missing user body = %q, want 'null'", body)
	}

	// ===== Login → session cookie =====

	// POST /login with form body → 302 + Set-Cookie (do NOT follow redirect)
	resp := hnPostFormNoRedirect(t, base+"/login", url.Values{
		"acct": {"testuser"},
		"pw":   {"secret"},
		"goto": {"news"},
	})
	if resp.StatusCode != 302 {
		t.Fatalf("login -> status %d, want 302", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "/news" {
		t.Fatalf("login Location = %q, want /news", resp.Header.Get("Location"))
	}
	setCookie := resp.Header.Get("Set-Cookie")
	if !strings.Contains(setCookie, "user=") {
		t.Fatalf("login Set-Cookie = %q, want user= cookie", setCookie)
	}
	sessionCookie := hnExtractCookie(setCookie, "user")
	if sessionCookie == "" {
		t.Fatal("login: no user cookie value extracted")
	}
	resp.Body.Close()

	// ===== Submit (with cookie) → 302; story appears in lists =====

	// POST /submit with cookie → 302 redirect
	resp = hnPostFormWithCookieNoRedirect(t, base+"/submit", sessionCookie, url.Values{
		"title": {"Show HN: Stunt adapter test"},
		"url":   {"https://example.test/stunt"},
	})
	if resp.StatusCode != 302 {
		t.Fatalf("submit -> status %d, want 302", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "/news" {
		t.Fatalf("submit Location = %q, want /news", resp.Header.Get("Location"))
	}
	resp.Body.Close()

	// The submitted story should appear in topstories.
	body, status = hnGet(t, base+"/v0/topstories.json")
	if status != 200 {
		t.Fatalf("topstories after submit -> status %d", status)
	}
	var topIDsAfter []any
	if err := json.Unmarshal([]byte(body), &topIDsAfter); err != nil {
		t.Fatalf("unmarshal topstories after submit: %v (body %s)", err, body)
	}
	if len(topIDsAfter) <= len(topIDs) {
		t.Fatalf("topstories did not grow after submit: before=%d after=%d", len(topIDs), len(topIDsAfter))
	}

	// ===== Submit (no cookie) → error =====

	resp = hnPostFormNoRedirect(t, base+"/submit", url.Values{
		"title": {"should fail"},
	})
	if resp.StatusCode == 302 {
		t.Fatal("submit without cookie -> 302, want non-redirect (error)")
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// ===== Catch-all 404 =====

	_, status = hnGet(t, base+"/v0/no-such-endpoint")
	if status != 404 {
		t.Fatalf("GET unmatched -> status %d, want 404", status)
	}
}

// === Helpers ===

// hnNoRedirectClient does not follow redirects (for 302 testing).
var hnNoRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func hnGet(t *testing.T, url string) (string, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func hnPostFormNoRedirect(t *testing.T, rawurl string, form url.Values) *http.Response {
	t.Helper()
	resp, err := hnNoRedirectClient.PostForm(rawurl, form)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func hnPostFormWithCookieNoRedirect(t *testing.T, rawurl, cookie string, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", "user="+cookie)
	resp, err := hnNoRedirectClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func hnExtractCookie(setCookie, name string) string {
	parts := strings.Split(setCookie, ";")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, name+"=") {
			return strings.TrimPrefix(p, name+"=")
		}
	}
	return ""
}
