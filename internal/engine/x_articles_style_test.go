package engine

import (
	"context"
	"crypto/sha256"
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

// TestXArticlesStyleAdapter exercises the X Articles reference adapter
// end-to-end through a real HTTP client, asserting it faithfully reproduces
// the Python mock_x_api contract:
//
//   - draft (valid -> {data:{id,title}}; empty title -> 400; missing blocks -> 400)
//   - publish (-> {data:{post_id}}; unknown id -> 404)
//   - get article metadata (-> full data incl. published/post_id)
//   - media upload (-> media_id_string)
//   - tweet (>280 -> 400; reply to unknown -> 400; valid -> 201; retrieve)
//   - oauth authorize (302 with code) + token (Basic required; bad code -> 400;
//     missing code_verifier -> 400; valid -> token pair)
func TestXArticlesStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "x-articles-style")
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
			"xarticles": {Adapter: absAdapterDir},
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

	base := addrs["xarticles"]

	const (
		clientID     = "test-client-id"
		clientSecret = "test-client-secret"
	)
	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(clientID+":"+clientSecret))

	// ===== Articles: draft =====

	// POST /2/articles/draft (valid) -> 200 { data:{ id, title } }
	draftBody := map[string]any{
		"title": "My First Long-Form Article",
		"content_state": map[string]any{
			"blocks": []any{
				map[string]any{"type": "unstyled", "text": "Hello world"},
			},
		},
	}
	body, status := xPostJSON(t, base+"/2/articles/draft", draftBody)
	if status != 200 {
		t.Fatalf("POST /2/articles/draft -> status %d, want 200; body %s", status, body)
	}
	var draftResp map[string]any
	if err := json.Unmarshal([]byte(body), &draftResp); err != nil {
		t.Fatalf("unmarshal draft response: %v (body %s)", err, body)
	}
	draftData, ok := draftResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("draft data = %v, want a dict", draftResp["data"])
	}
	articleID, ok := draftData["id"].(string)
	if !ok || !strings.HasPrefix(articleID, "a_") {
		t.Fatalf("article id = %v, want a_* prefix", draftData["id"])
	}
	if draftData["title"] != "My First Long-Form Article" {
		t.Fatalf("article title = %v, want 'My First Long-Form Article'", draftData["title"])
	}

	// POST /2/articles/draft (empty title) -> 400
	body, status = xPostJSON(t, base+"/2/articles/draft", map[string]any{
		"title": "",
		"content_state": map[string]any{
			"blocks": []any{map[string]any{"type": "unstyled"}},
		},
	})
	if status != 400 {
		t.Fatalf("empty title -> status %d, want 400; body %s", status, body)
	}
	var errResp map[string]any
	json.Unmarshal([]byte(body), &errResp)
	if errResp["error"] != "title is required" {
		t.Fatalf("empty title error = %v, want 'title is required'", errResp["error"])
	}

	// POST /2/articles/draft (missing content_state.blocks) -> 400
	body, status = xPostJSON(t, base+"/2/articles/draft", map[string]any{
		"title": "Has Title",
	})
	if status != 400 {
		t.Fatalf("missing blocks -> status %d, want 400; body %s", status, body)
	}

	// ===== Articles: publish =====

	// POST /2/articles/{id}/publish -> 200 { data:{ post_id } }
	body, status = xPostJSON(t, base+"/2/articles/"+articleID+"/publish", nil)
	if status != 200 {
		t.Fatalf("publish -> status %d, want 200; body %s", status, body)
	}
	var pubResp map[string]any
	if err := json.Unmarshal([]byte(body), &pubResp); err != nil {
		t.Fatalf("unmarshal publish response: %v (body %s)", err, body)
	}
	pubData, ok := pubResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("publish data = %v, want a dict", pubResp["data"])
	}
	postID, ok := pubData["post_id"].(string)
	if !ok || !strings.HasPrefix(postID, "p_") {
		t.Fatalf("post_id = %v, want p_* prefix", pubData["post_id"])
	}

	// POST /2/articles/{unknown}/publish -> 404
	body, status = xPostJSON(t, base+"/2/articles/a_doesnotexist/publish", nil)
	if status != 404 {
		t.Fatalf("publish unknown -> status %d, want 404; body %s", status, body)
	}

	// ===== Articles: get metadata =====

	// GET /2/articles/{id} -> 200 with full data (published=true, post_id set)
	body, status = xGet(t, base+"/2/articles/"+articleID)
	if status != 200 {
		t.Fatalf("GET article -> status %d, want 200; body %s", status, body)
	}
	var articleMeta map[string]any
	if err := json.Unmarshal([]byte(body), &articleMeta); err != nil {
		t.Fatalf("unmarshal article meta: %v (body %s)", err, body)
	}
	metaData, ok := articleMeta["data"].(map[string]any)
	if !ok {
		t.Fatalf("article meta data = %v, want a dict", articleMeta["data"])
	}
	if metaData["published"] != true {
		t.Fatalf("published = %v, want true", metaData["published"])
	}
	if metaData["post_id"] != postID {
		t.Fatalf("post_id = %v, want %s", metaData["post_id"], postID)
	}
	if metaData["title"] != "My First Long-Form Article" {
		t.Fatalf("title = %v, want original title", metaData["title"])
	}

	// GET /2/articles/{unknown} -> 404
	body, status = xGet(t, base+"/2/articles/a_nope")
	if status != 404 {
		t.Fatalf("GET unknown article -> status %d, want 404; body %s", status, body)
	}

	// ===== Media upload =====

	// POST /2/media/upload (octet-stream) -> 200 { data:{ media_id_string } }
	body, status = xPostRaw(t, base+"/2/media/upload", "application/octet-stream", []byte("fake-image-bytes"))
	if status != 200 {
		t.Fatalf("media upload -> status %d, want 200; body %s", status, body)
	}
	var mediaResp map[string]any
	if err := json.Unmarshal([]byte(body), &mediaResp); err != nil {
		t.Fatalf("unmarshal media response: %v (body %s)", err, body)
	}
	mediaData, ok := mediaResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("media data = %v, want a dict", mediaResp["data"])
	}
	mediaID, ok := mediaData["media_id_string"].(string)
	if !ok || !strings.HasPrefix(mediaID, "m_") {
		t.Fatalf("media_id_string = %v, want m_* prefix", mediaData["media_id_string"])
	}

	// ===== Draft with cover_media_id (full pipeline) =====

	body, status = xPostJSON(t, base+"/2/articles/draft", map[string]any{
		"title": "Article With Cover",
		"content_state": map[string]any{
			"blocks": []any{map[string]any{"type": "header-one", "text": "Title"}},
		},
		"cover_media_id": mediaID,
	})
	if status != 200 {
		t.Fatalf("draft with cover -> status %d, want 200; body %s", status, body)
	}
	var coverDraft map[string]any
	json.Unmarshal([]byte(body), &coverDraft)
	coverArticleID := coverDraft["data"].(map[string]any)["id"].(string)

	// Verify cover_media_id is stored.
	body, _ = xGet(t, base+"/2/articles/"+coverArticleID)
	var coverMeta map[string]any
	json.Unmarshal([]byte(body), &coverMeta)
	coverData := coverMeta["data"].(map[string]any)
	if coverData["cover_media_id"] != mediaID {
		t.Fatalf("cover_media_id = %v, want %s", coverData["cover_media_id"], mediaID)
	}

	// ===== Tweets =====

	// POST /2/tweets (valid) -> 201 { data:{ id, text } }
	body, status = xPostJSON(t, base+"/2/tweets", map[string]any{
		"text": "Hello from the X Articles test suite!",
	})
	if status != 201 {
		t.Fatalf("create tweet -> status %d, want 201; body %s", status, body)
	}
	var tweetResp map[string]any
	if err := json.Unmarshal([]byte(body), &tweetResp); err != nil {
		t.Fatalf("unmarshal tweet response: %v (body %s)", err, body)
	}
	tweetData, ok := tweetResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("tweet data = %v, want a dict", tweetResp["data"])
	}
	tweetID, ok := tweetData["id"].(string)
	if !ok || !strings.HasPrefix(tweetID, "tweet_") {
		t.Fatalf("tweet id = %v, want tweet_* prefix", tweetData["id"])
	}
	if tweetData["text"] != "Hello from the X Articles test suite!" {
		t.Fatalf("tweet text = %v, want original text", tweetData["text"])
	}

	// POST /2/tweets (>280 chars) -> 400
	longText := strings.Repeat("x", 281)
	body, status = xPostJSON(t, base+"/2/tweets", map[string]any{
		"text": longText,
	})
	if status != 400 {
		t.Fatalf("over-limit tweet -> status %d, want 400; body %s", status, body)
	}

	// POST /2/tweets (reply to unknown tweet) -> 400
	body, status = xPostJSON(t, base+"/2/tweets", map[string]any{
		"text":  "a reply",
		"reply": map[string]any{"in_reply_to_tweet_id": "tweet_nonexistent"},
	})
	if status != 400 {
		t.Fatalf("reply to unknown -> status %d, want 400; body %s", status, body)
	}

	// POST /2/tweets (reply to known tweet) -> 201
	body, status = xPostJSON(t, base+"/2/tweets", map[string]any{
		"text":  "a valid reply",
		"reply": map[string]any{"in_reply_to_tweet_id": tweetID},
	})
	if status != 201 {
		t.Fatalf("reply to known -> status %d, want 201; body %s", status, body)
	}

	// GET /2/tweets/{id} -> 200
	body, status = xGet(t, base+"/2/tweets/"+tweetID)
	if status != 200 {
		t.Fatalf("GET tweet -> status %d, want 200; body %s", status, body)
	}

	// GET /2/tweets/{unknown} -> 404
	body, status = xGet(t, base+"/2/tweets/tweet_nope")
	if status != 404 {
		t.Fatalf("GET unknown tweet -> status %d, want 404; body %s", status, body)
	}

	// ===== OAuth2 PKCE =====

	// Generate a real S256 challenge/verifier pair.
	verifier := "a-very-secret-and-sufficiently-long-random-verifier-string-12345"
	challengeBytes := sha256.Sum256([]byte(verifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	redirectURI := "http://localhost:3000/callback"

	// GET /2/oauth2/authorize -> 302 redirect with code + state
	authCode := xOAuthAuthorize(t, base+"/2/oauth2/authorize", redirectURI, "my-state-123", codeChallenge)

	// POST /2/oauth2/token (no Basic) -> 401 invalid_client
	body, status = xPostForm(t, base+"/2/oauth2/token", "", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
	if status != 401 {
		t.Fatalf("token without Basic -> status %d, want 401; body %s", status, body)
	}

	// POST /2/oauth2/token (bad code) -> 400 invalid_grant
	body, status = xPostForm(t, base+"/2/oauth2/token", basicAuth, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"code_nonexistent"},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
	if status != 400 {
		t.Fatalf("token with bad code -> status %d, want 400; body %s", status, body)
	}
	var badCodeResp map[string]any
	json.Unmarshal([]byte(body), &badCodeResp)
	if badCodeResp["error"] != "invalid_grant" {
		t.Fatalf("bad code error = %v, want invalid_grant", badCodeResp["error"])
	}

	// POST /2/oauth2/token (missing code_verifier) -> 400 invalid_grant
	body, status = xPostForm(t, base+"/2/oauth2/token", basicAuth, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {authCode},
		"redirect_uri": {redirectURI},
	})
	if status != 400 {
		t.Fatalf("token without verifier -> status %d, want 400; body %s", status, body)
	}
	var noVerifierResp map[string]any
	json.Unmarshal([]byte(body), &noVerifierResp)
	if noVerifierResp["error"] != "invalid_grant" {
		t.Fatalf("missing verifier error = %v, want invalid_grant", noVerifierResp["error"])
	}

	// The missing-verifier test above consumed authCode (single-use; the
	// mock pops the code before checking the verifier, matching the Python
	// source). Get a fresh code for the valid exchange.
	freshCode := xOAuthAuthorize(t, base+"/2/oauth2/authorize", redirectURI, "my-state-456", codeChallenge)

	// POST /2/oauth2/token (valid) -> 200 token pair
	body, status = xPostForm(t, base+"/2/oauth2/token", basicAuth, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {freshCode},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
	if status != 200 {
		t.Fatalf("token exchange -> status %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token response: %v (body %s)", err, body)
	}
	if tokenResp["token_type"] != "bearer" {
		t.Fatalf("token_type = %v, want bearer", tokenResp["token_type"])
	}
	if at, ok := tokenResp["access_token"].(string); !ok || at == "" {
		t.Fatalf("access_token = %v, want non-empty string", tokenResp["access_token"])
	}
	if rt, ok := tokenResp["refresh_token"].(string); !ok || rt == "" {
		t.Fatalf("refresh_token = %v, want non-empty string", tokenResp["refresh_token"])
	}
	if tokenResp["expires_in"] != float64(7200) {
		t.Fatalf("expires_in = %v, want 7200", tokenResp["expires_in"])
	}
	if tokenResp["scope"] != "tweet.read tweet.write users.read offline.access" {
		t.Fatalf("scope = %v, want expected scopes", tokenResp["scope"])
	}

	// POST /2/oauth2/token (code reuse — single-use) -> 400 invalid_grant
	body, status = xPostForm(t, base+"/2/oauth2/token", basicAuth, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {freshCode},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
	if status != 400 {
		t.Fatalf("code reuse -> status %d, want 400; body %s", status, body)
	}
}

// === Helpers ===

// xPostJSON performs a POST with a JSON body.
func xPostJSON(t *testing.T, target string, payload any) (string, int) {
	t.Helper()
	var r io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		r = strings.NewReader(string(data))
	}
	req, err := http.NewRequest("POST", target, r)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// xPostRaw performs a POST with a raw body and explicit content type.
func xPostRaw(t *testing.T, target, contentType string, data []byte) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", target, strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// xGet performs a GET request, following redirects (DefaultClient follows).
func xGet(t *testing.T, target string) (string, int) {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// xPostForm performs a POST with a URL-encoded form body and optional
// Authorization header.
func xPostForm(t *testing.T, target, authHeader string, form url.Values) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", target, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// xOAuthAuthorize hits the authorize endpoint and extracts the code from the
// 302 redirect Location. It uses a custom client that does NOT follow
// redirects so the 302 can be inspected directly.
func xOAuthAuthorize(t *testing.T, target, redirectURI, state, codeChallenge string) string {
	t.Helper()
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // don't follow
		},
	}
	u := target + "?" + url.Values{
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}.Encode()
	resp, err := client.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 302 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("authorize -> status %d, want 302; body %s", resp.StatusCode, b)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("authorize: missing Location header")
	}
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse redirect location: %v", err)
	}
	q := parsed.Query()
	code := q.Get("code")
	if code == "" {
		t.Fatal("authorize: redirect missing code parameter")
	}
	if q.Get("state") != state {
		t.Fatalf("authorize: state = %q, want %q", q.Get("state"), state)
	}
	return code
}
