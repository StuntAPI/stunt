package adapters

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// Drives the x-articles-style adapter scripts directly (lib.star preloaded)
// over a shared store and a virtual clock: the PKCE OAuth2 authorize/token
// dance with single-use codes and rotating refresh tokens, the article
// draft → read-back → publish lifecycle with media-upload cover attachment,
// and the x:tweet surface (280-char limit + reply-chain integrity).
//
// This adapter is a port of a reference client's mock, not the v2 wire
// shapes twitter-style models: its error bodies are {"error"} /
// {"title","detail"} rather than v2's errors[] envelope, /2/media/upload
// wraps media_id_string under data (per media.star's docstring), and the
// bearer gate is never enforced. Those are asserted as-is so a future
// alignment flips them loudly.
const (
	// Any "Basic " prefix passes: the mock never decodes the credentials.
	xArticlesBasic = "Basic dGVzdC1jbGllbnQ6dGVzdC1zZWNyZXQ="
	xArticlesAuth  = "Bearer mock-access-token"
)

type xArticlesFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newXArticlesFixture(t *testing.T, start time.Time) *xArticlesFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "x-articles-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	tmp := t.TempDir()
	store, _ := primitives.Open(filepath.Join(tmp, "s.db"))
	t.Cleanup(func() { store.Close() })
	kvStore, _ := kv.Open(filepath.Join(tmp, "s.kv.db"))
	t.Cleanup(func() { kvStore.Close() })
	blobStore, _ := blob.Open(filepath.Join(tmp, "blobs"))
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: events.NewEmitter(),
	})
	load := func(script string) *starlark.VM {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(root, "scripts", script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
		if err != nil {
			t.Fatalf("LoadWithLib %s: %v", script, err)
		}
		return vm
	}
	return &xArticlesFixture{t: t, vc: vc, host: "api.x.test", vms: map[string]*starlark.VM{
		"oauth": load("oauth.star"), "articles": load("articles.star"),
		"media": load("media.star"), "tweets": load("tweets.star"),
	}}
}

func (f *xArticlesFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// --- fixture drivers ---

// authorize runs the PKCE authorize redirect for redirect_uri.
func (f *xArticlesFixture) authorize(redirect, state, challenge string) starlark.Response {
	f.t.Helper()
	return f.call("oauth", "on_authorize", "GET", "/2/oauth2/authorize", nil, map[string]string{
		"redirect_uri": redirect, "state": state, "code_challenge": challenge, "code_challenge_method": "S256",
	}, nil, "")
}

// authorizeCode authorizes and extracts the code from the Location header.
func (f *xArticlesFixture) authorizeCode(redirect, state, challenge string) string {
	f.t.Helper()
	r := f.authorize(redirect, state, challenge)
	if r.Status != 302 {
		f.t.Fatalf("authorize -> %d: %v", r.Status, r.Body)
	}
	u, err := url.Parse(r.Headers["Location"])
	if err != nil {
		f.t.Fatalf("Location %q: %v", r.Headers["Location"], err)
	}
	if got := u.Query().Get("state"); got != state {
		f.t.Fatalf("redirect state = %q, want %q", got, state)
	}
	code := u.Query().Get("code")
	if code == "" {
		f.t.Fatalf("redirect carries no code: %q", r.Headers["Location"])
	}
	return code
}

// token exchanges (or refreshes) at the token endpoint.
func (f *xArticlesFixture) token(body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	return f.call("oauth", "on_token", "POST", "/2/oauth2/token", nil, nil, body, auth)
}

// uploadMedia POSTs a blob and returns its media_id_string.
func (f *xArticlesFixture) uploadMedia() string {
	f.t.Helper()
	r := f.call("media", "on_upload", "POST", "/2/media/upload", nil, nil, nil, xArticlesAuth)
	if r.Status != 200 {
		f.t.Fatalf("media upload -> %d: %v", r.Status, r.Body)
	}
	id, _ := r.Body["data"].(map[string]any)["media_id_string"].(string)
	if id == "" {
		f.t.Fatalf("upload returned no media_id_string: %v", r.Body)
	}
	return id
}

// draft creates an article draft and returns its id. A nil blocks list is
// replaced with one paragraph block.
func (f *xArticlesFixture) draft(title, cover string, blocks []any) starlark.Response {
	f.t.Helper()
	if blocks == nil {
		blocks = []any{map[string]any{"type": "paragraph", "text": "Suite body text"}}
	}
	cs := map[string]any{"blocks": blocks}
	body := map[string]any{"title": title, "content_state": cs}
	if cover != "" {
		body["cover_media_id"] = cover
	}
	return f.call("articles", "on_draft", "POST", "/2/articles/draft", nil, nil, body, xArticlesAuth)
}

// createDraft drafts and returns the minted article id.
func (f *xArticlesFixture) createDraft(title, cover string) string {
	f.t.Helper()
	r := f.draft(title, cover, nil)
	if r.Status != 200 {
		f.t.Fatalf("draft -> %d: %v", r.Status, r.Body)
	}
	id, _ := r.Body["data"].(map[string]any)["id"].(string)
	if id == "" {
		f.t.Fatalf("draft returned no id: %v", r.Body)
	}
	return id
}

// publish POSTs the publish action for id.
func (f *xArticlesFixture) publish(id string) starlark.Response {
	f.t.Helper()
	return f.call("articles", "on_publish", "POST", "/2/articles/"+id+"/publish", map[string]string{"id": id}, nil, nil, xArticlesAuth)
}

// getArticle runs GET /2/articles/{id}.
func (f *xArticlesFixture) getArticle(id string) starlark.Response {
	f.t.Helper()
	return f.call("articles", "on_get", "GET", "/2/articles/"+id, map[string]string{"id": id}, nil, nil, xArticlesAuth)
}

// createTweet POSTs a tweet and returns its id.
func (f *xArticlesFixture) createTweet(text, replyTo string) string {
	f.t.Helper()
	body := map[string]any{"text": text}
	if replyTo != "" {
		body["reply"] = map[string]any{"in_reply_to_tweet_id": replyTo}
	}
	r := f.call("tweets", "on_create", "POST", "/2/tweets", nil, nil, body, xArticlesAuth)
	if r.Status != 201 {
		f.t.Fatalf("create tweet -> %d: %v", r.Status, r.Body)
	}
	id, _ := r.Body["data"].(map[string]any)["id"].(string)
	if id == "" {
		f.t.Fatalf("create tweet returned no id: %v", r.Body)
	}
	return id
}

// getTweet runs GET /2/tweets/{id}.
func (f *xArticlesFixture) getTweet(id string) starlark.Response {
	f.t.Helper()
	return f.call("tweets", "on_retrieve", "GET", "/2/tweets/"+id, map[string]string{"id": id}, nil, nil, xArticlesAuth)
}

// --- assertion helpers ---

// xArticlesData returns the {data} object of a 2xx response.
func xArticlesData(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	if r.Status < 200 || r.Status > 299 {
		t.Fatalf("want 2xx, got %d: %v", r.Status, r.Body)
	}
	data, ok := r.Body["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v, want object", r.Body["data"])
	}
	return data
}

// xArticlesErrString asserts the article/media endpoints' {"error": msg}
// body — the reference-mock envelope, not v2's errors[].
func xArticlesErrString(t *testing.T, r starlark.Response, status int, msg string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("want %d, got %d: %v", status, r.Status, r.Body)
	}
	if r.Body["error"] != msg {
		t.Fatalf("error = %v, want %q", r.Body["error"], msg)
	}
}

// xArticlesTweetErr asserts the tweet endpoints' {title, detail} error body
// — again the reference-mock envelope, not v2's errors[].
func xArticlesTweetErr(t *testing.T, r starlark.Response, status int, title, detailPart string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("want %d, got %d: %v", status, r.Status, r.Body)
	}
	if r.Body["title"] != title {
		t.Fatalf("title = %v, want %q", r.Body["title"], title)
	}
	detail, _ := r.Body["detail"].(string)
	if !strings.Contains(detail, detailPart) {
		t.Fatalf("detail = %q, want it to contain %q", detail, detailPart)
	}
}

// xArticlesNum compares a JSON number regardless of int64/float64 width
// (values round-trip through the collection store as floats).
func xArticlesNum(t *testing.T, v any, want float64, what string) {
	t.Helper()
	switch n := v.(type) {
	case int64:
		if float64(n) != want {
			t.Fatalf("%s = %d, want %v", what, n, want)
		}
	case float64:
		if n != want {
			t.Fatalf("%s = %v, want %v", what, n, want)
		}
	default:
		t.Fatalf("%s = %T(%v), want number %v", what, v, v, want)
	}
}

// TestXArticlesPKCEOAuth: the confidential-client authorization-code flow —
// the 302 code redirect, the Basic/none gate, single-use codes, the
// redirect_uri match, the relaxed PKCE presence check, and rotating
// refresh tokens.
func TestXArticlesPKCEOAuth(t *testing.T) {
	f := newXArticlesFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	redirect := "https://client.example.test/callback"

	// ===== authorize 302s to the redirect_uri carrying a fresh code and the echoed state =====
	// The redirect joins with ? (or & when the uri already has a query) and
	// the code is minted from the KV sequence, so authorize is repeatable.
	first := f.authorizeCode(redirect, "st-alpha", "challenge-alpha")
	second := f.authorizeCode(redirect+"?src=vm", "st-beta", "challenge-beta")
	if first == second || !strings.HasPrefix(first, "code_") || !strings.HasPrefix(second, "code_") {
		t.Fatalf("codes = %q / %q, want distinct code_-prefixed values", first, second)
	}

	// ===== authorize demands redirect_uri, the S256 method and a non-empty challenge =====
	for _, drop := range []string{"redirect_uri", "code_challenge", "code_challenge_method"} {
		q := map[string]string{
			"redirect_uri": redirect, "state": "st", "code_challenge": "chal", "code_challenge_method": "S256",
		}
		delete(q, drop)
		xArticlesErrString(t, f.call("oauth", "on_authorize", "GET", "/2/oauth2/authorize", nil, q, nil, ""),
			400, "invalid_request")
	}
	// plain (unhashed) challenges are refused too.
	xArticlesErrString(t, f.call("oauth", "on_authorize", "GET", "/2/oauth2/authorize", nil, map[string]string{
		"redirect_uri": redirect, "state": "st", "code_challenge": "chal", "code_challenge_method": "plain",
	}, nil, ""), 400, "invalid_request")

	// ===== the token endpoint demands HTTP Basic client creds and a known grant type =====
	xArticlesErrString(t, f.token(map[string]any{"grant_type": "authorization_code", "code": first}, ""),
		401, "invalid_client")
	xArticlesErrString(t, f.token(map[string]any{"grant_type": "password"}, xArticlesBasic),
		400, "unsupported_grant_type")

	// ===== a valid code exchange mints a bearer+refresh pair and the code is single-use =====
	// The exchange carries the same redirect_uri and any non-empty verifier.
	pair := f.token(map[string]any{
		"grant_type": "authorization_code", "code": second, "redirect_uri": redirect + "?src=vm",
		"code_verifier": "verifier-whatever",
	}, xArticlesBasic)
	if pair.Status != 200 {
		t.Fatalf("token exchange -> %d: %v", pair.Status, pair.Body)
	}
	if pair.Body["token_type"] != "bearer" || pair.Body["scope"] != "tweet.read tweet.write users.read offline.access" {
		t.Fatalf("token pair = %v", pair.Body)
	}
	xArticlesNum(t, pair.Body["expires_in"], 7200, "expires_in")
	access, _ := pair.Body["access_token"].(string)
	refresh, _ := pair.Body["refresh_token"].(string)
	if !strings.HasPrefix(access, "mock_access_") || !strings.HasPrefix(refresh, "mock_refresh_") {
		t.Fatalf("access/refresh = %q / %q, want mock_-prefixed", access, refresh)
	}
	// Replay: the code was consumed by the first exchange.
	xArticlesErrString(t, f.token(map[string]any{
		"grant_type": "authorization_code", "code": second, "redirect_uri": redirect + "?src=vm",
		"code_verifier": "verifier-whatever",
	}, xArticlesBasic), 400, "invalid_grant")

	// ===== a mismatched redirect_uri or a missing verifier fails without burning the code =====
	mismatched := f.authorizeCode(redirect, "st-gamma", "challenge-gamma")
	xArticlesErrString(t, f.token(map[string]any{
		"grant_type": "authorization_code", "code": mismatched, "redirect_uri": "https://evil.example.test/cb",
		"code_verifier": "verifier-whatever",
	}, xArticlesBasic), 400, "invalid_grant")
	bare := f.authorizeCode(redirect, "st-delta", "challenge-delta")
	xArticlesErrString(t, f.token(map[string]any{
		"grant_type": "authorization_code", "code": bare, "redirect_uri": redirect,
	}, xArticlesBasic), 400, "invalid_grant")
	// House rule: the failed attempts above must not have consumed the
	// codes — the corrected exchanges still succeed.
	if r := f.token(map[string]any{
		"grant_type": "authorization_code", "code": mismatched, "redirect_uri": redirect,
		"code_verifier": "verifier-ok",
	}, xArticlesBasic); r.Status != 200 {
		t.Fatalf("corrected exchange after a mismatched attempt -> %d: %v (code should survive)", r.Status, r.Body)
	}
	// As-is (documented relaxation): a wrong-but-present verifier passes —
	// the S256 hash comparison is not performed.
	ok := f.token(map[string]any{
		"grant_type": "authorization_code", "code": f.authorizeCode(redirect, "st-eps", "challenge-eps"),
		"redirect_uri": redirect, "code_verifier": "definitely-not-the-matching-verifier",
	}, xArticlesBasic)
	if ok.Status != 200 {
		t.Fatalf("relaxed PKCE exchange -> %d: %v", ok.Status, ok.Body)
	}

	// ===== refresh grants rotate the pair and retire the presented token =====
	rotated := f.token(map[string]any{"grant_type": "refresh_token", "refresh_token": refresh}, xArticlesBasic)
	if rotated.Status != 200 {
		t.Fatalf("refresh -> %d: %v", rotated.Status, rotated.Body)
	}
	newAccess, _ := rotated.Body["access_token"].(string)
	newRefresh, _ := rotated.Body["refresh_token"].(string)
	if newAccess == access || newRefresh == refresh || newAccess == "" || newRefresh == "" {
		t.Fatalf("rotation returned the same tokens: %v", rotated.Body)
	}
	xArticlesNum(t, rotated.Body["expires_in"], 7200, "refresh expires_in")
	xArticlesErrString(t, f.token(map[string]any{"grant_type": "refresh_token", "refresh_token": refresh}, xArticlesBasic),
		400, "invalid_grant")
	xArticlesErrString(t, f.token(map[string]any{"grant_type": "refresh_token", "refresh_token": "mock_refresh_nope"}, xArticlesBasic),
		400, "invalid_grant")
}

// TestXArticlesDraftPublishFlow: the never-enforced bearer gate, the
// draft→read-back→publish lifecycle, media upload feeding cover_media_id,
// republish, and the article 404s.
func TestXArticlesDraftPublishFlow(t *testing.T) {
	f := newXArticlesFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== the bearer gate is never enforced =====
	// As-is: every endpoint accepts any (or no) Authorization header — the
	// manifest documents the mock as "not enforced". The real API answers
	// 401 without a valid bearer.
	if noAuth := f.call("articles", "on_get", "GET", "/2/articles/a_nope",
		map[string]string{"id": "a_nope"}, nil, nil, ""); noAuth.Status != 404 {
		t.Fatalf("no Authorization header -> %d, want 404 (auth not enforced)", noAuth.Status)
	}
	if bogus := f.call("articles", "on_draft", "POST", "/2/articles/draft", nil, nil,
		map[string]any{"title": "Gated?", "content_state": map[string]any{"blocks": []any{map[string]any{"type": "paragraph"}}}},
		"Bearer totally-bogus"); bogus.Status != 200 {
		t.Fatalf("bogus bearer draft -> %d, want 200 (auth not enforced)", bogus.Status)
	}

	// ===== media upload mints media_id_strings that attach as cover_media_id =====
	// As-is: the blob bytes are discarded (media.star) and the id wraps
	// under data.media_id_string rather than the real top-level shape.
	cover := f.uploadMedia()
	other := f.uploadMedia()
	if !strings.HasPrefix(cover, "m_") || cover == other {
		t.Fatalf("media ids = %q / %q, want distinct m_-prefixed values", cover, other)
	}

	// ===== draft create validates title and content_state.blocks =====
	// The literal /2/articles/draft route is declared before /2/articles/{id}
	// so the word "draft" is never treated as an article id.
	xArticlesErrString(t, f.draft("", "", nil), 400, "title is required")
	xArticlesErrString(t, f.draft("   ", "", nil), 400, "title is required")
	xArticlesErrString(t, f.draft("No blocks", "", []any{}), 400, "content_state.blocks is required")
	noCS := f.call("articles", "on_draft", "POST", "/2/articles/draft", nil, nil,
		map[string]any{"title": "No content_state"}, xArticlesAuth)
	xArticlesErrString(t, noCS, 400, "content_state.blocks is required")
	created := xArticlesData(t, f.draft("VM Suite Longread", cover, nil))
	id, _ := created["id"].(string)
	if id == "" || created["title"] != "VM Suite Longread" {
		t.Fatalf("draft data = %v, want {id,title}", created)
	}

	// ===== a draft reads back with its full metadata =====
	doc := xArticlesData(t, f.getArticle(id))
	if doc["id"] != id || doc["title"] != "VM Suite Longread" || doc["published"] != false {
		t.Fatalf("draft read-back = %v", doc)
	}
	if doc["post_id"] != nil || doc["cover_media_id"] != cover {
		t.Fatalf("draft post_id/cover = %v / %v, want nil / %s", doc["post_id"], doc["cover_media_id"], cover)
	}
	cs, ok := doc["content_state"].(map[string]any)
	if !ok {
		t.Fatalf("content_state = %v, want object", doc["content_state"])
	}
	blocks, _ := cs["blocks"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %v, want the round-tripped paragraph", cs["blocks"])
	}
	if block, _ := blocks[0].(map[string]any); block["text"] != "Suite body text" {
		t.Fatalf("blocks[0] = %v, want the round-tripped paragraph text", blocks[0])
	}
	// A coverless draft reads back a null cover_media_id.
	bare := xArticlesData(t, f.getArticle(f.createDraft("Coverless", "")))
	if v, present := bare["cover_media_id"]; !present || v != nil {
		t.Fatalf("coverless draft cover_media_id = %v (present=%v), want JSON null", v, present)
	}

	// ===== publish flips the draft and attaches the minted post_id =====
	pub := xArticlesData(t, f.publish(id))
	postID, _ := pub["post_id"].(string)
	if postID == "" || !strings.HasPrefix(postID, "p_") {
		t.Fatalf("publish data = %v, want a p_-prefixed post_id", pub)
	}
	published := xArticlesData(t, f.getArticle(id))
	if published["published"] != true || published["post_id"] != postID {
		t.Fatalf("published read-back = %v, want published=true post_id=%s", published, postID)
	}

	// ===== republishing mints a fresh post_id =====
	// As-is: a second publish is accepted and re-mints (no already-published
	// guard); the read-back keeps only the newest post_id.
	again := xArticlesData(t, f.publish(id))
	newPostID, _ := again["post_id"].(string)
	if newPostID == postID || !strings.HasPrefix(newPostID, "p_") {
		t.Fatalf("republish post_id = %v, want a fresh p_-prefixed id", again["post_id"])
	}
	if latest := xArticlesData(t, f.getArticle(id)); latest["post_id"] != newPostID {
		t.Fatalf("post-publish read-back post_id = %v, want the newest %s", latest["post_id"], newPostID)
	}

	// ===== unknown article ids are 404s on get and publish =====
	xArticlesErrString(t, f.getArticle("a_nope"), 404, "article not found")
	xArticlesErrString(t, f.publish("a_nope"), 404, "article not found")
}

// TestXArticlesTweetSurface: the x:tweet endpoints — required text, the
// 280-char ceiling, reply-chain integrity, and read-by-id.
func TestXArticlesTweetSurface(t *testing.T) {
	f := newXArticlesFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== create enforces required text and the inclusive 280-char limit =====
	xArticlesTweetErr(t, f.call("tweets", "on_create", "POST", "/2/tweets", nil, nil,
		map[string]any{}, xArticlesAuth), 400, "Invalid Request", "text is required")
	xArticlesTweetErr(t, f.call("tweets", "on_create", "POST", "/2/tweets", nil, nil,
		map[string]any{"text": "   "}, xArticlesAuth), 400, "Invalid Request", "text is required")
	xArticlesTweetErr(t, f.call("tweets", "on_create", "POST", "/2/tweets", nil, nil,
		map[string]any{"text": strings.Repeat("a", 281)}, xArticlesAuth), 400, "Invalid Request", "281 chars (max 280)")
	if r := f.call("tweets", "on_create", "POST", "/2/tweets", nil, nil,
		map[string]any{"text": strings.Repeat("b", 280)}, xArticlesAuth); r.Status != 201 {
		t.Fatalf("280-char tweet (inclusive limit) -> %d: %v", r.Status, r.Body)
	}

	// ===== replies must target a known tweet =====
	xArticlesTweetErr(t, f.call("tweets", "on_create", "POST", "/2/tweets", nil, nil, map[string]any{
		"text": "replying into the void", "reply": map[string]any{"in_reply_to_tweet_id": "tweet_nope"},
	}, xArticlesAuth), 400, "Invalid Request", "in_reply_to_tweet_id tweet_nope not found")
	root := f.createTweet("The longread is live", "")
	reply := f.createTweet("threading the needle", root)
	if !strings.HasPrefix(root, "tweet_") || !strings.HasPrefix(reply, "tweet_") || root == reply {
		t.Fatalf("tweet ids = %q / %q, want distinct tweet_-prefixed values", root, reply)
	}

	// ===== tweets read back whole and unknown ids are 404s =====
	doc := xArticlesData(t, f.getTweet(reply))
	if doc["id"] != reply || doc["text"] != "threading the needle" || doc["in_reply_to_tweet_id"] != root {
		t.Fatalf("tweet read-back = %v", doc)
	}
	rootDoc := xArticlesData(t, f.getTweet(root))
	if rootDoc["in_reply_to_tweet_id"] != nil {
		t.Fatalf("root tweet in_reply_to_tweet_id = %v, want null", rootDoc["in_reply_to_tweet_id"])
	}
	xArticlesTweetErr(t, f.getTweet("tweet_nope"), 404, "Not Found", "tweet not found")
}
