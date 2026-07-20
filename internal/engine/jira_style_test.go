package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestJiraStyleAdapter exercises the Jira-style adapter end-to-end:
//
//   - Basic auth (email:api_token) → myself endpoint
//   - serverInfo
//   - list projects
//   - create issue → {id, key, self}
//   - search (JQL) → shows created issue (STATEFUL)
//   - GET issue by key
//   - transition issue (status workflow)
//   - add comment
//   - PUT update issue
//   - 401 without auth → Jira error envelope
func TestJiraStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "jira-style")
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
			"jira": {Adapter: absAdapterDir},
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

	base := addrs["jira"]

	// ===== myself with Basic auth =====

	body, status := jiraBasicGet(t, base+"/rest/api/3/myself", "test@example.com", "test-api-token")
	if status != 200 {
		t.Fatalf("myself -> %d, want 200; body %s", status, body)
	}
	var myself map[string]any
	if err := json.Unmarshal([]byte(body), &myself); err != nil {
		t.Fatalf("unmarshal myself: %v (body %s)", err, body)
	}
	accountID, ok := myself["accountId"].(string)
	if !ok || accountID == "" {
		t.Fatalf("accountId = %v, want non-empty", myself["accountId"])
	}
	if !strings.HasPrefix(accountID, "5") {
		t.Fatalf("accountId = %q, want '5'-prefixed (Atlassian format)", accountID)
	}
	if _, ok := myself["displayName"].(string); !ok {
		t.Fatalf("displayName = %v, want string", myself["displayName"])
	}
	if myself["active"] != true {
		t.Fatalf("active = %v, want true", myself["active"])
	}

	// ===== serverInfo =====

	body, status = jiraBasicGet(t, base+"/rest/api/3/serverInfo", "test@example.com", "test-api-token")
	if status != 200 {
		t.Fatalf("serverInfo -> %d, want 200; body %s", status, body)
	}
	var serverInfo map[string]any
	if err := json.Unmarshal([]byte(body), &serverInfo); err != nil {
		t.Fatalf("unmarshal serverInfo: %v (body %s)", err, body)
	}
	if _, ok := serverInfo["version"].(string); !ok {
		t.Fatalf("version = %v, want string", serverInfo["version"])
	}

	// ===== list projects =====

	body, status = jiraBasicGet(t, base+"/rest/api/3/project", "test@example.com", "test-api-token")
	if status != 200 {
		t.Fatalf("projects -> %d, want 200; body %s", status, body)
	}
	var projects []any
	if err := json.Unmarshal([]byte(body), &projects); err != nil {
		t.Fatalf("unmarshal projects: %v (body %s)", err, body)
	}
	if len(projects) == 0 {
		t.Fatal("projects empty, want at least 1")
	}
	proj0 := projects[0].(map[string]any)
	projectKey, _ := proj0["key"].(string)
	if projectKey == "" {
		t.Fatalf("project key = %v, want non-empty", proj0["key"])
	}

	// ===== create issue =====

	body, status = jiraBasicPostJSON(t, base+"/rest/api/3/issue", "test@example.com", "test-api-token", map[string]any{
		"fields": map[string]any{
			"project":   map[string]any{"key": projectKey},
			"summary":   "Fix login page styling",
			"issuetype": map[string]any{"name": "Task"},
		},
	})
	if status != 201 {
		t.Fatalf("create issue -> %d, want 201; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal create resp: %v (body %s)", err, body)
	}
	issueKey, ok := createResp["key"].(string)
	if !ok || issueKey == "" {
		t.Fatalf("key = %v, want non-empty", createResp["key"])
	}
	if !strings.HasPrefix(issueKey, projectKey+"-") {
		t.Fatalf("issue key = %q, want %s-prefixed", issueKey, projectKey)
	}
	issueID, ok := createResp["id"].(string)
	if !ok || issueID == "" {
		t.Fatalf("id = %v, want non-empty", createResp["id"])
	}
	if _, ok := createResp["self"].(string); !ok {
		t.Fatalf("self = %v, want string", createResp["self"])
	}

	// ===== search (JQL) → shows created issue =====

	body, status = jiraBasicGet(t, base+"/rest/api/3/search?jql="+
		url.QueryEscape("project = "+projectKey), "test@example.com", "test-api-token")
	if status != 200 {
		t.Fatalf("search -> %d, want 200; body %s", status, body)
	}
	var searchResp map[string]any
	if err := json.Unmarshal([]byte(body), &searchResp); err != nil {
		t.Fatalf("unmarshal search resp: %v (body %s)", err, body)
	}
	issues, ok := searchResp["issues"].([]any)
	if !ok || len(issues) < 1 {
		t.Fatalf("issues = %v, want at least 1", searchResp["issues"])
	}
	total, ok := searchResp["total"].(float64)
	if !ok || int(total) < 1 {
		t.Fatalf("total = %v, want >= 1", searchResp["total"])
	}
	// Verify the issue structure.
	issue0 := issues[0].(map[string]any)
	if _, ok := issue0["key"].(string); !ok {
		t.Fatalf("issue key = %v, want string", issue0["key"])
	}
	fields, ok := issue0["fields"].(map[string]any)
	if !ok {
		t.Fatalf("issue fields = %v, want object", issue0["fields"])
	}
	if _, ok := fields["summary"].(string); !ok {
		t.Fatalf("summary = %v, want string", fields["summary"])
	}
	issueStatus, ok := fields["status"].(map[string]any)
	if !ok {
		t.Fatalf("status = %v, want object", fields["status"])
	}
	if _, ok := issueStatus["name"].(string); !ok {
		t.Fatalf("status name = %v, want string", issueStatus["name"])
	}

	// ===== GET issue by key =====

	body, status = jiraBasicGet(t, base+"/rest/api/3/issue/"+issueKey, "test@example.com", "test-api-token")
	if status != 200 {
		t.Fatalf("get issue -> %d, want 200; body %s", status, body)
	}
	var issue map[string]any
	if err := json.Unmarshal([]byte(body), &issue); err != nil {
		t.Fatalf("unmarshal issue: %v (body %s)", err, body)
	}
	if issue["key"] != issueKey {
		t.Fatalf("retrieved key = %v, want %s", issue["key"], issueKey)
	}

	// ===== transitions: list available =====

	body, status = jiraBasicGet(t, base+"/rest/api/3/issue/"+issueKey+"/transitions", "test@example.com", "test-api-token")
	if status != 200 {
		t.Fatalf("list transitions -> %d, want 200; body %s", status, body)
	}
	var transResp map[string]any
	if err := json.Unmarshal([]byte(body), &transResp); err != nil {
		t.Fatalf("unmarshal transitions: %v (body %s)", err, body)
	}
	transitions, ok := transResp["transitions"].([]any)
	if !ok || len(transitions) == 0 {
		t.Fatalf("transitions = %v, want non-empty", transResp["transitions"])
	}
	trans0 := transitions[0].(map[string]any)
	doneTransitionID, _ := trans0["id"].(string)
	if doneTransitionID == "" {
		t.Fatalf("transition id = %v, want non-empty", trans0["id"])
	}

	// ===== transition issue (status workflow) =====

	body, status = jiraBasicPostJSON(t, base+"/rest/api/3/issue/"+issueKey+"/transitions", "test@example.com", "test-api-token", map[string]any{
		"transition": map[string]any{"id": doneTransitionID},
	})
	if status != 204 {
		t.Fatalf("transition -> %d, want 204; body %s", status, body)
	}

	// Verify the status changed.
	body, status = jiraBasicGet(t, base+"/rest/api/3/issue/"+issueKey, "test@example.com", "test-api-token")
	if status != 200 {
		t.Fatalf("get issue after transition -> %d, want 200; body %s", status, body)
	}
	var updatedIssue map[string]any
	if err := json.Unmarshal([]byte(body), &updatedIssue); err != nil {
		t.Fatalf("unmarshal updated issue: %v (body %s)", err, body)
	}
	updatedFields := updatedIssue["fields"].(map[string]any)
	updatedStatus := updatedFields["status"].(map[string]any)
	if updatedStatus["name"] == "To Do" || updatedStatus["name"] == "Open" {
		t.Fatalf("status after transition = %v, expected changed from initial", updatedStatus["name"])
	}

	// ===== add comment =====

	commentBody := "This looks good, ship it."
	body, status = jiraBasicPostJSON(t, base+"/rest/api/3/issue/"+issueKey+"/comment", "test@example.com", "test-api-token", map[string]any{
		"body": commentBody,
	})
	if status != 201 {
		t.Fatalf("add comment -> %d, want 201; body %s", status, body)
	}
	var commentResp map[string]any
	if err := json.Unmarshal([]byte(body), &commentResp); err != nil {
		t.Fatalf("unmarshal comment resp: %v (body %s)", err, body)
	}
	if _, ok := commentResp["id"].(string); !ok {
		t.Fatalf("comment id = %v, want string", commentResp["id"])
	}

	// ===== pagination: startAt/maxResults =====

	body, status = jiraBasicGet(t, base+"/rest/api/3/search?jql="+
		url.QueryEscape("project = "+projectKey)+"&startAt=0&maxResults=1", "test@example.com", "test-api-token")
	if status != 200 {
		t.Fatalf("search with pagination -> %d, want 200; body %s", status, body)
	}
	var pageResp map[string]any
	if err := json.Unmarshal([]byte(body), &pageResp); err != nil {
		t.Fatalf("unmarshal page resp: %v (body %s)", err, body)
	}
	if pageResp["maxResults"] != float64(1) {
		t.Fatalf("maxResults = %v, want 1", pageResp["maxResults"])
	}

	// ===== update issue (PUT) =====

	body, status = jiraBasicPutJSON(t, base+"/rest/api/3/issue/"+issueKey, "test@example.com", "test-api-token", map[string]any{
		"fields": map[string]any{
			"summary": "Fix login page styling (updated)",
		},
	})
	if status != 204 {
		t.Fatalf("update issue -> %d, want 204; body %s", status, body)
	}
	// Verify update.
	body, status = jiraBasicGet(t, base+"/rest/api/3/issue/"+issueKey, "test@example.com", "test-api-token")
	if status != 200 {
		t.Fatalf("get after update -> %d, want 200; body %s", status, body)
	}
	var putUpdated map[string]any
	if err := json.Unmarshal([]byte(body), &putUpdated); err != nil {
		t.Fatalf("unmarshal after put: %v (body %s)", err, body)
	}
	putFields := putUpdated["fields"].(map[string]any)
	if putFields["summary"] != "Fix login page styling (updated)" {
		t.Fatalf("updated summary = %v, want 'Fix login page styling (updated)'", putFields["summary"])
	}

	_ = issueID // issueID used for diagnostics

	// ===== Bearer token also works for auth =====

	body, status = jiraBearerGet(t, base+"/rest/api/3/myself", "mock-pat-token")
	if status != 200 {
		t.Fatalf("myself with bearer -> %d, want 200; body %s", status, body)
	}

	// ===== 401 without auth → Jira error envelope =====

	body, status = jiraNoAuthGet(t, base+"/rest/api/3/search?jql="+url.QueryEscape("project = "+projectKey))
	if status != 401 {
		t.Fatalf("no-auth search -> %d, want 401; body %s", status, body)
	}
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal error resp: %v (body %s)", err, body)
	}
	if errMessages, ok := errResp["errorMessages"].([]any); !ok || len(errMessages) == 0 {
		t.Fatalf("errorMessages = %v, want non-empty array", errResp["errorMessages"])
	}
}

// === Jira test helpers ===

func jiraBasicGet(t *testing.T, rawurl, email, apiToken string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(email+":"+apiToken)))
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func jiraBearerGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func jiraNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func jiraBasicPostJSON(t *testing.T, rawurl, email, apiToken string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(email+":"+apiToken)))
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func jiraBasicPutJSON(t *testing.T, rawurl, email, apiToken string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("PUT", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(email+":"+apiToken)))
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
