package engine

import (
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

// TestRedditStyleAdapter exercises the Reddit-style reference adapter
// end-to-end through the submit + token-refresh + User-Agent surfaces,
// asserting it faithfully reproduces the Python mock_reddit contract:
//
//   - access_token via authorization_code (duration=permanent) → access + refresh
//   - access_token via refresh_token (HTTP Basic) → access ONLY, no new refresh
//   - refresh_token grant with Basic missing → 401 invalid_client
//   - submit (valid) → {json:{errors:[], data:{id,url,name}}}
//   - submit missing title → non-empty errors[] (HTTP 200)
//   - submit missing subreddit → non-empty errors[] (HTTP 200)
//   - submit without bearer → 401 USER_REQUIRED
//   - missing User-Agent → 429
//   - generic User-Agent (no parens) → 429
func TestRedditStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "reddit-style")
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
			"reddit": {Adapter: absAdapterDir},
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

	base := addrs["reddit"]

	const (
		clientID     = "test-client-id"
		clientSecret = "test-client-secret"
		userAgent    = "***REMOVED***.me/1.0 (by /u/test)"
	)
	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(clientID+":"+clientSecret))

	// ===== Seed a refresh token via authorization_code (duration=permanent) =====

	body, status := redditPostFormUA(t, base+"/api/v1/access_token", basicAuth, userAgent, url.Values{
		"grant_type": {"authorization_code"},
		"code":       {"mock_code_1"},
		"duration":   {"permanent"},
	})
	if status != 200 {
		t.Fatalf("auth code grant -> status %d, want 200; body %s", status, body)
	}
	var authCodeResp map[string]any
	if err := json.Unmarshal([]byte(body), &authCodeResp); err != nil {
		t.Fatalf("unmarshal auth-code response: %v (body %s)", err, body)
	}
	refreshToken, ok := authCodeResp["refresh_token"].(string)
	if !ok || refreshToken == "" {
		t.Fatalf("refresh_token = %v, want non-empty string", authCodeResp["refresh_token"])
	}
	if !strings.HasPrefix(refreshToken, "rdref_") {
		t.Fatalf("refresh_token = %q, want rdref_* prefix", refreshToken)
	}
	seedAccess, ok := authCodeResp["access_token"].(string)
	if !ok || seedAccess == "" {
		t.Fatalf("access_token = %v, want non-empty string", authCodeResp["access_token"])
	}

	// ===== Refresh via grant_type=refresh_token (HTTP Basic) → access ONLY =====

	body, status = redditPostFormUA(t, base+"/api/v1/access_token", basicAuth, userAgent, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
	if status != 200 {
		t.Fatalf("refresh grant -> status %d, want 200; body %s", status, body)
	}
	var refreshResp map[string]any
	if err := json.Unmarshal([]byte(body), &refreshResp); err != nil {
		t.Fatalf("unmarshal refresh response: %v (body %s)", err, body)
	}
	accessToken, ok := refreshResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty string", refreshResp["access_token"])
	}
	if !strings.HasPrefix(accessToken, "rdtok_") {
		t.Fatalf("access_token = %q, want rdtok_* prefix", accessToken)
	}
	// Reddit does NOT issue a new refresh_token on a refresh grant.
	if _, exists := refreshResp["refresh_token"]; exists {
		t.Fatalf("refresh grant must NOT return a new refresh_token; got %v", refreshResp["refresh_token"])
	}
	if refreshResp["token_type"] != "bearer" {
		t.Fatalf("token_type = %v, want bearer", refreshResp["token_type"])
	}
	if refreshResp["expires_in"] != float64(3600) {
		t.Fatalf("expires_in = %v, want 3600", refreshResp["expires_in"])
	}
	if refreshResp["scope"] != "submit identity" {
		t.Fatalf("scope = %v, want 'submit identity'", refreshResp["scope"])
	}

	// ===== Refresh with invalid refresh_token → 400 invalid_grant =====

	body, status = redditPostFormUA(t, base+"/api/v1/access_token", basicAuth, userAgent, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"rdref_nonexistent"},
	})
	if status != 400 {
		t.Fatalf("invalid refresh_token -> status %d, want 400; body %s", status, body)
	}

	// ===== access_token without Basic auth → 401 invalid_client =====

	body, status = redditPostFormUA(t, base+"/api/v1/access_token", "", userAgent, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
	if status != 401 {
		t.Fatalf("no Basic -> status %d, want 401; body %s", status, body)
	}
	var noBasicResp map[string]any
	if err := json.Unmarshal([]byte(body), &noBasicResp); err != nil {
		t.Fatalf("unmarshal no-basic response: %v (body %s)", err, body)
	}
	if noBasicResp["error"] != "invalid_client" {
		t.Fatalf("error = %v, want invalid_client", noBasicResp["error"])
	}

	// ===== Submit valid post → {json:{errors:[], data:{id,url,name}}} =====

	body, status = redditPostFormBearerUA(t, base+"/api/submit", accessToken, userAgent, url.Values{
		"sr":    {"test"},
		"title": {"Hello from the stunt test suite!"},
		"text":  {"body text"},
		"kind":  {"self"},
	})
	if status != 200 {
		t.Fatalf("submit -> status %d, want 200; body %s", status, body)
	}
	var submitResp map[string]any
	if err := json.Unmarshal([]byte(body), &submitResp); err != nil {
		t.Fatalf("unmarshal submit response: %v (body %s)", err, body)
	}
	jsonObj, ok := submitResp["json"].(map[string]any)
	if !ok {
		t.Fatalf("submit response missing json key: %v", submitResp)
	}
	errorsArr, ok := jsonObj["errors"].([]any)
	if !ok {
		t.Fatalf("json.errors = %v, want list", jsonObj["errors"])
	}
	if len(errorsArr) != 0 {
		t.Fatalf("json.errors = %v, want empty", jsonObj["errors"])
	}
	data, ok := jsonObj["data"].(map[string]any)
	if !ok {
		t.Fatalf("json.data = %v, want map", jsonObj["data"])
	}
	postID, ok := data["id"].(string)
	if !ok || postID == "" {
		t.Fatalf("json.data.id = %v, want non-empty string", data["id"])
	}
	postURL, ok := data["url"].(string)
	if !ok || !strings.Contains(postURL, "/r/test/comments/") {
		t.Fatalf("json.data.url = %v, want URL containing /r/test/comments/", data["url"])
	}
	postName, ok := data["name"].(string)
	if !ok || postName != "t3_"+postID {
		t.Fatalf("json.data.name = %v, want t3_%s", data["name"], postID)
	}

	// ===== Submit missing title → non-empty errors[] (HTTP 200) =====

	body, status = redditPostFormBearerUA(t, base+"/api/submit", accessToken, userAgent, url.Values{
		"sr": {"test"},
	})
	if status != 200 {
		t.Fatalf("submit missing title -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &submitResp); err != nil {
		t.Fatalf("unmarshal submit response: %v (body %s)", err, body)
	}
	jsonObj, _ = submitResp["json"].(map[string]any)
	errorsArr, _ = jsonObj["errors"].([]any)
	if len(errorsArr) == 0 {
		t.Fatalf("submit missing title: json.errors empty, want non-empty; body %s", body)
	}

	// ===== Submit missing subreddit → non-empty errors[] (HTTP 200) =====

	body, status = redditPostFormBearerUA(t, base+"/api/submit", accessToken, userAgent, url.Values{
		"title": {"Some Title"},
	})
	if status != 200 {
		t.Fatalf("submit missing sr -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &submitResp); err != nil {
		t.Fatalf("unmarshal submit response: %v (body %s)", err, body)
	}
	jsonObj, _ = submitResp["json"].(map[string]any)
	errorsArr, _ = jsonObj["errors"].([]any)
	if len(errorsArr) == 0 {
		t.Fatalf("submit missing sr: json.errors empty, want non-empty; body %s", body)
	}

	// ===== Submit without bearer → 401 USER_REQUIRED =====

	body, status = redditPostFormUA(t, base+"/api/submit", "", userAgent, url.Values{
		"sr":    {"test"},
		"title": {"Title"},
	})
	if status != 401 {
		t.Fatalf("submit without bearer -> status %d, want 401; body %s", status, body)
	}

	// ===== Missing User-Agent on access_token → 429 =====

	body, status = redditPostFormBearerNoUA(t, base+"/api/v1/access_token", basicAuth, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
	if status != 429 {
		t.Fatalf("access_token without UA -> status %d, want 429; body %s", status, body)
	}
	var uaMissingResp map[string]any
	if err := json.Unmarshal([]byte(body), &uaMissingResp); err != nil {
		t.Fatalf("unmarshal UA-missing response: %v (body %s)", err, body)
	}
	if uaMissingResp["error"] != float64(429) {
		t.Fatalf("UA-missing error = %v, want 429", uaMissingResp["error"])
	}

	// ===== Generic User-Agent (no parens) on submit → 429 =====

	body, status = redditPostFormBearerUA(t, base+"/api/submit", accessToken, "python-requests/2.31.0", url.Values{
		"sr":    {"test"},
		"title": {"Title"},
	})
	if status != 429 {
		t.Fatalf("submit with generic UA -> status %d, want 429; body %s", status, body)
	}

	// Sanity: the seed access token also works for submit (any rdtok_* is valid).
	body, status = redditPostFormBearerUA(t, base+"/api/submit", seedAccess, userAgent, url.Values{
		"sr":    {"test"},
		"title": {"Works with seed token"},
	})
	if status != 200 {
		t.Fatalf("submit with seed token -> status %d, want 200; body %s", status, body)
	}
}

// === Helpers ===

// redditPostFormUA performs a POST with form body, optional Authorization
// header (Basic or empty), and a User-Agent header.
func redditPostFormUA(t *testing.T, target, authHeader, userAgent string, form url.Values) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", target, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// redditPostFormBearerUA performs a POST with Bearer auth, User-Agent, and form body.
func redditPostFormBearerUA(t *testing.T, target, token, userAgent string, form url.Values) (string, int) {
	t.Helper()
	return redditPostFormUA(t, target, "Bearer "+token, userAgent, form)
}

// redditPostFormBearerNoUA performs a POST with auth header but NO User-Agent.
func redditPostFormBearerNoUA(t *testing.T, target, authHeader string, form url.Values) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", target, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", authHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
