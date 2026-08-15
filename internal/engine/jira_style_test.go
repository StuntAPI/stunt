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

	"stuntapi.com/stunt/internal/manifest"
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

func jiraBasicDelete(t *testing.T, rawurl, email, apiToken string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
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

// TestJiraStyleAdapterFidelity exercises the P2 fidelity surface:
//
//   - create stores + returns the real field set (description, priority,
//     labels, assignee, issuetype, custom fields)
//   - unknown/unsettable fields are rejected per-field with 400
//   - JQL: IN, !=, ~ contains, IS NOT EMPTY, AND/OR precedence, ORDER BY,
//     startAt/maxResults paging; unparseable JQL -> 400
//   - transitions are workflow-constrained: GET returns only the allowed
//     transitions for the current status; POST to a disallowed target is
//     400; Done sets resolution, Reopen clears it
//   - comments: list (paged), update, delete
func TestJiraStyleAdapterFidelity(t *testing.T) {
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
	const email = "test@example.com"
	const token = "test-api-token"

	// ===== create issue with the full field set =====

	richFields := map[string]any{
		"project":     map[string]any{"key": "TEST"},
		"summary":     "Fidelity probe issue",
		"description": "A rich description with details.",
		"issuetype":   map[string]any{"name": "Bug"},
		"priority":    map[string]any{"name": "High"},
		"labels":      []any{"backend", "fidelity"},
		"assignee":    map[string]any{"accountId": "5f1b3a4c5d6e7f8a9b0c1d2e"},
		"reporter":    map[string]any{"accountId": "5f1b3a4c5d6e7f8a9b0c1d2e"},
		"customfield_10010": map[string]any{
			"value": "Custom value",
		},
	}
	body, status := jiraBasicPostJSON(t, base+"/rest/api/3/issue", email, token, map[string]any{
		"fields": richFields,
	})
	if status != 201 {
		t.Fatalf("create rich issue -> %d, want 201; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal create resp: %v (body %s)", err, body)
	}
	richKey, _ := createResp["key"].(string)
	if richKey == "" {
		t.Fatalf("key = %v, want non-empty", createResp["key"])
	}

	// GET it back and verify the stored field set round-trips.
	body, status = jiraBasicGet(t, base+"/rest/api/3/issue/"+richKey, email, token)
	if status != 200 {
		t.Fatalf("get rich issue -> %d, want 200; body %s", status, body)
	}
	var richIssue map[string]any
	if err := json.Unmarshal([]byte(body), &richIssue); err != nil {
		t.Fatalf("unmarshal rich issue: %v (body %s)", err, body)
	}
	richFieldsMap := richIssue["fields"].(map[string]any)
	if richFieldsMap["description"] != "A rich description with details." {
		t.Fatalf("description = %v, want stored value", richFieldsMap["description"])
	}
	priority := richFieldsMap["priority"].(map[string]any)
	if priority["name"] != "High" {
		t.Fatalf("priority name = %v, want High", priority["name"])
	}
	labels, ok := richFieldsMap["labels"].([]any)
	if !ok || len(labels) != 2 {
		t.Fatalf("labels = %v, want [backend fidelity]", richFieldsMap["labels"])
	}
	issueType := richFieldsMap["issuetype"].(map[string]any)
	if issueType["name"] != "Bug" {
		t.Fatalf("issuetype name = %v, want Bug", issueType["name"])
	}
	assignee := richFieldsMap["assignee"].(map[string]any)
	if assignee["accountId"] != "5f1b3a4c5d6e7f8a9b0c1d2e" {
		t.Fatalf("assignee accountId = %v, want the stored account", assignee["accountId"])
	}
	custom := richFieldsMap["customfield_10010"].(map[string]any)
	if custom["value"] != "Custom value" {
		t.Fatalf("customfield_10010 = %v, want preserved", richFieldsMap["customfield_10010"])
	}
	richStatus := richFieldsMap["status"].(map[string]any)
	if richStatus["name"] != "To Do" {
		t.Fatalf("new issue status = %v, want To Do", richStatus["name"])
	}

	// ===== unknown field is rejected per-field =====

	body, status = jiraBasicPostJSON(t, base+"/rest/api/3/issue", email, token, map[string]any{
		"fields": map[string]any{
			"project":              map[string]any{"key": "TEST"},
			"summary":              "Should be rejected",
			"issuetype":            map[string]any{"name": "Task"},
			"bogusUnsettableField": "nope",
		},
	})
	if status != 400 {
		t.Fatalf("create with unknown field -> %d, want 400; body %s", status, body)
	}
	var fieldErr map[string]any
	if err := json.Unmarshal([]byte(body), &fieldErr); err != nil {
		t.Fatalf("unmarshal field error: %v (body %s)", err, body)
	}
	fieldErrors, ok := fieldErr["errors"].(map[string]any)
	if !ok {
		t.Fatalf("errors = %v, want object", fieldErr["errors"])
	}
	if msg, ok := fieldErrors["bogusUnsettableField"].(string); !ok || !strings.Contains(msg, "cannot be set") {
		t.Fatalf("errors[bogusUnsettableField] = %v, want 'cannot be set' message", fieldErrors["bogusUnsettableField"])
	}

	// PUT with an unknown field is rejected too.
	body, status = jiraBasicPutJSON(t, base+"/rest/api/3/issue/"+richKey, email, token, map[string]any{
		"fields": map[string]any{"alsoBogus": 1},
	})
	if status != 400 {
		t.Fatalf("update with unknown field -> %d, want 400; body %s", status, body)
	}

	// ===== JQL: seed sortable issues =====

	probeSummaries := []string{"zz-orderprobe alpha", "zz-orderprobe beta", "zz-orderprobe gamma"}
	probeKeys := []string{}
	for _, s := range probeSummaries {
		body, status = jiraBasicPostJSON(t, base+"/rest/api/3/issue", email, token, map[string]any{
			"fields": map[string]any{
				"project":   map[string]any{"key": "TEST"},
				"summary":   s,
				"issuetype": map[string]any{"name": "Task"},
				"priority":  map[string]any{"name": "High"},
				"labels":    []any{"fidelity"},
			},
		})
		if status != 201 {
			t.Fatalf("create probe issue %q -> %d, want 201; body %s", s, status, body)
		}
		var pr map[string]any
		if err := json.Unmarshal([]byte(body), &pr); err != nil {
			t.Fatalf("unmarshal probe create: %v (body %s)", err, body)
		}
		k, _ := pr["key"].(string)
		probeKeys = append(probeKeys, k)
	}

	search := func(jql string) map[string]any {
		t.Helper()
		b, code := jiraBasicGet(t, base+"/rest/api/3/search?jql="+url.QueryEscape(jql), email, token)
		if code != 200 {
			t.Fatalf("search %q -> %d, want 200; body %s", jql, code, b)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(b), &out); err != nil {
			t.Fatalf("unmarshal search %q: %v (body %s)", jql, err, b)
		}
		return out
	}

	// status IN (...) — the probes are To Do, the rich issue is To Do too,
	// the seed TEST-1 is In Progress (excluded).
	res := search(`project = TEST AND status IN ("To Do", "Done")`)
	if res["total"] != float64(4) {
		t.Fatalf("status IN total = %v, want 4", res["total"])
	}

	// ~ contains + != + IS NOT EMPTY.
	res = search(`summary ~ "zz-orderprobe" AND assignee IS NOT EMPTY`)
	if res["total"] != float64(0) {
		t.Fatalf("probes have no assignee: total = %v, want 0", res["total"])
	}
	res = search(`summary ~ "orderprobe"`)
	if res["total"] != float64(3) {
		t.Fatalf("~ contains total = %v, want 3", res["total"])
	}
	res = search(`project = TEST AND status != Done`)
	if res["total"] != float64(5) {
		t.Fatalf("status != Done total = %v, want 5 (seed + rich + 3 probes)", res["total"])
	}
	res = search(`labels = fidelity`)
	if res["total"] != float64(4) {
		t.Fatalf("labels = fidelity total = %v, want 4 (rich + probes)", res["total"])
	}

	// AND/OR precedence: AND binds tighter than OR.
	res = search(`summary ~ "nomatch-word" OR summary ~ "zz-orderprobe" AND priority = High`)
	if res["total"] != float64(3) {
		t.Fatalf("OR precedence total = %v, want 3", res["total"])
	}
	res = search(`summary ~ "zz-orderprobe" AND priority = Medium OR summary ~ "nomatch-word"`)
	if res["total"] != float64(0) {
		t.Fatalf("OR precedence (no match) total = %v, want 0", res["total"])
	}

	// ORDER BY summary DESC / ASC.
	res = search(`summary ~ "zz-orderprobe" ORDER BY summary DESC`)
	issues := res["issues"].([]any)
	if len(issues) != 3 {
		t.Fatalf("ORDER BY issues = %d, want 3", len(issues))
	}
	first := issues[0].(map[string]any)["fields"].(map[string]any)["summary"]
	if first != "zz-orderprobe gamma" {
		t.Fatalf("ORDER BY summary DESC first = %v, want zz-orderprobe gamma", first)
	}
	res = search(`summary ~ "zz-orderprobe" ORDER BY summary ASC`)
	issues = res["issues"].([]any)
	first = issues[0].(map[string]any)["fields"].(map[string]any)["summary"]
	if first != "zz-orderprobe alpha" {
		t.Fatalf("ORDER BY summary ASC first = %v, want zz-orderprobe alpha", first)
	}

	// startAt/maxResults paging.
	b, code := jiraBasicGet(t, base+"/rest/api/3/search?jql="+url.QueryEscape(`summary ~ "zz-orderprobe" ORDER BY summary ASC`)+"&startAt=1&maxResults=2", email, token)
	if code != 200 {
		t.Fatalf("paged search -> %d, want 200; body %s", code, b)
	}
	var page map[string]any
	if err := json.Unmarshal([]byte(b), &page); err != nil {
		t.Fatalf("unmarshal paged search: %v (body %s)", err, b)
	}
	if page["startAt"] != float64(1) || page["maxResults"] != float64(2) || page["total"] != float64(3) {
		t.Fatalf("page envelope = %v/%v/%v, want 1/2/3", page["startAt"], page["maxResults"], page["total"])
	}
	if len(page["issues"].([]any)) != 2 {
		t.Fatalf("paged issues = %d, want 2", len(page["issues"].([]any)))
	}

	// Unparseable JQL -> 400 with Jira's error envelope.
	for _, bad := range []string{`summary =`, `summary ~~ x`, `bogusfield = 1`, `summary ~ "unbalanced`, `project = TEST AND`} {
		b, code = jiraBasicGet(t, base+"/rest/api/3/search?jql="+url.QueryEscape(bad), email, token)
		if code != 400 {
			t.Fatalf("bad JQL %q -> %d, want 400; body %s", bad, code, b)
		}
		var badResp map[string]any
		if err := json.Unmarshal([]byte(b), &badResp); err != nil {
			t.Fatalf("unmarshal bad JQL resp: %v (body %s)", err, b)
		}
		if msgs, ok := badResp["errorMessages"].([]any); !ok || len(msgs) == 0 {
			t.Fatalf("bad JQL %q errorMessages = %v, want non-empty", bad, badResp["errorMessages"])
		}
	}

	// ===== transitions are workflow-constrained =====

	// From To Do: only In Progress (21) and Done (31) are available.
	body, status = jiraBasicGet(t, base+"/rest/api/3/issue/"+richKey+"/transitions", email, token)
	if status != 200 {
		t.Fatalf("list transitions -> %d, want 200; body %s", status, body)
	}
	var transResp map[string]any
	if err := json.Unmarshal([]byte(body), &transResp); err != nil {
		t.Fatalf("unmarshal transitions: %v (body %s)", err, body)
	}
	allowed := map[string]bool{}
	for _, tv := range transResp["transitions"].([]any) {
		tr := tv.(map[string]any)
		allowed[tr["id"].(string)] = true
		to := tr["to"].(map[string]any)
		if to["name"] == nil {
			t.Fatalf("transition %v missing to.name", tr["id"])
		}
	}
	if !allowed["21"] || !allowed["31"] {
		t.Fatalf("transitions from To Do = %v, want 21 and 31", allowed)
	}
	if allowed["11"] || allowed["41"] {
		t.Fatalf("transitions from To Do = %v, must not include 11/41", allowed)
	}

	// Disallowed target (Reopen from To Do) -> 400.
	body, status = jiraBasicPostJSON(t, base+"/rest/api/3/issue/"+richKey+"/transitions", email, token, map[string]any{
		"transition": map[string]any{"id": "41"},
	})
	if status != 400 {
		t.Fatalf("disallowed transition -> %d, want 400; body %s", status, body)
	}
	// Unknown transition id -> 400.
	body, status = jiraBasicPostJSON(t, base+"/rest/api/3/issue/"+richKey+"/transitions", email, token, map[string]any{
		"transition": map[string]any{"id": "999"},
	})
	if status != 400 {
		t.Fatalf("unknown transition -> %d, want 400; body %s", status, body)
	}

	// To Do -> In Progress -> Done, then verify status id/name and resolution.
	transition := func(id string) {
		t.Helper()
		b, code := jiraBasicPostJSON(t, base+"/rest/api/3/issue/"+richKey+"/transitions", email, token, map[string]any{
			"transition": map[string]any{"id": id},
		})
		if code != 204 {
			t.Fatalf("transition %s -> %d, want 204; body %s", id, code, b)
		}
	}
	issueStatus := func() (string, string, any) {
		t.Helper()
		b, code := jiraBasicGet(t, base+"/rest/api/3/issue/"+richKey, email, token)
		if code != 200 {
			t.Fatalf("get issue -> %d, want 200; body %s", code, b)
		}
		var iss map[string]any
		if err := json.Unmarshal([]byte(b), &iss); err != nil {
			t.Fatalf("unmarshal issue: %v (body %s)", err, b)
		}
		f := iss["fields"].(map[string]any)
		st := f["status"].(map[string]any)
		name, _ := st["name"].(string)
		id, _ := st["id"].(string)
		return name, id, f["resolution"]
	}

	transition("21")
	name, id, resolution := issueStatus()
	if name != "In Progress" || id != "21" {
		t.Fatalf("after 21: status = %v/%v, want In Progress/21", name, id)
	}
	if resolution != nil {
		t.Fatalf("after 21: resolution = %v, want nil", resolution)
	}

	// From In Progress: Done (31) and Stop Progress (11) are available.
	body, status = jiraBasicGet(t, base+"/rest/api/3/issue/"+richKey+"/transitions", email, token)
	if status != 200 {
		t.Fatalf("list transitions (In Progress) -> %d, want 200; body %s", status, body)
	}
	transResp = map[string]any{}
	if err := json.Unmarshal([]byte(body), &transResp); err != nil {
		t.Fatalf("unmarshal transitions: %v (body %s)", err, body)
	}
	allowed = map[string]bool{}
	for _, tv := range transResp["transitions"].([]any) {
		allowed[tv.(map[string]any)["id"].(string)] = true
	}
	if !allowed["31"] || !allowed["11"] || len(allowed) != 2 {
		t.Fatalf("transitions from In Progress = %v, want exactly 31 and 11", allowed)
	}
	// Disallowed from In Progress: Reopen.
	body, status = jiraBasicPostJSON(t, base+"/rest/api/3/issue/"+richKey+"/transitions", email, token, map[string]any{
		"transition": map[string]any{"id": "41"},
	})
	if status != 400 {
		t.Fatalf("reopen from In Progress -> %d, want 400; body %s", status, body)
	}

	transition("31")
	name, id, resolution = issueStatus()
	if name != "Done" || id != "31" {
		t.Fatalf("after 31: status = %v/%v, want Done/31", name, id)
	}
	resMap, ok := resolution.(map[string]any)
	if !ok || resMap["name"] != "Done" {
		t.Fatalf("after Done transition: resolution = %v, want {name: Done}", resolution)
	}

	// From Done: only Reopen (41); reopening clears the resolution.
	body, status = jiraBasicGet(t, base+"/rest/api/3/issue/"+richKey+"/transitions", email, token)
	if status != 200 {
		t.Fatalf("list transitions (Done) -> %d, want 200; body %s", status, body)
	}
	transResp = map[string]any{}
	if err := json.Unmarshal([]byte(body), &transResp); err != nil {
		t.Fatalf("unmarshal transitions: %v (body %s)", err, body)
	}
	allowed = map[string]bool{}
	for _, tv := range transResp["transitions"].([]any) {
		allowed[tv.(map[string]any)["id"].(string)] = true
	}
	if len(allowed) != 1 || !allowed["41"] {
		t.Fatalf("transitions from Done = %v, want only 41", allowed)
	}

	transition("41")
	name, id, resolution = issueStatus()
	if name != "Reopened" || id != "41" {
		t.Fatalf("after 41: status = %v/%v, want Reopened/41", name, id)
	}
	if resolution != nil {
		t.Fatalf("after reopen: resolution = %v, want cleared", resolution)
	}

	// ===== comments: list, update, delete =====

	commentIDs := []string{}
	for _, c := range []string{"First comment", "Second comment"} {
		body, status = jiraBasicPostJSON(t, base+"/rest/api/3/issue/"+richKey+"/comment", email, token, map[string]any{
			"body": c,
		})
		if status != 201 {
			t.Fatalf("add comment -> %d, want 201; body %s", status, body)
		}
		var cr map[string]any
		if err := json.Unmarshal([]byte(body), &cr); err != nil {
			t.Fatalf("unmarshal comment: %v (body %s)", err, body)
		}
		cid, _ := cr["id"].(string)
		if cid == "" {
			t.Fatalf("comment id = %v, want non-empty", cr["id"])
		}
		commentIDs = append(commentIDs, cid)
	}

	// List (paged envelope).
	body, status = jiraBasicGet(t, base+"/rest/api/3/issue/"+richKey+"/comment", email, token)
	if status != 200 {
		t.Fatalf("list comments -> %d, want 200; body %s", status, body)
	}
	var commentsResp map[string]any
	if err := json.Unmarshal([]byte(body), &commentsResp); err != nil {
		t.Fatalf("unmarshal comments: %v (body %s)", err, body)
	}
	if commentsResp["total"] != float64(2) {
		t.Fatalf("comments total = %v, want 2", commentsResp["total"])
	}
	if _, ok := commentsResp["_issue"]; ok {
		t.Fatal("comments response leaks the internal _issue key")
	}

	// Paging.
	body, status = jiraBasicGet(t, base+"/rest/api/3/issue/"+richKey+"/comment?startAt=1&maxResults=1", email, token)
	if status != 200 {
		t.Fatalf("paged comments -> %d, want 200; body %s", status, body)
	}
	commentsResp = map[string]any{}
	if err := json.Unmarshal([]byte(body), &commentsResp); err != nil {
		t.Fatalf("unmarshal paged comments: %v (body %s)", err, body)
	}
	if commentsResp["total"] != float64(2) || len(commentsResp["comments"].([]any)) != 1 {
		t.Fatalf("paged comments = total %v len %d, want 2/1", commentsResp["total"], len(commentsResp["comments"].([]any)))
	}

	// Update.
	body, status = jiraBasicPutJSON(t, base+"/rest/api/3/issue/"+richKey+"/comment/"+commentIDs[0], email, token, map[string]any{
		"body": "First comment (edited)",
	})
	if status != 200 {
		t.Fatalf("update comment -> %d, want 200; body %s", status, body)
	}
	var updatedComment map[string]any
	if err := json.Unmarshal([]byte(body), &updatedComment); err != nil {
		t.Fatalf("unmarshal updated comment: %v (body %s)", err, body)
	}
	if updatedComment["body"] != "First comment (edited)" {
		t.Fatalf("updated comment body = %v, want edited text", updatedComment["body"])
	}
	if _, ok := updatedComment["_issue"]; ok {
		t.Fatal("comment response leaks the internal _issue key")
	}

	// Empty body on update -> 400.
	body, status = jiraBasicPutJSON(t, base+"/rest/api/3/issue/"+richKey+"/comment/"+commentIDs[0], email, token, map[string]any{
		"body": "",
	})
	if status != 400 {
		t.Fatalf("update comment empty body -> %d, want 400; body %s", status, body)
	}

	// Delete.
	body, status = jiraBasicDelete(t, base+"/rest/api/3/issue/"+richKey+"/comment/"+commentIDs[1], email, token)
	if status != 204 {
		t.Fatalf("delete comment -> %d, want 204; body %s", status, body)
	}
	body, status = jiraBasicGet(t, base+"/rest/api/3/issue/"+richKey+"/comment", email, token)
	if status != 200 {
		t.Fatalf("list comments after delete -> %d, want 200; body %s", status, body)
	}
	commentsResp = map[string]any{}
	if err := json.Unmarshal([]byte(body), &commentsResp); err != nil {
		t.Fatalf("unmarshal comments after delete: %v (body %s)", err, body)
	}
	if commentsResp["total"] != float64(1) {
		t.Fatalf("comments total after delete = %v, want 1", commentsResp["total"])
	}

	// Missing comment -> 404; comments on a missing issue -> 404.
	body, status = jiraBasicDelete(t, base+"/rest/api/3/issue/"+richKey+"/comment/"+commentIDs[1], email, token)
	if status != 404 {
		t.Fatalf("delete missing comment -> %d, want 404; body %s", status, body)
	}
	body, status = jiraBasicGet(t, base+"/rest/api/3/issue/NOPE-1/comment", email, token)
	if status != 404 {
		t.Fatalf("comments on missing issue -> %d, want 404; body %s", status, body)
	}
}
