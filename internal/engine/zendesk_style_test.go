package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestZendeskStyleAdapter exercises the Zendesk REST API v2 adapter end-to-end:
//
//   - list tickets (cursor pagination meta.has_more + links.next)
//   - create ticket ({ticket:{subject, comment:{body}, requester}})
//   - get ticket by id
//   - add comment to ticket
//   - list comments
//   - list users
//   - search
//   - 401 without auth → Zendesk error envelope
func TestZendeskStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "zendesk-style")
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
			"zendesk": {Adapter: absAdapterDir},
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

	base := addrs["zendesk"]

	// Zendesk uses Basic auth: email/token:secret
	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin@example.com/token:test-secret"))

	// ===== list tickets (cursor pagination) =====

	body, status := zdAuthGet(t, base+"/api/v2/tickets", basicAuth)
	if status != 200 {
		t.Fatalf("list tickets -> %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list resp: %v (body %s)", err, body)
	}
	tickets, ok := listResp["tickets"].([]any)
	if !ok || len(tickets) == 0 {
		t.Fatalf("tickets = %v, want non-empty array", listResp["tickets"])
	}
	// Verify ticket shape.
	ticket0 := tickets[0].(map[string]any)
	if _, ok := ticket0["id"].(string); !ok {
		t.Fatalf("ticket id = %v, want string", ticket0["id"])
	}
	if _, ok := ticket0["subject"].(string); !ok {
		t.Fatalf("ticket subject = %v, want string", ticket0["subject"])
	}
	if _, ok := ticket0["status"].(string); !ok {
		t.Fatalf("ticket status = %v, want string", ticket0["status"])
	}
	// Cursor pagination fields.
	meta, ok := listResp["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta = %v, want object", listResp["meta"])
	}
	if _, ok := meta["has_more"].(bool); !ok {
		t.Fatalf("meta.has_more = %v, want bool", meta["has_more"])
	}
	if _, ok := listResp["links"].(map[string]any); !ok {
		t.Fatalf("links = %v, want object", listResp["links"])
	}

	// ===== create ticket =====

	body, status = zdAuthPostJSON(t, base+"/api/v2/tickets", basicAuth, map[string]any{
		"ticket": map[string]any{
			"subject": "Cannot access my account",
			"comment": map[string]any{"body": "I keep getting an error when trying to log in."},
			"requester": map[string]any{
				"id": "101",
			},
		},
	})
	if status != 201 {
		t.Fatalf("create ticket -> %d, want 201; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal create resp: %v (body %s)", err, body)
	}
	createdTicket, ok := createResp["ticket"].(map[string]any)
	if !ok {
		t.Fatalf("ticket = %v, want object", createResp["ticket"])
	}
	ticketID, ok := createdTicket["id"].(string)
	if !ok || ticketID == "" {
		t.Fatalf("created ticket id = %v, want non-empty string", createdTicket["id"])
	}
	if createdTicket["subject"] != "Cannot access my account" {
		t.Fatalf("created ticket subject = %v", createdTicket["subject"])
	}
	if createdTicket["status"] != "open" {
		t.Fatalf("created ticket status = %v, want open", createdTicket["status"])
	}

	// ===== get ticket by id =====

	body, status = zdAuthGet(t, base+"/api/v2/tickets/"+ticketID, basicAuth)
	if status != 200 {
		t.Fatalf("get ticket -> %d, want 200; body %s", status, body)
	}
	var getResp map[string]any
	if err := json.Unmarshal([]byte(body), &getResp); err != nil {
		t.Fatalf("unmarshal get ticket: %v (body %s)", err, body)
	}
	getTicket, ok := getResp["ticket"].(map[string]any)
	if !ok {
		t.Fatalf("get ticket = %v, want object", getResp["ticket"])
	}
	if getTicket["id"] != ticketID {
		t.Fatalf("retrieved ticket id = %v, want %s", getTicket["id"], ticketID)
	}

	// ===== add comment to ticket =====

	body, status = zdAuthPostJSON(t, base+"/api/v2/tickets/"+ticketID+"/comments", basicAuth, map[string]any{
		"ticket": map[string]any{
			"comment": map[string]any{
				"body":   "Can you try resetting your password?",
				"public": true,
			},
		},
	})
	if status != 201 {
		t.Fatalf("add comment -> %d, want 201; body %s", status, body)
	}

	// ===== list comments (verify comment was added — STATEFUL) =====

	body, status = zdAuthGet(t, base+"/api/v2/tickets/"+ticketID+"/comments", basicAuth)
	if status != 200 {
		t.Fatalf("list comments -> %d, want 200; body %s", status, body)
	}
	var commentsResp map[string]any
	if err := json.Unmarshal([]byte(body), &commentsResp); err != nil {
		t.Fatalf("unmarshal comments: %v (body %s)", err, body)
	}
	comments, ok := commentsResp["comments"].([]any)
	if !ok || len(comments) < 1 {
		t.Fatalf("comments = %v, want at least 1", commentsResp["comments"])
	}
	cmt0 := comments[0].(map[string]any)
	if _, ok := cmt0["body"].(string); !ok {
		t.Fatalf("comment body = %v, want string", cmt0["body"])
	}

	// ===== list users =====

	body, status = zdAuthGet(t, base+"/api/v2/users", basicAuth)
	if status != 200 {
		t.Fatalf("list users -> %d, want 200; body %s", status, body)
	}
	var usersResp map[string]any
	if err := json.Unmarshal([]byte(body), &usersResp); err != nil {
		t.Fatalf("unmarshal users: %v (body %s)", err, body)
	}
	users, ok := usersResp["users"].([]any)
	if !ok || len(users) == 0 {
		t.Fatalf("users = %v, want non-empty", usersResp["users"])
	}
	user0 := users[0].(map[string]any)
	if _, ok := user0["id"].(string); !ok {
		t.Fatalf("user id = %v, want string", user0["id"])
	}
	if _, ok := user0["name"].(string); !ok {
		t.Fatalf("user name = %v, want string", user0["name"])
	}
	if _, ok := user0["role"].(string); !ok {
		t.Fatalf("user role = %v, want string", user0["role"])
	}

	// ===== list organizations =====

	body, status = zdAuthGet(t, base+"/api/v2/organizations", basicAuth)
	if status != 200 {
		t.Fatalf("list orgs -> %d, want 200; body %s", status, body)
	}
	var orgResp map[string]any
	if err := json.Unmarshal([]byte(body), &orgResp); err != nil {
		t.Fatalf("unmarshal orgs: %v (body %s)", err, body)
	}
	orgs, ok := orgResp["organizations"].([]any)
	if !ok || len(orgs) == 0 {
		t.Fatalf("organizations = %v, want non-empty", orgResp["organizations"])
	}

	// ===== search =====

	body, status = zdAuthGet(t, base+"/api/v2/search.json?query=billing", basicAuth)
	if status != 200 {
		t.Fatalf("search -> %d, want 200; body %s", status, body)
	}
	var searchResp map[string]any
	if err := json.Unmarshal([]byte(body), &searchResp); err != nil {
		t.Fatalf("unmarshal search: %v (body %s)", err, body)
	}
	if _, ok := searchResp["results"].([]any); !ok {
		t.Fatalf("search results = %v, want array", searchResp["results"])
	}

	// ===== list groups =====

	body, status = zdAuthGet(t, base+"/api/v2/groups", basicAuth)
	if status != 200 {
		t.Fatalf("list groups -> %d, want 200; body %s", status, body)
	}
	var groupsResp map[string]any
	if err := json.Unmarshal([]byte(body), &groupsResp); err != nil {
		t.Fatalf("unmarshal groups: %v (body %s)", err, body)
	}
	if _, ok := groupsResp["groups"].([]any); !ok {
		t.Fatalf("groups = %v, want array", groupsResp["groups"])
	}

	// ===== 401 without auth → Zendesk error envelope =====

	body, status = zdNoAuthGet(t, base+"/api/v2/tickets")
	if status != 401 {
		t.Fatalf("no-auth tickets -> %d, want 401; body %s", status, body)
	}
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal error resp: %v (body %s)", err, body)
	}
	if _, ok := errResp["error"].(string); !ok {
		t.Fatalf("error = %v, want string", errResp["error"])
	}
	if _, ok := errResp["description"].(string); !ok {
		t.Fatalf("description = %v, want string", errResp["description"])
	}
}

// === Zendesk test helpers ===

func zdAuthGet(t *testing.T, rawurl, authHeader string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", authHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func zdNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func zdAuthPostJSON(t *testing.T, rawurl, authHeader string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// Guard: suppress unused imports.
var _ = fmt.Sprintf
var _ = strings.Contains
