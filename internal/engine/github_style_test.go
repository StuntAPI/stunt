package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestGitHubStyleAdapter exercises the GitHub App-style adapter end-to-end:
//
//   - 401 without auth
//   - App installation access token exchange (POST /app/installations/{id}/access_tokens)
//   - GET /app → app metadata
//   - GET /app/installations → list installations
//   - GET /repos/{owner}/{repo} → repo metadata
//   - Create issue → appears in issues list (STATEFUL)
//   - Get issue by number
//   - Close issue (PATCH)
//   - Create PR + list PRs (STATEFUL)
//   - PR reviews
//   - Workflow dispatch + actions runs
//   - Register webhook
//   - GraphQL: viewer query
//   - GitHub error envelope {message, documentation_url}
func TestGitHubStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "github-style")
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
			"github": {Adapter: absAdapterDir},
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

	base := addrs["github"]

	// ===== 401 without auth =====

	_, status := ghNoAuth(t, base+"/repos/octocat/hello-world/issues")
	if status != 401 {
		t.Fatalf("GET issues without auth -> status %d, want 401", status)
	}

	// ===== 401 with an unknown token (validation is real, not presence-only) =====

	_, status = ghGetBearer(t, base+"/repos/octocat/hello-world/issues", "ghp_bogus_token")
	if status != 401 {
		t.Fatalf("GET issues with bogus PAT -> status %d, want 401", status)
	}

	// ===== App installation access token exchange =====
	// POST /app/installations/{id}/access_tokens with Bearer <app-jwt>

	body, status := ghPostBearer(t, base+"/app/installations/1/access_tokens", "mock-app-jwt-token", map[string]any{})
	if status != 201 {
		t.Fatalf("access_tokens -> status %d, want 201; body %s", status, body)
	}
	var tokResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokResp); err != nil {
		t.Fatalf("unmarshal access_token: %v (body %s)", err, body)
	}
	installToken, ok := tokResp["token"].(string)
	if !ok || !strings.HasPrefix(installToken, "ghs_") {
		t.Fatalf("installation token = %v, want ghs_* prefix", tokResp["token"])
	}
	if _, ok := tokResp["expires_at"].(string); !ok {
		t.Fatalf("expires_at = %v, want string", tokResp["expires_at"])
	}
	perms, ok := tokResp["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions = %v, want object", tokResp["permissions"])
	}
	if len(perms) < 1 {
		t.Fatalf("permissions empty, want >=1 entry")
	}

	// ===== GET /app → app metadata (with app JWT) =====

	body, status = ghGetBearer(t, base+"/app", "mock-app-jwt-token")
	if status != 200 {
		t.Fatalf("GET /app -> status %d; body %s", status, body)
	}
	var appObj map[string]any
	if err := json.Unmarshal([]byte(body), &appObj); err != nil {
		t.Fatalf("unmarshal app: %v", err)
	}
	if _, ok := appObj["slug"].(string); !ok {
		t.Fatalf("app slug = %v", appObj["slug"])
	}

	// ===== GET /repos/{owner}/{repo} =====

	const owner = "octocat"
	const repo = "hello-world"
	body, status = ghGetBearer(t, base+"/repos/"+owner+"/"+repo, installToken)
	if status != 200 {
		t.Fatalf("GET repo -> status %d; body %s", status, body)
	}
	var repoObj map[string]any
	if err := json.Unmarshal([]byte(body), &repoObj); err != nil {
		t.Fatalf("unmarshal repo: %v", err)
	}
	if repoObj["full_name"] != owner+"/"+repo {
		t.Fatalf("full_name = %v, want %s/%s", repoObj["full_name"], owner, repo)
	}
	if _, ok := repoObj["default_branch"].(string); !ok {
		t.Fatalf("default_branch = %v", repoObj["default_branch"])
	}

	// ===== List issues (seeded) =====

	body, status = ghGetBearer(t, base+"/repos/"+owner+"/"+repo+"/issues?state=open", installToken)
	if status != 200 {
		t.Fatalf("GET issues -> status %d; body %s", status, body)
	}
	var issues []any
	if err := json.Unmarshal([]byte(body), &issues); err != nil {
		t.Fatalf("unmarshal issues: %v (body %s)", err, body)
	}
	initialIssues := len(issues)
	if initialIssues < 1 {
		t.Fatalf("expected >=1 seeded issue, got %d", initialIssues)
	}

	// ===== Create issue → appears in list (STATEFUL) =====

	body, status = ghPostBearer(t, base+"/repos/"+owner+"/"+repo+"/issues", installToken, map[string]any{
		"title":  "Bug: synthetic test issue",
		"body":   "This is a synthetic issue body.",
		"labels": []string{"bug", "test"},
	})
	if status != 201 {
		t.Fatalf("POST issue -> status %d, want 201; body %s", status, body)
	}
	var createdIssue map[string]any
	if err := json.Unmarshal([]byte(body), &createdIssue); err != nil {
		t.Fatalf("unmarshal created issue: %v", err)
	}
	issueNum := ghToInt(createdIssue["number"])
	if issueNum == 0 {
		t.Fatalf("issue number = %v, want non-zero", createdIssue["number"])
	}
	if createdIssue["state"] != "open" {
		t.Fatalf("issue state = %v, want open", createdIssue["state"])
	}
	if createdIssue["title"] != "Bug: synthetic test issue" {
		t.Fatalf("issue title = %v", createdIssue["title"])
	}

	// Verify it appears in the list.
	body, status = ghGetBearer(t, base+"/repos/"+owner+"/"+repo+"/issues", installToken)
	if err := json.Unmarshal([]byte(body), &issues); err != nil {
		t.Fatalf("re-unmarshal issues: %v", err)
	}
	if len(issues) != initialIssues+1 {
		t.Fatalf("issues count = %d, want %d", len(issues), initialIssues+1)
	}
	foundIssue := false
	for _, i := range issues {
		if ghToInt(i.(map[string]any)["number"]) == issueNum {
			foundIssue = true
		}
	}
	if !foundIssue {
		t.Fatalf("created issue #%d not in list", issueNum)
	}

	// ===== Get issue by number =====

	body, status = ghGetBearer(t, base+"/repos/"+owner+"/"+repo+"/issues/"+strconv.Itoa(issueNum), installToken)
	if status != 200 {
		t.Fatalf("GET issue by number -> status %d; body %s", status, body)
	}
	var fetchedIssue map[string]any
	if err := json.Unmarshal([]byte(body), &fetchedIssue); err != nil {
		t.Fatalf("unmarshal fetched issue: %v", err)
	}
	if ghToInt(fetchedIssue["number"]) != issueNum {
		t.Fatalf("fetched issue number mismatch")
	}

	// ===== Close issue (PATCH) =====

	body, status = ghPatchBearer(t, base+"/repos/"+owner+"/"+repo+"/issues/"+strconv.Itoa(issueNum), installToken, map[string]any{
		"state": "closed",
	})
	if status != 200 {
		t.Fatalf("PATCH issue -> status %d; body %s", status, body)
	}
	var closedIssue map[string]any
	if err := json.Unmarshal([]byte(body), &closedIssue); err != nil {
		t.Fatalf("unmarshal closed issue: %v", err)
	}
	if closedIssue["state"] != "closed" {
		t.Fatalf("closed issue state = %v, want closed", closedIssue["state"])
	}

	// ===== List PRs (seeded) =====

	body, status = ghGetBearer(t, base+"/repos/"+owner+"/"+repo+"/pulls", installToken)
	if status != 200 {
		t.Fatalf("GET pulls -> status %d; body %s", status, body)
	}
	var pulls []any
	if err := json.Unmarshal([]byte(body), &pulls); err != nil {
		t.Fatalf("unmarshal pulls: %v (body %s)", err, body)
	}
	if len(pulls) < 1 {
		t.Fatalf("expected >=1 seeded PR, got %d", len(pulls))
	}

	// ===== Create PR (STATEFUL) =====

	body, status = ghPostBearer(t, base+"/repos/"+owner+"/"+repo+"/pulls", installToken, map[string]any{
		"title": "feat: synthetic PR",
		"head":  "feature-branch",
		"base":  "main",
	})
	if status != 201 {
		t.Fatalf("POST PR -> status %d, want 201; body %s", status, body)
	}
	var createdPR map[string]any
	if err := json.Unmarshal([]byte(body), &createdPR); err != nil {
		t.Fatalf("unmarshal created PR: %v", err)
	}
	prNum := ghToInt(createdPR["number"])
	if prNum == 0 {
		t.Fatalf("PR number = %v", createdPR["number"])
	}
	if createdPR["state"] != "open" {
		t.Fatalf("PR state = %v, want open", createdPR["state"])
	}

	// ===== PR reviews =====

	body, status = ghGetBearer(t, base+"/repos/"+owner+"/"+repo+"/pulls/"+strconv.Itoa(prNum)+"/reviews", installToken)
	if status != 200 {
		t.Fatalf("GET PR reviews -> status %d; body %s", status, body)
	}
	var reviews []any
	if err := json.Unmarshal([]byte(body), &reviews); err != nil {
		t.Fatalf("unmarshal reviews: %v", err)
	}

	// ===== Workflow dispatch =====

	body, status = ghPostBearer(t, base+"/repos/"+owner+"/"+repo+"/dispatches", installToken, map[string]any{
		"event_type": "synthetic-test",
		"client_payload": map[string]any{
			"env": "test",
		},
	})
	if status != 204 {
		t.Fatalf("POST dispatches -> status %d, want 204; body %s", status, body)
	}

	// ===== Actions runs =====

	body, status = ghGetBearer(t, base+"/repos/"+owner+"/"+repo+"/actions/runs", installToken)
	if status != 200 {
		t.Fatalf("GET actions runs -> status %d; body %s", status, body)
	}
	var runsObj map[string]any
	if err := json.Unmarshal([]byte(body), &runsObj); err != nil {
		t.Fatalf("unmarshal runs: %v", err)
	}
	if _, ok := runsObj["workflow_runs"].([]any); !ok {
		t.Fatalf("workflow_runs = %v, want array", runsObj["workflow_runs"])
	}

	// ===== Register webhook =====

	body, status = ghPostBearer(t, base+"/repos/"+owner+"/"+repo+"/hooks", installToken, map[string]any{
		"config": map[string]any{
			"url":          "https://example.com/webhook",
			"content_type": "json",
			"secret":       "webhook_secret_value",
		},
		"events": []string{"push", "pull_request"},
	})
	if status != 201 {
		t.Fatalf("POST hooks -> status %d, want 201; body %s", status, body)
	}
	var hookResp map[string]any
	if err := json.Unmarshal([]byte(body), &hookResp); err != nil {
		t.Fatalf("unmarshal hook: %v", err)
	}
	if _, ok := hookResp["id"]; !ok {
		t.Fatalf("hook has no id: %v", hookResp)
	}

	// ===== GraphQL: viewer =====

	body, status = ghPostBearer(t, base+"/graphql", installToken, map[string]any{
		"query": `{ viewer { login } }`,
	})
	if status != 200 {
		t.Fatalf("graphql viewer -> status %d; body %s", status, body)
	}
	var gqlResp map[string]any
	if err := json.Unmarshal([]byte(body), &gqlResp); err != nil {
		t.Fatalf("unmarshal graphql: %v", err)
	}
	gqlData, ok := gqlResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("graphql data = %v", gqlResp["data"])
	}
	viewer, ok := gqlData["viewer"].(map[string]any)
	if !ok {
		t.Fatalf("graphql viewer = %v", gqlData["viewer"])
	}
	if _, ok := viewer["login"].(string); !ok {
		t.Fatalf("viewer.login = %v", viewer["login"])
	}

	// ===== GET /app/installations =====

	body, status = ghGetBearer(t, base+"/app/installations", "mock-app-jwt-token")
	if status != 200 {
		t.Fatalf("GET installations -> status %d; body %s", status, body)
	}
	var installations []any
	if err := json.Unmarshal([]byte(body), &installations); err != nil {
		t.Fatalf("unmarshal installations: %v", err)
	}
	if len(installations) < 1 {
		t.Fatalf("installations = %d items, want >=1", len(installations))
	}

	// ===== 404 on nonexistent repo =====

	body, status = ghGetBearer(t, base+"/repos/nobody/nonexistent", installToken)
	if status != 404 {
		t.Fatalf("GET nonexistent repo -> status %d, want 404; body %s", status, body)
	}
	var notFound map[string]any
	if err := json.Unmarshal([]byte(body), &notFound); err != nil {
		t.Fatalf("unmarshal 404: %v", err)
	}
	if _, ok := notFound["message"].(string); !ok {
		t.Fatalf("404 message = %v", notFound["message"])
	}
	if _, ok := notFound["documentation_url"].(string); !ok {
		t.Fatalf("documentation_url = %v", notFound["documentation_url"])
	}

	// ===== PAT also works (token prefix) =====

	_, status = ghGetBearer(t, base+"/repos/"+owner+"/"+repo+"/issues", "ghp_pat_token_mock")
	if status != 200 {
		t.Fatalf("GET issues with PAT -> status %d, want 200", status)
	}
}

// === GitHub test helpers ===

func ghNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func ghGetBearer(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	ghSetAuth(req, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func ghPostBearer(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	ghSetAuth(req, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func ghPatchBearer(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PATCH", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	ghSetAuth(req, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// ghSetAuth sets the Authorization header: "Bearer <t>" for ghs_ tokens and
// app JWTs; "token <t>" for ghp_ PAT tokens.
func ghSetAuth(req *http.Request, token string) {
	if strings.HasPrefix(token, "ghp_") {
		req.Header.Set("Authorization", "token "+token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// ghToInt converts a JSON-unmarshaled number (float64) to int.
func ghToInt(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// TestGitHubStyleSignatureVerifies proves the adapter computes an
// X-Hub-Signature-256 the real GitHub formula accepts, and tags the delivery
// with X-GitHub-Event. A hook subscribing to "issues" is registered, then an
// issue is created → _emit_if_subscribed → _signed_emit.
func TestGitHubStyleSignatureVerifies(t *testing.T) {
	const secret = "stunt_mock_github_webhook_secret_2026"
	const anyToken = "ghp_signature_test"
	sink := newCaptureSink()
	defer sink.close()

	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "github-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"github": {Adapter: adapterDir, Config: map[string]any{"webhook_url": sink.srv.URL}},
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
	base := addrs["github"]

	// Register a hook that subscribes to "issues" so issue creation emits.
	// The hook's URL must be the sink: the emitter keeps one target per service
	// (last Register wins), so the adapter's events_register(hook.url) would
	// otherwise overwrite the config webhook_url sink registered at boot.
	hookOwner, hookRepo := "octocat", "hello-world"
	if _, status := ghPostBearer(t, base+"/repos/"+hookOwner+"/"+hookRepo+"/hooks", anyToken, map[string]any{
		"config": map[string]any{"url": sink.srv.URL, "content_type": "json"},
		"events": []string{"issues"},
	}); status != 201 {
		t.Fatalf("POST hooks -> %d, want 201", status)
	}

	// Create an issue → signed "issues" delivery.
	if _, status := ghPostBearer(t, base+"/repos/"+hookOwner+"/"+hookRepo+"/issues", anyToken, map[string]any{
		"title": "signature test",
		"body":  "verify me",
	}); status != 201 {
		t.Fatalf("POST issue -> %d, want 201", status)
	}

	raw, hdr := sink.awaitDelivery(t, time.Second)
	verifyGitHubSig(t, raw, hdr, secret, "issues")
}

// TestGitHubStyleActionsRunLifecycle proves the derive-on-read run state
// machine: a dispatched run is queued/in_progress immediately, reaches
// completed/success after the 3s window, and completed/failure with the
// simulator-only simulate_fail flag. Also exercises the single-run endpoint.
func TestGitHubStyleActionsRunLifecycle(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "github-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"github": {Adapter: adapterDir},
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
	base := addrs["github"]

	const token = "ghp_pat_token_mock"
	runsURL := base + "/repos/octocat/hello-world/actions/runs"

	// Dispatch a normal run and a simulate_fail run.
	if _, status := ghPostBearer(t, base+"/repos/octocat/hello-world/dispatches", token, map[string]any{
		"event_type": "lifecycle-ok",
	}); status != 204 {
		t.Fatalf("dispatch ok -> %d, want 204", status)
	}
	if _, status := ghPostBearer(t, base+"/repos/octocat/hello-world/dispatches", token, map[string]any{
		"event_type":    "lifecycle-fail",
		"simulate_fail": true,
	}); status != 204 {
		t.Fatalf("dispatch fail -> %d, want 204", status)
	}

	ghFindRun := func(name string) map[string]any {
		t.Helper()
		body, status := ghGetBearer(t, runsURL, token)
		if status != 200 {
			t.Fatalf("GET runs -> %d; body %s", status, body)
		}
		var runsObj map[string]any
		if err := json.Unmarshal([]byte(body), &runsObj); err != nil {
			t.Fatal(err)
		}
		for _, r := range runsObj["workflow_runs"].([]any) {
			rm := r.(map[string]any)
			if rm["name"] == name {
				return rm
			}
		}
		t.Fatalf("run %q not found", name)
		return nil
	}

	// Immediately: both runs are still pre-terminal (queued or in_progress).
	for _, name := range []string{"lifecycle-ok", "lifecycle-fail"} {
		run := ghFindRun(name)
		st := run["status"].(string)
		if st != "queued" && st != "in_progress" {
			t.Fatalf("run %q immediate status = %q, want queued|in_progress", name, st)
		}
	}

	// After the 3s window: success and failure conclusions.
	time.Sleep(3500 * time.Millisecond)

	okRun := ghFindRun("lifecycle-ok")
	if okRun["status"] != "completed" || okRun["conclusion"] != "success" {
		t.Fatalf("ok run = %v/%v, want completed/success", okRun["status"], okRun["conclusion"])
	}
	failRun := ghFindRun("lifecycle-fail")
	if failRun["status"] != "completed" || failRun["conclusion"] != "failure" {
		t.Fatalf("fail run = %v/%v, want completed/failure", failRun["status"], failRun["conclusion"])
	}

	// Single-run endpoint agrees with the list.
	body, status := ghGetBearer(t, runsURL+"/"+strconv.Itoa(ghToInt(okRun["id"])), token)
	if status != 200 {
		t.Fatalf("GET single run -> %d; body %s", status, body)
	}
	var single map[string]any
	if err := json.Unmarshal([]byte(body), &single); err != nil {
		t.Fatal(err)
	}
	if single["status"] != "completed" || single["conclusion"] != "success" {
		t.Fatalf("single run = %v/%v, want completed/success", single["status"], single["conclusion"])
	}
}
