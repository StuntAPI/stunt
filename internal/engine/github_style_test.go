package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	return ghDoBearer(t, "PATCH", rawurl, token, body)
}

func ghPutBearer(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	return ghDoBearer(t, "PUT", rawurl, token, body)
}

func ghDeleteBearer(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	return ghDoBearer(t, "DELETE", rawurl, token, nil)
}

func ghDoBearer(t *testing.T, method, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, rawurl, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
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

// TestGitHubStyleWebhookReceiver drives the local receiver surface
// (POST /webhooks/receive): an X-Hub-Signature-256 computed in Go over the
// verbatim body with the hook's registered per-hook secret gets 200, while a
// missing, tampered, or wrong-secret signature gets GitHub's 401 envelope.
func TestGitHubStyleWebhookReceiver(t *testing.T) {
	const token = "ghp_signature_test"
	const hookSecret = "receiver-hook-secret"
	base := ghBoot(t, "")

	// Register a hook carrying its own secret (GitHub's per-hook model).
	if _, status := ghPostBearer(t, base+"/repos/octocat/hello-world/hooks", token, map[string]any{
		"config": map[string]any{
			"url":          "https://example.com/hook",
			"content_type": "json",
			"secret":       hookSecret,
		},
		"events": []string{"push"},
	}); status != 201 {
		t.Fatalf("POST hooks -> %d, want 201", status)
	}

	raw := []byte(`{"zen":"Design for failure.","event":"push"}`)

	post := func(sig string) (string, int) {
		req, _ := http.NewRequest("POST", base+"/webhooks/receive", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if sig != "" {
			req.Header.Set("X-Hub-Signature-256", sig)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.StatusCode
	}

	mac := hmac.New(sha256.New, []byte(hookSecret))
	mac.Write(raw)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	// Correct per-hook signature → 200.
	if _, code := post(good); code != 200 {
		t.Fatalf("receiver with correct signature -> %d, want 200", code)
	}

	// Missing signature → 401 (GitHub envelope).
	body, code := post("")
	if code != 401 {
		t.Fatalf("receiver without signature -> %d, want 401; body %s", code, body)
	}
	var e map[string]any
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("unmarshal 401: %v (body %s)", err, body)
	}
	if e["message"] != "Invalid signature" {
		t.Fatalf("401 envelope = %v, want message Invalid signature", e)
	}

	// Tampered signature (right shape, wrong MAC) → 401.
	if _, code := post("sha256=" + strings.Repeat("0", 64)); code != 401 {
		t.Fatalf("receiver with tampered signature -> %d, want 401", code)
	}

	// Signature computed with an unknown secret → 401.
	wrong := hmac.New(sha256.New, []byte("not-a-registered-secret"))
	wrong.Write(raw)
	if _, code := post("sha256=" + hex.EncodeToString(wrong.Sum(nil))); code != 401 {
		t.Fatalf("receiver with wrong-secret signature -> %d, want 401", code)
	}

	// The documented fallback secret also verifies (hooks registered without
	// a secret deliver MACed with it).
	fb := hmac.New(sha256.New, []byte("stunt_mock_github_webhook_secret_2026"))
	fb.Write(raw)
	if _, code := post("sha256=" + hex.EncodeToString(fb.Sum(nil))); code != 200 {
		t.Fatalf("receiver with fallback-secret signature -> %d, want 200", code)
	}
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

// ghBoot starts a github-style service for a test and returns its base URL
// plus teardown.
func ghBoot(t *testing.T, webhookURL string) string {
	t.Helper()
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "github-style"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{}
	if webhookURL != "" {
		cfg["webhook_url"] = webhookURL
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"github": {Adapter: adapterDir, Config: cfg},
		},
	}
	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	t.Cleanup(cancel)
	time.Sleep(50 * time.Millisecond)
	return addrs["github"]
}

// TestGitHubStylePRLifecycle exercises the PR lifecycle (PATCH edit, PUT
// merge incl. the 405 conflict path, reviews) and the issue-comments / labels
// / issue-events / PATCH-state-validation surfaces, including failure paths.
func TestGitHubStylePRLifecycle(t *testing.T) {
	base := ghBoot(t, "")
	const token = "ghp_pat_token_mock"
	const ownerRepo = "repos/octocat/hello-world"

	// ===== Create a PR to drive through the lifecycle =====

	body, status := ghPostBearer(t, base+"/"+ownerRepo+"/pulls", token, map[string]any{
		"title": "feat: lifecycle PR",
		"head":  "feature-x",
		"base":  "main",
	})
	if status != 201 {
		t.Fatalf("POST PR -> %d; body %s", status, body)
	}
	var pr map[string]any
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatal(err)
	}
	prNum := ghToInt(pr["number"])
	prURL := base + "/" + ownerRepo + "/pulls/" + strconv.Itoa(prNum)

	// ===== PATCH: title/body edit =====

	body, status = ghPatchBearer(t, prURL, token, map[string]any{"title": "feat: renamed"})
	if status != 200 {
		t.Fatalf("PATCH PR title -> %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatal(err)
	}
	if pr["title"] != "feat: renamed" {
		t.Fatalf("patched PR title = %v", pr["title"])
	}

	// ===== PATCH: invalid state -> 422 (GitHub Validation Failed) =====

	body, status = ghPatchBearer(t, prURL, token, map[string]any{"state": "merged"})
	if status != 422 {
		t.Fatalf("PATCH PR state=merged -> %d, want 422; body %s", status, body)
	}
	var valErr map[string]any
	if err := json.Unmarshal([]byte(body), &valErr); err != nil {
		t.Fatal(err)
	}
	if valErr["message"] != "Validation Failed" {
		t.Fatalf("422 message = %v", valErr["message"])
	}

	// ===== Merge conflict path: retarget base, merge -> 405, resolve, merge -> 200 =====

	if _, status = ghPatchBearer(t, prURL, token, map[string]any{"base": "develop"}); status != 200 {
		t.Fatalf("PATCH PR base -> %d", status)
	}
	body, status = ghPutBearer(t, prURL+"/merge", token, map[string]any{})
	if status != 405 {
		t.Fatalf("merge with moved base -> %d, want 405; body %s", status, body)
	}
	// Re-PATCH the same base = conflicts resolved.
	if _, status = ghPatchBearer(t, prURL, token, map[string]any{"base": "develop"}); status != 200 {
		t.Fatalf("PATCH PR base resolve -> %d", status)
	}
	body, status = ghPutBearer(t, prURL+"/merge", token, map[string]any{})
	if status != 200 {
		t.Fatalf("merge -> %d, want 200; body %s", status, body)
	}
	var merged map[string]any
	if err := json.Unmarshal([]byte(body), &merged); err != nil {
		t.Fatal(err)
	}
	if merged["merged"] != true {
		t.Fatalf("merge merged = %v, want true", merged["merged"])
	}
	if _, ok := merged["sha"].(string); !ok || merged["sha"] == "" {
		t.Fatalf("merge sha = %v, want non-empty string", merged["sha"])
	}

	// The PR itself now reports merged + closed, with the merge commit sha.
	body, status = ghGetBearer(t, prURL, token)
	if status != 200 {
		t.Fatalf("GET PR -> %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatal(err)
	}
	if pr["merged"] != true || pr["state"] != "closed" {
		t.Fatalf("merged PR = %v/%v, want closed/merged", pr["state"], pr["merged"])
	}
	if pr["merge_commit_sha"] != merged["sha"] {
		t.Fatalf("merge_commit_sha = %v, want %v", pr["merge_commit_sha"], merged["sha"])
	}
	if _, ok := pr["merged_at"].(string); !ok {
		t.Fatalf("merged_at = %v, want string", pr["merged_at"])
	}
	// Internal _ keys (the _base_changed conflict marker) must never leak.
	if strings.Contains(body, "_base_changed") {
		t.Fatalf("PR view leaked internal _base_changed: %s", body)
	}

	// ===== Re-merge -> 405 not mergeable =====

	if _, status = ghPutBearer(t, prURL+"/merge", token, map[string]any{}); status != 405 {
		t.Fatalf("re-merge -> %d, want 405", status)
	}

	// ===== Reviews: submit + list + invalid event =====

	body, status = ghPostBearer(t, base+"/"+ownerRepo+"/pulls", token, map[string]any{
		"title": "feat: reviewed PR",
		"head":  "feature-y",
		"base":  "main",
	})
	if status != 201 {
		t.Fatalf("POST reviewed PR -> %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatal(err)
	}
	revNum := ghToInt(pr["number"])
	revURL := base + "/" + ownerRepo + "/pulls/" + strconv.Itoa(revNum)

	body, status = ghPostBearer(t, revURL+"/reviews", token, map[string]any{
		"body":  "ship it",
		"event": "APPROVE",
	})
	if status != 200 {
		t.Fatalf("POST APPROVE review -> %d; body %s", status, body)
	}
	var review map[string]any
	if err := json.Unmarshal([]byte(body), &review); err != nil {
		t.Fatal(err)
	}
	if review["state"] != "APPROVED" {
		t.Fatalf("approve review state = %v", review["state"])
	}

	body, status = ghPostBearer(t, revURL+"/reviews", token, map[string]any{
		"body":  "one more thing",
		"event": "REQUEST_CHANGES",
	})
	if status != 200 {
		t.Fatalf("POST REQUEST_CHANGES review -> %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &review); err != nil {
		t.Fatal(err)
	}
	if review["state"] != "CHANGES_REQUESTED" {
		t.Fatalf("request-changes review state = %v", review["state"])
	}

	// Invalid event value -> 422.
	if _, status = ghPostBearer(t, revURL+"/reviews", token, map[string]any{
		"event": "BAD_EVENT",
	}); status != 422 {
		t.Fatalf("POST review bad event -> %d, want 422", status)
	}

	// List shows both submitted reviews.
	body, status = ghGetBearer(t, revURL+"/reviews", token)
	if status != 200 {
		t.Fatalf("GET reviews -> %d; body %s", status, body)
	}
	var reviews []any
	if err := json.Unmarshal([]byte(body), &reviews); err != nil {
		t.Fatal(err)
	}
	states := map[string]int{}
	for _, r := range reviews {
		states[r.(map[string]any)["state"].(string)]++
	}
	if states["APPROVED"] != 1 || states["CHANGES_REQUESTED"] != 1 {
		t.Fatalf("review states = %v, want one APPROVED + one CHANGES_REQUESTED", states)
	}

	// ===== Issue comments: create + list + empty-body 422; works on PRs too =====

	num := strconv.Itoa(revNum)
	body, status = ghPostBearer(t, base+"/"+ownerRepo+"/issues/"+num+"/comments", token, map[string]any{
		"body": "commenting on a PR via the issues API",
	})
	if status != 201 {
		t.Fatalf("POST PR comment -> %d; body %s", status, body)
	}
	var comment map[string]any
	if err := json.Unmarshal([]byte(body), &comment); err != nil {
		t.Fatal(err)
	}
	if comment["body"] != "commenting on a PR via the issues API" {
		t.Fatalf("comment body = %v", comment["body"])
	}

	if _, status = ghPostBearer(t, base+"/"+ownerRepo+"/issues/"+num+"/comments", token, map[string]any{}); status != 422 {
		t.Fatalf("POST empty comment -> %d, want 422", status)
	}

	body, status = ghGetBearer(t, base+"/"+ownerRepo+"/issues/"+num+"/comments", token)
	if status != 200 {
		t.Fatalf("GET PR comments -> %d; body %s", status, body)
	}
	var comments []any
	if err := json.Unmarshal([]byte(body), &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Fatalf("PR comments = %d, want 1", len(comments))
	}

	// ===== Labels: add (idempotent), remove, remove-absent 404 =====

	body, status = ghPostBearer(t, base+"/"+ownerRepo+"/issues/"+num+"/labels/bug", token, nil)
	if status != 200 {
		t.Fatalf("POST label -> %d; body %s", status, body)
	}
	var labels []any
	if err := json.Unmarshal([]byte(body), &labels); err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels[0].(map[string]any)["name"] != "bug" {
		t.Fatalf("labels after add = %v", labels)
	}
	// Idempotent re-add keeps exactly one instance.
	if _, status = ghPostBearer(t, base+"/"+ownerRepo+"/issues/"+num+"/labels/bug", token, nil); status != 200 {
		t.Fatalf("POST label re-add -> %d", status)
	}

	if _, status = ghDeleteBearer(t, base+"/"+ownerRepo+"/issues/"+num+"/labels/bug", token); status != 204 {
		t.Fatalf("DELETE label -> %d, want 204", status)
	}
	if _, status = ghDeleteBearer(t, base+"/"+ownerRepo+"/issues/"+num+"/labels/bug", token); status != 404 {
		t.Fatalf("DELETE absent label -> %d, want 404", status)
	}

	// ===== Issue events surface: labeled/unlabeled from above + merged + closed =====

	body, status = ghGetBearer(t, base+"/"+ownerRepo+"/issues/"+strconv.Itoa(prNum)+"/events", token)
	if status != 200 {
		t.Fatalf("GET merged PR events -> %d; body %s", status, body)
	}
	var events []any
	if err := json.Unmarshal([]byte(body), &events); err != nil {
		t.Fatal(err)
	}
	// The merged PR's timeline: merged (from PUT merge).
	found := false
	for _, e := range events {
		if e.(map[string]any)["event"] == "merged" {
			found = true
		}
	}
	if !found {
		t.Fatalf("merged PR timeline missing merged event: %v", events)
	}

	body, status = ghGetBearer(t, base+"/"+ownerRepo+"/issues/"+num+"/events", token)
	if status != 200 {
		t.Fatalf("GET PR events -> %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &events); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, e := range events {
		kinds[e.(map[string]any)["event"].(string)]++
	}
	if kinds["labeled"] != 1 || kinds["unlabeled"] != 1 {
		t.Fatalf("PR timeline = %v, want labeled + unlabeled", kinds)
	}
	// The labeled event carries the label object.
	for _, e := range events {
		em := e.(map[string]any)
		if em["event"] == "labeled" {
			l, ok := em["label"].(map[string]any)
			if !ok || l["name"] != "bug" {
				t.Fatalf("labeled event label = %v, want bug", em["label"])
			}
		}
	}

	// ===== Issue PATCH state validation + closed_at/state_reason =====

	body, status = ghPostBearer(t, base+"/"+ownerRepo+"/issues", token, map[string]any{
		"title": "state validation target",
	})
	if status != 201 {
		t.Fatalf("POST issue -> %d; body %s", status, body)
	}
	var issue map[string]any
	if err := json.Unmarshal([]byte(body), &issue); err != nil {
		t.Fatal(err)
	}
	issueNum := strconv.Itoa(ghToInt(issue["number"]))

	if _, status = ghPatchBearer(t, base+"/"+ownerRepo+"/issues/"+issueNum, token, map[string]any{
		"state": "MERGED",
	}); status != 422 {
		t.Fatalf("PATCH issue bad state -> %d, want 422", status)
	}
	if _, status = ghPatchBearer(t, base+"/"+ownerRepo+"/issues/"+issueNum, token, map[string]any{
		"state":        "closed",
		"state_reason": "bogus",
	}); status != 422 {
		t.Fatalf("PATCH issue bad state_reason -> %d, want 422", status)
	}

	body, status = ghPatchBearer(t, base+"/"+ownerRepo+"/issues/"+issueNum, token, map[string]any{
		"state":        "closed",
		"state_reason": "completed",
	})
	if status != 200 {
		t.Fatalf("PATCH issue close -> %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &issue); err != nil {
		t.Fatal(err)
	}
	if issue["state"] != "closed" || issue["state_reason"] != "completed" {
		t.Fatalf("closed issue = %v/%v", issue["state"], issue["state_reason"])
	}
	if _, ok := issue["closed_at"].(string); !ok {
		t.Fatalf("closed_at = %v, want string", issue["closed_at"])
	}

	// Reopen clears closed_at.
	body, status = ghPatchBearer(t, base+"/"+ownerRepo+"/issues/"+issueNum, token, map[string]any{
		"state": "open",
	})
	if status != 200 {
		t.Fatalf("PATCH issue reopen -> %d", status)
	}
	if err := json.Unmarshal([]byte(body), &issue); err != nil {
		t.Fatal(err)
	}
	if issue["state"] != "open" || issue["closed_at"] != nil {
		t.Fatalf("reopened issue = %v closed_at=%v", issue["state"], issue["closed_at"])
	}
}

// TestGitHubStyleCommentAndReviewWebhooks proves the new surfaces deliver
// signed webhooks with the per-hook secret: issue_comment on comment creation
// and pull_request_review on review submission.
func TestGitHubStyleCommentAndReviewWebhooks(t *testing.T) {
	const secret = "gh_p2_hook_secret"
	const token = "ghp_signature_test"
	sink := newCaptureSink()
	defer sink.close()
	base := ghBoot(t, sink.srv.URL)

	// Register a hook with its own secret, subscribing to the new events.
	if _, status := ghPostBearer(t, base+"/repos/octocat/hello-world/hooks", token, map[string]any{
		"config": map[string]any{"url": sink.srv.URL, "content_type": "json", "secret": secret},
		"events": []string{"issue_comment", "pull_request_review"},
	}); status != 201 {
		t.Fatalf("POST hooks -> %d, want 201", status)
	}

	// Comment on the seeded issue #1 -> signed issue_comment delivery.
	if _, status := ghPostBearer(t, base+"/repos/octocat/hello-world/issues/1/comments", token, map[string]any{
		"body": "signed comment delivery",
	}); status != 201 {
		t.Fatalf("POST comment -> %d, want 201", status)
	}
	raw, hdr := sink.awaitDelivery(t, time.Second)
	verifyGitHubSig(t, raw, hdr, secret, "issue_comment")
	if strings.Contains(string(raw), "_base_changed") {
		t.Fatalf("issue_comment payload leaked internal keys: %s", raw)
	}

	// Review the seeded PR #1 -> signed pull_request_review delivery.
	if _, status := ghPostBearer(t, base+"/repos/octocat/hello-world/pulls/1/reviews", token, map[string]any{
		"body":  "signed review delivery",
		"event": "APPROVE",
	}); status != 200 {
		t.Fatalf("POST review -> %d, want 200", status)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		n := len(sink.notifies)
		sink.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sink.mu.Lock()
	second := sink.notifies[len(sink.notifies)-1]
	sink.mu.Unlock()
	verifyGitHubSig(t, second.body, second.hdr, secret, "pull_request_review")
}

// ghSetupGraphql serves the committed github-style adapter and returns its
// HTTP base URL + cleanup.
func ghSetupGraphql(t *testing.T) (string, func()) {
	t.Helper()

	absDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "github-style"))
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"github": {Adapter: absDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		e.Close()
		t.Fatalf("ServeForTest: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	url := addrs["github"]
	cleanup := func() {
		cancel()
		e.Close()
	}
	return url, cleanup
}

// ghGraphql sends a GraphQL POST (with a valid PAT) and returns data + the
// raw body, failing the test when the response carries top-level errors.
func ghGraphql(t *testing.T, base, query string, variables map[string]any) (map[string]any, string) {
	t.Helper()
	body, status := ghPostBearer(t, base+"/graphql", "ghp_pat_token_mock", map[string]any{
		"query":     query,
		"variables": variables,
	})
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal graphql response: %v (body %s)", err, body)
	}
	if errs, ok := resp["errors"]; ok && errs != nil {
		t.Fatalf("graphql errors: %v (query %s)", errs, query)
	}
	if status != 200 {
		t.Fatalf("graphql -> status %d, want 200 (body %s)", status, body)
	}
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("graphql data = %v (body %s)", resp["data"], body)
	}
	return data, body
}

// TestGitHubStyleGraphqlExecution exercises the real GraphQL executor end
// to end: nested repository/issue/PR joins, the Actor interface, variables +
// aliases, mutations sharing the REST state machine (with REST parity), and
// validation failures for unknown fields and bad global ids.
func TestGitHubStyleGraphqlExecution(t *testing.T) {
	base, cleanup := ghSetupGraphql(t)
	defer cleanup()
	const pat = "ghp_pat_token_mock"

	// ===== Read: repository + issues + PRs with nested joins =====

	query := `query($owner: String!, $name: String!, $n: Int!) {
		a: repository(owner: $owner, name: $name) {
			id
			nameWithOwner
			isPrivate
			defaultBranchRef { name prefix }
			issues(first: $n, states: OPEN, orderBy: {field: CREATED_AT, direction: ASC}) {
				totalCount
				nodes { number title state stateReason author { __typename login url } labels(first: 5) { nodes { id name color } } comments(first: 5) { totalCount } url createdAt }
			}
			pullRequests(first: $n) { totalCount nodes { number state headRefName baseRefName merged } }
		}
		b: viewer { login name }
		miss: repository(owner: "octocat", name: "nope") { id }
	}`
	data, _ := ghGraphql(t, base, query, map[string]any{"owner": "octocat", "name": "hello-world", "n": 10})
	repo := data["a"].(map[string]any)

	repoGID, ok := repo["id"].(string)
	if !ok || repoGID == "" {
		t.Fatalf("repository.id = %v, want a base64 global id", repo["id"])
	}
	if repo["nameWithOwner"] != "octocat/hello-world" {
		t.Fatalf("nameWithOwner = %v", repo["nameWithOwner"])
	}
	if ref := repo["defaultBranchRef"].(map[string]any); ref["name"] != "main" || ref["prefix"] != "refs/heads/" {
		t.Fatalf("defaultBranchRef = %v", ref)
	}

	issues := repo["issues"].(map[string]any)
	if issues["totalCount"] != float64(1) {
		t.Fatalf("issues.totalCount = %v, want 1 (the seeded issue)", issues["totalCount"])
	}
	node := issues["nodes"].([]any)[0].(map[string]any)
	if node["number"] != float64(1) || node["state"] != "OPEN" {
		t.Fatalf("seeded issue = %v, want #1 OPEN", node)
	}
	author := node["author"].(map[string]any)
	if author["__typename"] != "User" || author["login"] != "octocat" {
		t.Fatalf("issue.author = %v, want a User actor 'octocat'", author)
	}
	labels := node["labels"].(map[string]any)["nodes"].([]any)
	if len(labels) != 1 || labels[0].(map[string]any)["name"] != "documentation" {
		t.Fatalf("issue labels = %v, want [documentation]", labels)
	}
	if !strings.HasPrefix(node["url"].(string), "https://github.com/octocat/hello-world/issues/1") {
		t.Fatalf("issue url = %v", node["url"])
	}

	prs := repo["pullRequests"].(map[string]any)
	pr := prs["nodes"].([]any)[0].(map[string]any)
	if pr["state"] != "OPEN" || pr["headRefName"] != "develop" || pr["baseRefName"] != "main" || pr["merged"] != false {
		t.Fatalf("seeded PR = %v, want OPEN develop→main unmerged", pr)
	}

	if data["b"].(map[string]any)["login"] != "stunt-dev" {
		t.Fatalf("viewer.login = %v", data["b"])
	}
	if miss := data["miss"]; miss != nil {
		t.Fatalf("repository(octocat/nope) = %v, want null", miss)
	}

	// ===== Mutations: createIssue → updateIssue → addComment =====

	createMut := `mutation($rid: ID!) {
		createIssue(input: {repositoryId: $rid, title: "From GraphQL", body: "created via the executor", labels: ["graphql"]}) {
			issue { number title state author { __typename login } labels(first: 5) { nodes { name } } }
		}
	}`
	data, _ = ghGraphql(t, base, createMut, map[string]any{"rid": repoGID})
	created := data["createIssue"].(map[string]any)["issue"].(map[string]any)
	number := created["number"].(float64)
	if number != 2 {
		t.Fatalf("created issue number = %v, want 2 (per-repo sequence)", number)
	}
	if created["author"].(map[string]any)["__typename"] != "Bot" {
		t.Fatalf("created author = %v, want a Bot actor", created["author"])
	}
	if got := created["labels"].(map[string]any)["nodes"].([]any); len(got) != 1 || got[0].(map[string]any)["name"] != "graphql" {
		t.Fatalf("created labels = %v, want [graphql]", got)
	}

	// REST parity: the GraphQL-created issue is visible through REST.
	body, status := ghGetBearer(t, base+"/repos/octocat/hello-world/issues", pat)
	if status != 200 {
		t.Fatalf("REST list issues -> %d", status)
	}
	var restIssues []any
	json.Unmarshal([]byte(body), &restIssues)
	found := false
	for _, i := range restIssues {
		if i.(map[string]any)["number"] == number && i.(map[string]any)["title"] == "From GraphQL" {
			found = true
		}
	}
	if !found {
		t.Fatalf("GraphQL-created issue %v not visible via REST (body %s)", number, body)
	}

	// The issue global id round-trips through the schema's id field.
	gqlIDQuery := `query($n: Int!) { repository(owner: "octocat", name: "hello-world") { issue(number: $n) { id } } }`
	data, _ = ghGraphql(t, base, gqlIDQuery, map[string]any{"n": number})
	issueGID := data["repository"].(map[string]any)["issue"].(map[string]any)["id"].(string)

	updateMut := `mutation($id: ID!) {
		updateIssue(input: {id: $id, state: CLOSED, stateReason: COMPLETED}) { issue { number state stateReason closedAt } }
	}`
	data, _ = ghGraphql(t, base, updateMut, map[string]any{"id": issueGID})
	updated := data["updateIssue"].(map[string]any)["issue"].(map[string]any)
	if updated["state"] != "CLOSED" || updated["stateReason"] != "COMPLETED" {
		t.Fatalf("updated issue = %v, want CLOSED/COMPLETED", updated)
	}
	if updated["closedAt"] == nil {
		t.Fatalf("closedAt = nil after closing via GraphQL")
	}

	commentMut := `mutation($sid: ID!) {
		addComment(input: {subjectId: $sid, body: "comment via GraphQL"}) {
			commentEdge { node { id body author { login } } }
			subject { number }
		}
	}`
	data, _ = ghGraphql(t, base, commentMut, map[string]any{"sid": issueGID})
	payload := data["addComment"].(map[string]any)
	comment := payload["commentEdge"].(map[string]any)["node"].(map[string]any)
	if comment["body"] != "comment via GraphQL" {
		t.Fatalf("comment = %v", comment)
	}
	if payload["subject"].(map[string]any)["number"] != number {
		t.Fatalf("subject number = %v, want %v", payload["subject"], number)
	}

	// The comment shows up through the nested connection.
	data, _ = ghGraphql(t, base, `query($n: Int!) { repository(owner: "octocat", name: "hello-world") { issue(number: $n) { comments(first: 5) { totalCount nodes { body author { __typename } } } } } }`, map[string]any{"n": number})
	comments := data["repository"].(map[string]any)["issue"].(map[string]any)["comments"].(map[string]any)
	if comments["totalCount"] != float64(1) {
		t.Fatalf("comments.totalCount = %v, want 1", comments["totalCount"])
	}
	if comments["nodes"].([]any)[0].(map[string]any)["author"].(map[string]any)["__typename"] != "Bot" {
		t.Fatalf("comment author = %v, want a Bot actor", comments["nodes"])
	}

	// ===== Failure paths =====

	// Unknown field → 400 + errors[] naming the field.
	body, status = ghPostBearer(t, base+"/graphql", pat, map[string]any{
		"query": `{ viewer { login nonexistentField } }`,
	})
	if status != 400 {
		t.Fatalf("unknown field -> %d, want 400 (body %s)", status, body)
	}
	if !strings.Contains(body, "nonexistentField") {
		t.Fatalf("unknown field error should name the field: %s", body)
	}

	// Unknown root operation → 400.
	body, status = ghPostBearer(t, base+"/graphql", pat, map[string]any{
		"query": `{ repositories(first: 5) { id } }`,
	})
	if status != 400 {
		t.Fatalf("unknown root field -> %d, want 400 (body %s)", status, body)
	}

	// createIssue with an unresolvable repositoryId → GraphQL errors[]
	// (GitHub's "Could not resolve to a Repository" message).
	body, status = ghPostBearer(t, base+"/graphql", pat, map[string]any{
		"query":     `mutation { createIssue(input: {repositoryId: "gid-bogus", title: "x"}) { issue { number } } }`,
		"variables": map[string]any{},
	})
	if status != 200 {
		t.Fatalf("bogus repositoryId -> %d, want 200 with errors[] (body %s)", status, body)
	}
	var errResp map[string]any
	json.Unmarshal([]byte(body), &errResp)
	if errs, ok := errResp["errors"].([]any); !ok || len(errs) == 0 || !strings.Contains(body, "Could not resolve to a Repository") {
		t.Fatalf("bogus repositoryId errors = %v", errs)
	}

	// addComment with an unresolvable subjectId → GraphQL errors[].
	body, _ = ghPostBearer(t, base+"/graphql", pat, map[string]any{
		"query": `mutation { addComment(input: {subjectId: "bm90LWEtZ2lk", body: "x"}) { commentEdge { node { id } } } }`,
	})
	if !strings.Contains(body, "Could not resolve") {
		t.Fatalf("bogus subjectId errors = %s", body)
	}
}
