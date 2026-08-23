package adapters

import (
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

// These tests drive the instagram-style adapter scripts directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock: the Meta
// authorization-code OAuth flow, the access-token gate (Graph error envelope
// {message, type, code, fbtrace_id}), the token-bound /me profile, the
// two-step container/publish lifecycle (IN_PROGRESS -> FINISHED/ERROR on the
// clock), the media edge (newest-first, fields= projection, after-cursor
// paging), per-media insights metrics and the deterministic comments edge
// with its since filter, plus long-lived token refresh.

// One OAuth client for the whole suite; the adapter accepts any values.
const (
	igRedirectURI   = "http://localhost:3000/callback"
	igState         = "vm-suite-state"
	igClientID      = "ig-vm-client-id"
	igClientSecret  = "ig-vm-client-secret"
	igGraphStampFmt = "2006-01-02T15:04:05-0700" // Graph media timestamps: no colon in the offset
)

// igFixture is one shared store + virtual clock with a loaded VM per handler
// script (oauth, profile, publish, insights and comments each get their own
// VM, but they observe the same collections/kv state, like the engine).
type igFixture struct {
	t   *testing.T
	vc  *clock.Clock
	vms map[string]*starlark.VM
}

func newInstagramFixture(t *testing.T, start time.Time) *igFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "instagram-style")
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
	return &igFixture{t: t, vc: vc, vms: map[string]*starlark.VM{
		"oauth": load("oauth.star"), "profile": load("profile.star"),
		"publish": load("publish.star"), "insights": load("insights.star"),
		"comments": load("comments.star"),
	}}
}

// call invokes handler on the named script VM; auth is the full
// Authorization header value ("" = header absent).
func (f *igFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: "graph.instagram.test",
		Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// igAuthorize runs the authorize redirect against redirectURI and returns
// the single-use code it mints.
func (f *igFixture) igAuthorize(redirectURI string) string {
	f.t.Helper()
	r := f.call("oauth", "on_authorize", "GET", "/oauth/authorize", nil, map[string]string{
		"client_id": igClientID, "redirect_uri": redirectURI, "state": igState,
		"response_type": "code", "scope": "user_profile,user_media",
	}, nil, "")
	if r.Status != 302 {
		f.t.Fatalf("authorize -> %d: %v", r.Status, r.Body)
	}
	loc, err := url.Parse(r.Headers["Location"])
	if err != nil {
		f.t.Fatalf("authorize Location %q: %v", r.Headers["Location"], err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		f.t.Fatalf("authorize Location %q carries no code", r.Headers["Location"])
	}
	return code
}

// igExchange trades a code at the token endpoint; "" omits the field.
func (f *igFixture) igExchange(grant, code, clientID, secret, redirectURI string) starlark.Response {
	f.t.Helper()
	body := map[string]any{"grant_type": grant, "code": code}
	if clientID != "" {
		body["client_id"] = clientID
	}
	if secret != "" {
		body["client_secret"] = secret
	}
	if redirectURI != "" {
		body["redirect_uri"] = redirectURI
	}
	return f.call("oauth", "on_access_token", "POST", "/oauth/access_token", nil, nil, body, "")
}

// igMintUser runs the authorization-code flow end to end and returns the
// minted (token, ig_user_id).
func (f *igFixture) igMintUser() (string, string) {
	f.t.Helper()
	code := f.igAuthorize(igRedirectURI)
	r := f.igExchange("authorization_code", code, igClientID, igClientSecret, igRedirectURI)
	if r.Status != 200 {
		f.t.Fatalf("access_token -> %d: %v", r.Status, r.Body)
	}
	token, _ := r.Body["access_token"].(string)
	userID, _ := r.Body["user_id"].(string)
	if token == "" || userID == "" {
		f.t.Fatalf("access_token response = %v, want token + user_id", r.Body)
	}
	return token, userID
}

// igPublish creates a media container and publishes it once FINISHED: the
// virtual clock jumps the ~3s processing window, so no sleeping. Returns the
// media id.
func (f *igFixture) igPublish(token, userID string, body map[string]any) string {
	f.t.Helper()
	c := f.call("publish", "on_create", "POST", "/v21.0/"+userID+"/media",
		map[string]string{"ig_user_id": userID}, nil, body, "Bearer "+token)
	if c.Status != 200 {
		f.t.Fatalf("create container -> %d: %v", c.Status, c.Body)
	}
	containerID, _ := c.Body["id"].(string)
	f.vc.Advance(4 * time.Second)
	p := f.call("publish", "on_publish", "POST", "/v21.0/"+userID+"/media_publish",
		map[string]string{"ig_user_id": userID}, map[string]string{"creation_id": containerID}, nil, "Bearer "+token)
	if p.Status != 200 {
		f.t.Fatalf("publish %s -> %d: %v", containerID, p.Status, p.Body)
	}
	mediaID, _ := p.Body["id"].(string)
	return mediaID
}

// igErr returns the Graph error envelope object from an error response.
func igErr(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	e, ok := r.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response %d error = %v, want the Graph error envelope", r.Status, r.Body)
	}
	return e
}

// igNum compares a response number against want whether it arrives as an
// int (handler literal) or a float (round-tripped through a collection).
func igNum(got any, want int64) bool {
	switch n := got.(type) {
	case int64:
		return n == want
	case float64:
		return n == float64(want)
	}
	return false
}

// igData returns the response's data array.
func igData(t *testing.T, r starlark.Response) []any {
	t.Helper()
	data, ok := r.Body["data"].([]any)
	if !ok {
		t.Fatalf("response %d data = %v, want an array", r.Status, r.Body["data"])
	}
	return data
}

// TestInstagramOAuthAuthorizationCodeFlow: authorize demands its three
// params and redirects with a fresh code + state echo, the exchange is
// grant-type checked, codes are single-use, and client mismatches are
// rejected while each successful flow mints a distinct user.
func TestInstagramOAuthAuthorizationCodeFlow(t *testing.T) {
	f := newInstagramFixture(t, time.Unix(1_750_000_000, 0).UTC())
	authorize := func(query map[string]string) starlark.Response {
		return f.call("oauth", "on_authorize", "GET", "/oauth/authorize", nil, query, nil, "")
	}

	// ===== authorize without redirect_uri, state or client_id is invalid_request =====
	// Each missing param alone is a 400.
	for name, query := range map[string]map[string]string{
		"no redirect_uri": {"client_id": igClientID, "state": igState},
		"no state":        {"client_id": igClientID, "redirect_uri": igRedirectURI},
		"no client_id":    {"state": igState, "redirect_uri": igRedirectURI},
	} {
		if r := authorize(query); r.Status != 400 || r.Body["error"] != "invalid_request" {
			t.Fatalf("authorize %s -> %d %v, want 400 invalid_request", name, r.Status, r.Body)
		}
	}

	// ===== authorize redirects back with a fresh code and the state echoed =====
	code := f.igAuthorize(igRedirectURI)
	if !strings.HasPrefix(code, "mock_code_") {
		t.Fatalf("authorize code = %q, want a mock_code_* mint", code)
	}
	r := authorize(map[string]string{
		"client_id": igClientID, "redirect_uri": igRedirectURI, "state": igState,
	})
	loc, err := url.Parse(r.Headers["Location"])
	if err != nil {
		t.Fatalf("authorize Location %q: %v", r.Headers["Location"], err)
	}
	if loc.Query().Get("state") != igState {
		t.Fatalf("authorize Location %q, want the caller's state echoed", r.Headers["Location"])
	}

	// ===== a redirect_uri that already carries a query is joined with & =====
	r = authorize(map[string]string{
		"client_id": igClientID, "redirect_uri": "http://localhost:3000/cb?from=vm", "state": igState,
	})
	if loc := r.Headers["Location"]; !strings.Contains(loc, "cb?from=vm&code=") {
		t.Fatalf("authorize Location = %q, want the existing query joined with &", loc)
	}

	// ===== the exchange demands grant_type=authorization_code =====
	// Wrong grant types are 400 unsupported_grant_type (RFC 6749 shape).
	if r := f.igExchange("client_credentials", code, igClientID, igClientSecret, igRedirectURI); r.Status != 400 ||
		r.Body["error"] != "unsupported_grant_type" {
		t.Fatalf("client_credentials grant -> %d %v, want 400 unsupported_grant_type", r.Status, r.Body)
	}

	// ===== an unknown code is 400 invalid_grant =====
	if r := f.igExchange("authorization_code", "mock_code_nope", igClientID, igClientSecret, igRedirectURI); r.Status != 400 ||
		r.Body["error"] != "invalid_grant" {
		t.Fatalf("unknown code -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}

	// ===== a good exchange mints a token bound to a fresh IG user =====
	r = f.igExchange("authorization_code", code, igClientID, igClientSecret, igRedirectURI)
	if r.Status != 200 {
		t.Fatalf("access_token -> %d: %v", r.Status, r.Body)
	}
	token, _ := r.Body["access_token"].(string)
	userID, _ := r.Body["user_id"].(string)
	if !strings.HasPrefix(token, "mock_token_") || !strings.HasPrefix(userID, "ig_") {
		t.Fatalf("exchange minted token %q user %q, want mock_token_* / ig_*", token, userID)
	}

	// ===== the code is single-use: a replay is invalid_grant =====
	if r := f.igExchange("authorization_code", code, igClientID, igClientSecret, igRedirectURI); r.Status != 400 ||
		r.Body["error"] != "invalid_grant" {
		t.Fatalf("code replay -> %d %v, want 400 invalid_grant (single-use)", r.Status, r.Body)
	}

	// ===== client mismatches are 400 invalid_client =====
	// client_id, redirect_uri and client_secret must all match the authorize;
	// each case gets a fresh code (codes are single-use).
	for _, tc := range []struct{ name, clientID, secret, redirectURI string }{
		{"wrong client_id", "attacker-app", igClientSecret, igRedirectURI},
		{"wrong redirect_uri", igClientID, igClientSecret, "http://evil.test/cb"},
		{"missing secret", igClientID, "", igRedirectURI},
	} {
		r := f.igExchange("authorization_code", f.igAuthorize(igRedirectURI), tc.clientID, tc.secret, tc.redirectURI)
		if r.Status != 400 || r.Body["error"] != "invalid_client" {
			t.Fatalf("exchange %s -> %d %v, want 400 invalid_client", tc.name, r.Status, r.Body)
		}
	}

	// ===== a mismatched attempt must not burn the code =====
	// The wrong client's failed exchange leaves the code redeemable by the
	// right one (OAuth2: the delete happens on the matched path only).
	code3 := f.igAuthorize(igRedirectURI)
	f.igExchange("authorization_code", code3, "attacker-app", igClientSecret, igRedirectURI) // 400, above
	if r := f.igExchange("authorization_code", code3, igClientID, igClientSecret, igRedirectURI); r.Status != 200 {
		t.Fatalf("right client after a mismatched attempt -> %d %v, want 200 (code not burned)", r.Status, r.Body)
	}

	// ===== a second flow mints a distinct user =====
	token2, user2 := f.igMintUser()
	if user2 == userID || token2 == token {
		t.Fatalf("second flow minted user %q token %q, want distinct from %q/%q", user2, token2, userID, token)
	}
}

// TestInstagramAccessTokenGate: every API route validates the Bearer against
// the tokens collection — missing, malformed-scheme, unknown and expired
// tokens all answer 401 in the Graph error envelope with code 190.
func TestInstagramAccessTokenGate(t *testing.T) {
	f := newInstagramFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, userID := f.igMintUser()

	// ===== a missing bearer is 401 in the Graph error envelope =====
	// The envelope carries message, type, code and fbtrace_id.
	r := f.call("profile", "on_profile", "GET", "/v21.0/me", nil, nil, nil, "")
	if r.Status != 401 {
		t.Fatalf("no bearer -> %d, want 401", r.Status)
	}
	e := igErr(t, r)
	if e["message"] != "Missing or invalid access token" || e["type"] != "OAuthException" ||
		!igNum(e["code"], 190) || e["fbtrace_id"] != "synthetic_fbtrace_id_190" {
		t.Fatalf("401 envelope = %v, want OAuthException code 190 with fbtrace_id", e)
	}

	// ===== wrong schemes and unknown bearers answer the same 190 =====
	for _, auth := range []string{
		"Token " + token, // wrong scheme
		token,            // bare token, no scheme
		"Bearer totally-fake-token",
	} {
		if r := f.call("profile", "on_profile", "GET", "/v21.0/me", nil, nil, nil, auth); r.Status != 401 ||
			!igNum(igErr(t, r)["code"], 190) {
			t.Fatalf("auth %q -> %d, want 401 code 190", auth, r.Status)
		}
	}

	// ===== every API route enforces the same gate =====
	// All 8 Graph-surface handlers reject a missing and an unknown bearer.
	routes := []struct {
		group, handler, method, path string
		params, query                map[string]string
		body                         map[string]any
	}{
		{"profile", "on_profile", "GET", "/v21.0/me", nil, nil, nil},
		{"publish", "on_create", "POST", "/v21.0/" + userID + "/media",
			map[string]string{"ig_user_id": userID}, nil, map[string]any{"image_url": "https://x.test/1.jpg"}},
		{"publish", "on_publish", "POST", "/v21.0/" + userID + "/media_publish",
			map[string]string{"ig_user_id": userID}, map[string]string{"creation_id": "c_1"}, nil},
		{"publish", "on_list_media", "GET", "/v21.0/" + userID + "/media",
			map[string]string{"ig_user_id": userID}, nil, nil},
		{"publish", "on_container_status", "GET", "/v21.0/c_1",
			map[string]string{"container_id": "c_1"}, nil, nil},
		{"insights", "on_insights", "GET", "/v21.0/m_1/insights",
			map[string]string{"media_id": "m_1"}, nil, nil},
		{"comments", "on_comments", "GET", "/v21.0/m_1/comments",
			map[string]string{"media_id": "m_1"}, nil, nil},
	}
	for _, rt := range routes {
		for _, auth := range []string{"", "Bearer unknown-token"} {
			r := f.call(rt.group, rt.handler, rt.method, rt.path, rt.params, rt.query, rt.body, auth)
			if r.Status != 401 {
				t.Fatalf("%s (auth %q) -> %d, want 401", rt.path, auth, r.Status)
			}
			if e := igErr(t, r); e["type"] != "OAuthException" || !igNum(e["code"], 190) {
				t.Fatalf("%s 401 envelope = %v, want OAuthException code 190", rt.path, e)
			}
		}
	}

	// ===== a bearer dies at its clock-derived 60-day expiry =====
	// expires_at is minted from the engine clock; the clock is virtual.
	f.vc.Advance(61 * 24 * time.Hour)
	if r := f.call("profile", "on_profile", "GET", "/v21.0/me", nil, nil, nil, "Bearer "+token); r.Status != 401 ||
		!igNum(igErr(t, r)["code"], 190) {
		t.Fatalf("expired bearer -> %d %v, want 401 code 190", r.Status, r.Body)
	}
}

// TestInstagramProfileMe: /me answers the profile bound to the bearer's
// OAuth user, media_count tracks that user's published media, and a second
// user's profile is isolated.
func TestInstagramProfileMe(t *testing.T) {
	f := newInstagramFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, userID := f.igMintUser()
	me := func(tok string) starlark.Response {
		return f.call("profile", "on_profile", "GET", "/v21.0/me", nil, nil, nil, "Bearer "+tok)
	}

	// ===== me returns the OAuth-bound profile =====
	r := me(token)
	if r.Status != 200 {
		t.Fatalf("me -> %d: %v", r.Status, r.Body)
	}
	if r.Body["id"] != userID || r.Body["account_type"] != "BUSINESS" {
		t.Fatalf("profile = %v, want id %s (the OAuth user)", r.Body, userID)
	}
	username, _ := r.Body["username"].(string)
	if !strings.HasPrefix(username, "mock_user_") {
		t.Fatalf("profile username = %v, want the minted mock_user_*", r.Body["username"])
	}
	if !igNum(r.Body["followers_count"], 1000) {
		t.Fatalf("followers_count = %v (%T), want 1000", r.Body["followers_count"], r.Body["followers_count"])
	}

	// ===== media_count starts at 0 and tracks published media =====
	// The real node reports the user's published media, not a constant.
	if got := r.Body["media_count"]; !igNum(got, 0) {
		t.Fatalf("fresh media_count = %v, want 0", got)
	}
	f.igPublish(token, userID, map[string]any{"image_url": "https://x.test/a.jpg", "caption": "first"})
	f.igPublish(token, userID, map[string]any{"image_url": "https://x.test/b.jpg", "caption": "second"})
	if got := me(token).Body["media_count"]; !igNum(got, 2) {
		t.Fatalf("media_count after 2 publishes = %v (%T), want 2", got, got)
	}

	// ===== a second user's profile counts only its own media =====
	token2, user2 := f.igMintUser()
	f.igPublish(token2, user2, map[string]any{"image_url": "https://x.test/c.jpg"})
	p2 := me(token2)
	if p2.Body["id"] != user2 || !igNum(p2.Body["media_count"], 1) {
		t.Fatalf("second user profile = %v, want id %s media_count 1", p2.Body, user2)
	}
	if got := me(token).Body["media_count"]; !igNum(got, 2) {
		t.Fatalf("first user media_count = %v after user2's publish, want still 2", got)
	}
}

// TestInstagramPublishLifecycle: the two-step publish flow — container ids,
// the code-100 create validation, the derive-on-read processing lifecycle on
// the virtual clock (IN_PROGRESS -> FINISHED, or ERROR with simulate_fail),
// the 9007 publish gate, and the catch-all 404.
func TestInstagramPublishLifecycle(t *testing.T) {
	f := newInstagramFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, userID := f.igMintUser()
	auth := "Bearer " + token
	create := func(body map[string]any) starlark.Response {
		return f.call("publish", "on_create", "POST", "/v21.0/"+userID+"/media",
			map[string]string{"ig_user_id": userID}, nil, body, auth)
	}
	status := func(id, fields string) starlark.Response {
		query := map[string]string{}
		if fields != "" {
			query["fields"] = fields
		}
		return f.call("publish", "on_container_status", "GET", "/v21.0/"+id,
			map[string]string{"container_id": id}, query, nil, auth)
	}
	publish := func(id string) starlark.Response {
		return f.call("publish", "on_publish", "POST", "/v21.0/"+userID+"/media_publish",
			map[string]string{"ig_user_id": userID}, map[string]string{"creation_id": id}, nil, auth)
	}

	// ===== a container without image_url or video_url is a code-100 400 =====
	r := create(map[string]any{"caption": "no media"})
	if r.Status != 400 {
		t.Fatalf("create without urls -> %d, want 400", r.Status)
	}
	e := igErr(t, r)
	if !igNum(e["code"], 100) || !strings.Contains(e["message"].(string), "image_url or video_url is required") {
		t.Fatalf("create validation error = %v, want code 100", e)
	}

	// ===== create assigns a c_* container id =====
	r = create(map[string]any{"image_url": "https://x.test/photo.jpg", "caption": "hello"})
	if r.Status != 200 {
		t.Fatalf("create -> %d: %v", r.Status, r.Body)
	}
	containerID, _ := r.Body["id"].(string)
	if !strings.HasPrefix(containerID, "c_") {
		t.Fatalf("container id = %v, want c_*", r.Body["id"])
	}

	// ===== a fresh container polls IN_PROGRESS and publish is gated =====
	// media_publish before FINISHED is Graph error 9007.
	r = status(containerID, "id,status_code")
	if r.Status != 200 || r.Body["status_code"] != "IN_PROGRESS" || r.Body["id"] != containerID {
		t.Fatalf("fresh container status = %d %v, want IN_PROGRESS", r.Status, r.Body)
	}
	if r := publish(containerID); r.Status != 400 || !igNum(igErr(t, r)["code"], 9007) {
		t.Fatalf("publish while IN_PROGRESS -> %d %v, want 400 code 9007", r.Status, r.Body)
	}

	// ===== the status derives FINISHED after the ~3s window =====
	// The clock is virtual: no sleeping.
	f.vc.Advance(4 * time.Second)
	if r := status(containerID, ""); r.Status != 200 || r.Body["status_code"] != "FINISHED" {
		t.Fatalf("container status after window = %d %v, want FINISHED", r.Status, r.Body)
	}
	// A second poll agrees (the transition is persisted, not just derived).
	if r := status(containerID, "status_code"); len(r.Body) != 1 || r.Body["status_code"] != "FINISHED" {
		t.Fatalf("projected status = %v, want only status_code FINISHED", r.Body)
	}
	// Unknown fields are dropped by the fields= projection.
	if r := status(containerID, "bogus"); len(r.Body) != 0 {
		t.Fatalf("fields=bogus -> %v, want an empty projection", r.Body)
	}

	// ===== publishing a FINISHED container assigns an m_* media id =====
	r = publish(containerID)
	if r.Status != 200 {
		t.Fatalf("publish -> %d: %v", r.Status, r.Body)
	}
	mediaID, _ := r.Body["id"].(string)
	if !strings.HasPrefix(mediaID, "m_") {
		t.Fatalf("media id = %v, want m_*", r.Body["id"])
	}

	// ===== simulate_fail ends in ERROR and never publishes =====
	// Simulator-only failure injection (documented deviation).
	r = create(map[string]any{"image_url": "https://x.test/broken.jpg", "simulate_fail": "true"})
	failID, _ := r.Body["id"].(string)
	f.vc.Advance(4 * time.Second)
	if r := status(failID, "status_code"); r.Body["status_code"] != "ERROR" {
		t.Fatalf("simulate_fail status = %v, want ERROR", r.Body)
	}
	if r := publish(failID); r.Status != 400 || !igNum(igErr(t, r)["code"], 9007) {
		t.Fatalf("publish ERROR container -> %d %v, want 400 code 9007", r.Status, r.Body)
	}

	// ===== unknown container ids answer the catch-all 404 =====
	for _, r := range []starlark.Response{publish("c_nope"), status("c_nope", "")} {
		if r.Status != 404 || !igNum(igErr(t, r)["code"], 404) {
			t.Fatalf("unknown container -> %d %v, want 404 code 404", r.Status, r.Body)
		}
	}

	// ===== a video container publishes VIDEO media carrying the video url =====
	r = create(map[string]any{"video_url": "https://x.test/clip.mp4"})
	vidContainer, _ := r.Body["id"].(string)
	f.vc.Advance(4 * time.Second)
	vidMedia := publish(vidContainer).Body["id"].(string)
	lr := f.call("publish", "on_list_media", "GET", "/v21.0/"+userID+"/media",
		map[string]string{"ig_user_id": userID}, map[string]string{"fields": "id,media_type,media_url"}, nil, auth)
	if lr.Status != 200 {
		t.Fatalf("list media -> %d: %v", lr.Status, lr.Body)
	}
	var vidRow map[string]any
	for _, item := range igData(t, lr) {
		if m := item.(map[string]any); m["id"] == vidMedia {
			vidRow = m
		}
	}
	if vidRow == nil || vidRow["media_type"] != "VIDEO" || vidRow["media_url"] != "https://x.test/clip.mp4" {
		t.Fatalf("video media row = %v, want VIDEO with the video url", vidRow)
	}
}

// TestInstagramMediaListOrderFieldsPaging: the media edge lists a user's
// media newest first, projects each row onto ?fields=, pages by limit + after
// cursors with a next link that preserves the query, and is scoped to the
// path user.
func TestInstagramMediaListOrderFieldsPaging(t *testing.T) {
	f := newInstagramFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, userID := f.igMintUser()
	token2, user2 := f.igMintUser()
	list := func(tok, user string, query map[string]string) starlark.Response {
		return f.call("publish", "on_list_media", "GET", "/v21.0/"+user+"/media",
			map[string]string{"ig_user_id": user}, query, nil, "Bearer "+tok)
	}

	// Publish three media an hour apart: oldest first is first, second, third.
	first := f.igPublish(token, userID, map[string]any{"image_url": "https://x.test/1.jpg", "caption": "first"})
	f.vc.Advance(time.Hour)
	second := f.igPublish(token, userID, map[string]any{"image_url": "https://x.test/2.jpg", "caption": "second"})
	f.vc.Advance(time.Hour)
	third := f.igPublish(token, userID, map[string]any{"image_url": "https://x.test/3.jpg", "caption": "third"})
	other := f.igPublish(token2, user2, map[string]any{"image_url": "https://x.test/other.jpg"})

	// ===== the media edge lists newest first (reverse chronological) =====
	r := list(token, userID, nil)
	if r.Status != 200 {
		t.Fatalf("list media -> %d: %v", r.Status, r.Body)
	}
	data := igData(t, r)
	if len(data) != 3 {
		t.Fatalf("media list has %d rows, want the user's 3", len(data))
	}
	wantOrder := []string{third, second, first}
	prev := time.Now()
	for i, item := range data {
		m := item.(map[string]any)
		if m["id"] != wantOrder[i] {
			t.Fatalf("media[%d] id = %v, want %s (newest first)", i, m["id"], wantOrder[i])
		}
		ts, err := time.Parse(igGraphStampFmt, m["timestamp"].(string))
		if err != nil {
			t.Fatalf("media[%d] timestamp %q unparsable as a Graph stamp: %v", i, m["timestamp"], err)
		}
		if i > 0 && !ts.Before(prev) {
			t.Fatalf("media[%d] timestamp %v not before media[%d] %v (newest first)", i, ts, i-1, prev)
		}
		prev = ts
	}

	// ===== ?fields= projects each row and drops unknown fields =====
	r = list(token, userID, map[string]string{"fields": "id,caption"})
	for _, item := range igData(t, r) {
		m := item.(map[string]any)
		if len(m) != 2 || m["id"] == "" || m["caption"] == "" {
			t.Fatalf("projected row = %v, want exactly id + caption", m)
		}
	}
	if r := list(token, userID, map[string]string{"fields": "id,bogus"}); len(igData(t, r)[0].(map[string]any)) != 1 {
		t.Fatalf("fields with an unknown name = %v, want unknown fields dropped", igData(t, r)[0])
	}

	// ===== limit pages through after cursors with a next that keeps the query =====
	r = list(token, userID, map[string]string{"limit": "2", "fields": "id"})
	page1 := igData(t, r)
	if len(page1) != 2 || page1[0].(map[string]any)["id"] != third || page1[1].(map[string]any)["id"] != second {
		t.Fatalf("page 1 = %v, want the two newest", page1)
	}
	paging, ok := r.Body["paging"].(map[string]any)
	if !ok {
		t.Fatalf("partial page carries no paging block: %v", r.Body)
	}
	after := paging["cursors"].(map[string]any)["after"].(string)
	if after == "" {
		t.Fatalf("paging.cursors.after = %v, want an opaque cursor", paging["cursors"])
	}
	// The next link re-issues the original query (limit + fields) with the
	// new cursor, so following it keeps the page shape.
	wantNext := "/v21.0/" + userID + "/media?after=" + after + "&limit=2&fields=id"
	if paging["next"] != wantNext {
		t.Fatalf("paging.next = %v, want %q", paging["next"], wantNext)
	}
	r = list(token, userID, map[string]string{"limit": "2", "fields": "id", "after": after})
	page2 := igData(t, r)
	if len(page2) != 1 || page2[0].(map[string]any)["id"] != first {
		t.Fatalf("page 2 = %v, want the oldest row", page2)
	}
	if _, has := r.Body["paging"]; has {
		t.Fatalf("final page carries paging = %v, want none", r.Body["paging"])
	}

	// ===== a malformed after cursor is a code-100 400 =====
	r = list(token, userID, map[string]string{"limit": "2", "after": "zzz"})
	if r.Status != 400 {
		t.Fatalf("bad cursor -> %d, want 400", r.Status)
	}
	if e := igErr(t, r); e["type"] != "OAuthException" || !igNum(e["code"], 100) {
		t.Fatalf("bad cursor error = %v, want code 100", e)
	}

	// ===== the list is scoped to the path user =====
	r = list(token2, user2, map[string]string{"fields": "id"})
	data = igData(t, r)
	if len(data) != 1 || data[0].(map[string]any)["id"] != other {
		t.Fatalf("user2 media list = %v, want only its own media %s", data, other)
	}
}

// TestInstagramInsightsMetrics: the insights edge returns the four manifest
// metrics (impressions, reach, likes, comments) as single-value series with
// deterministic per-media values, filters to ?metric=, silently empties on
// an unknown metric name (documented deviation), and 404s unknown media.
func TestInstagramInsightsMetrics(t *testing.T) {
	f := newInstagramFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, userID := f.igMintUser()
	mediaID := f.igPublish(token, userID, map[string]any{"image_url": "https://x.test/1.jpg"})
	insights := func(metric string) starlark.Response {
		query := map[string]string{}
		if metric != "" {
			query["metric"] = metric
		}
		return f.call("insights", "on_insights", "GET", "/v21.0/"+mediaID+"/insights",
			map[string]string{"media_id": mediaID}, query, nil, "Bearer "+token)
	}
	// Ranges from the handler's hash math: impressions 100..100099,
	// reach 50..50049, likes 0..4999, comments 0..999.
	metricRange := map[string][2]int64{
		"impressions": {100, 100099}, "reach": {50, 50049}, "likes": {0, 4999}, "comments": {0, 999},
	}
	titles := map[string]string{
		"impressions": "Impressions", "reach": "Reach", "likes": "Likes", "comments": "Comments",
	}
	valuesOf := func(r starlark.Response) map[string]int64 {
		out := map[string]int64{}
		for _, item := range igData(t, r) {
			m := item.(map[string]any)
			name := m["name"].(string)
			series := m["values"].([]any)
			if len(series) != 1 {
				t.Fatalf("metric %q values = %v, want a single-period series", name, series)
			}
			v := series[0].(map[string]any)["value"]
			n, ok := v.(int64)
			if !ok {
				if f, isf := v.(float64); isf {
					n, ok = int64(f), true
				}
			}
			if !ok {
				t.Fatalf("metric %q value = %v (%T), want a number", name, v, v)
			}
			lo, hi := metricRange[name][0], metricRange[name][1]
			if n < lo || n > hi {
				t.Fatalf("metric %q value = %d, want within [%d,%d]", name, n, lo, hi)
			}
			out[name] = n
		}
		return out
	}

	// ===== all four manifest metrics return in canonical order =====
	r := insights("")
	if r.Status != 200 {
		t.Fatalf("insights -> %d: %v", r.Status, r.Body)
	}
	data := igData(t, r)
	if len(data) != 4 {
		t.Fatalf("insights data has %d rows, want 4 metrics", len(data))
	}
	for i, want := range []string{"impressions", "reach", "likes", "comments"} {
		m := data[i].(map[string]any)
		if m["name"] != want {
			t.Fatalf("insights[%d] = %v, want metric %q", i, m["name"], want)
		}
		if m["title"] != titles[want] {
			t.Fatalf("metric %q title = %v, want %q", want, m["title"], titles[want])
		}
	}
	first := valuesOf(r)

	// ===== values are deterministic across reads =====
	if second := valuesOf(insights("")); !reflect.DeepEqual(first, second) {
		t.Fatalf("insights values changed across reads: %v then %v", first, second)
	}

	// ===== ?metric= filters to the requested names =====
	r = insights("reach,likes")
	if got := valuesOf(r); len(got) != 2 {
		t.Fatalf("metric=reach,likes -> %v, want exactly those two", got)
	}
	if r := insights("impressions"); len(igData(t, r)) != 1 || igData(t, r)[0].(map[string]any)["name"] != "impressions" {
		t.Fatalf("metric=impressions -> %v, want only impressions", r.Body["data"])
	}

	// ===== an unknown metric name empties the data set (deviation, as-is) =====
	// Real Graph answers an invalid metric with error code 100; the adapter
	// filters silently.
	if r := insights("bogus"); r.Status != 200 || len(igData(t, r)) != 0 {
		t.Fatalf("metric=bogus -> %d %v, want 200 with empty data", r.Status, r.Body)
	}

	// ===== insights for an unknown media id are the catch-all 404 =====
	// Bare envelope without type/fbtrace_id (assert as-is).
	r = f.call("insights", "on_insights", "GET", "/v21.0/m_nope/insights",
		map[string]string{"media_id": "m_nope"}, nil, nil, "Bearer "+token)
	if r.Status != 404 {
		t.Fatalf("insights unknown media -> %d, want 404", r.Status)
	}
	e := igErr(t, r)
	if !igNum(e["code"], 404) || e["message"] != "resource not found" {
		t.Fatalf("insights 404 envelope = %v, want the catch-all shape", e)
	}
	if _, has := e["type"]; has {
		t.Fatalf("insights 404 carries type = %v, want the bare catch-all envelope", e)
	}
}

// TestInstagramComments: the comments edge authorizes via the access_token
// query param (SDK style) or the bearer header, synthesizes two stable
// per-media comments with staggered timestamps, honors the Graph since
// filter, and answers unknown media with a code-100 OAuthException 404.
func TestInstagramComments(t *testing.T) {
	f := newInstagramFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, userID := f.igMintUser()
	mediaID := f.igPublish(token, userID, map[string]any{"image_url": "https://x.test/1.jpg", "caption": "post"})
	comments := func(query map[string]string, auth string) starlark.Response {
		return f.call("comments", "on_comments", "GET", "/v21.0/"+mediaID+"/comments",
			map[string]string{"media_id": mediaID}, query, nil, auth)
	}
	seedUsers := map[string]bool{"artlover": true, "printsfan": true, "canvas.curious": true, "studio.visitor": true}

	// ===== comments authorize via the access_token query param =====
	r := comments(map[string]string{"access_token": token}, "")
	if r.Status != 200 {
		t.Fatalf("comments (query token) -> %d: %v", r.Status, r.Body)
	}
	data := igData(t, r)
	if len(data) != 2 {
		t.Fatalf("comments data = %v, want 2 deterministic comments", data)
	}
	stamps := []time.Time{}
	for i, item := range data {
		c := item.(map[string]any)
		if c["id"] != "cmt_"+mediaID+"_"+strconv.Itoa(i+1) {
			t.Fatalf("comment[%d] id = %v, want cmt_%s_%d", i, c["id"], mediaID, i+1)
		}
		if !seedUsers[c["username"].(string)] {
			t.Fatalf("comment[%d] username = %v, want from the seed set", i, c["username"])
		}
		if txt := c["text"].(string); txt == "" {
			t.Fatalf("comment[%d] text = %v, want non-empty", i, c["text"])
		}
		if n, ok := c["like_count"].(int64); !ok || n < 0 || n > 11 {
			t.Fatalf("comment[%d] like_count = %v (%T), want 0..11", i, c["like_count"], c["like_count"])
		}
		ts, err := time.Parse(time.RFC3339, c["timestamp"].(string))
		if err != nil {
			t.Fatalf("comment[%d] timestamp %q: %v", i, c["timestamp"], err)
		}
		stamps = append(stamps, ts)
	}
	// The second comment is staggered minutes after the first.
	if !stamps[1].After(stamps[0]) {
		t.Fatalf("comment timestamps %v not staggered", stamps)
	}

	// ===== the bearer header authorizes the identical read =====
	r2 := comments(nil, "Bearer "+token)
	if !reflect.DeepEqual(r.Body["data"], r2.Body["data"]) {
		t.Fatalf("comments differ between query-token and bearer reads: %v vs %v", r.Body["data"], r2.Body["data"])
	}

	// ===== a bad query token is the 190 envelope =====
	if r := comments(map[string]string{"access_token": "mock_token_nope"}, ""); r.Status != 401 ||
		!igNum(igErr(t, r)["code"], 190) {
		t.Fatalf("bad query token -> %d %v, want 401 code 190", r.Status, r.Body)
	}

	// ===== since hides comments at-or-before the cutoff =====
	since := func(cutoff int64) int {
		r := comments(map[string]string{"access_token": token, "since": strconv.FormatInt(cutoff, 10)}, "")
		return len(igData(t, r))
	}
	if n := since(stamps[0].Unix() - 1); n != 2 {
		t.Fatalf("since before both comments -> %d, want 2", n)
	}
	if n := since(stamps[0].Unix()); n != 1 {
		t.Fatalf("since at the first comment -> %d, want 1 (at-or-before is hidden)", n)
	}
	if n := since(stamps[1].Unix()); n != 0 {
		t.Fatalf("since at the second comment -> %d, want 0", n)
	}

	// ===== comments on an unknown media id are a code-100 OAuthException 404 =====
	r = f.call("comments", "on_comments", "GET", "/v21.0/m_nope/comments",
		map[string]string{"media_id": "m_nope"}, map[string]string{"access_token": token}, nil, "")
	if r.Status != 404 {
		t.Fatalf("comments unknown media -> %d, want 404", r.Status)
	}
	e := igErr(t, r)
	if e["type"] != "OAuthException" || !igNum(e["code"], 100) || e["fbtrace_id"] != "synthetic_fbtrace_id_100" {
		t.Fatalf("comments 404 envelope = %v, want code 100 with fbtrace_id", e)
	}
}

// TestInstagramRefreshLongLivedToken: GET /v21.0/refresh_access_token demands
// grant_type=ig_refresh_token and a known token, mints a fresh 60-day token
// without invalidating the old one, and the refreshed token outlives the
// original's clock-derived expiry.
func TestInstagramRefreshLongLivedToken(t *testing.T) {
	base := time.Unix(1_750_000_000, 0).UTC()
	f := newInstagramFixture(t, base)
	token, userID := f.igMintUser()
	refresh := func(query map[string]string, auth string) starlark.Response {
		return f.call("oauth", "on_refresh_token", "GET", "/v21.0/refresh_access_token", nil, query, nil, auth)
	}
	me := func(tok string) int {
		return f.call("profile", "on_profile", "GET", "/v21.0/me", nil, nil, nil, "Bearer "+tok).Status
	}

	// ===== refresh demands a token (query param or bearer) =====
	if r := refresh(map[string]string{"grant_type": "ig_refresh_token"}, ""); r.Status != 400 ||
		!igNum(igErr(t, r)["code"], 190) {
		t.Fatalf("refresh without token -> %d %v, want 400 code 190", r.Status, r.Body)
	}
	// The bearer header substitutes for the access_token query param.
	if r := refresh(map[string]string{"grant_type": "nope"}, "Bearer "+token); r.Status != 400 ||
		!igNum(igErr(t, r)["code"], 1) {
		t.Fatalf("refresh wrong grant (bearer auth) -> %d %v, want 400 code 1", r.Status, r.Body)
	}

	// ===== an unknown token is rejected with 190 =====
	if r := refresh(map[string]string{"grant_type": "ig_refresh_token", "access_token": "mock_token_nope"}, ""); r.Status != 400 ||
		igErr(t, r)["message"] != "Invalid OAuth access token" {
		t.Fatalf("refresh unknown token -> %d %v, want 400 invalid token", r.Status, r.Body)
	}

	// ===== refresh mints a fresh 60-day token; the old one keeps working =====
	f.vc.Advance(30 * 24 * time.Hour) // half the original's life
	r := refresh(map[string]string{"grant_type": "ig_refresh_token", "access_token": token}, "")
	if r.Status != 200 {
		t.Fatalf("refresh -> %d: %v", r.Status, r.Body)
	}
	fresh, _ := r.Body["access_token"].(string)
	if fresh == "" || fresh == token {
		t.Fatalf("refreshed token = %v, want a fresh mint", r.Body["access_token"])
	}
	if r.Body["token_type"] != "bearer" || !igNum(r.Body["expires_in"], 60*24*3600) {
		t.Fatalf("refresh response = %v, want token_type bearer expires_in 60d", r.Body)
	}
	// No rotation invalidation: the original still authorizes.
	if me(token) != 200 {
		t.Fatal("original token invalidated by refresh, want it valid until its own expiry")
	}
	// The fresh token is bound to the same user.
	p := f.call("profile", "on_profile", "GET", "/v21.0/me", nil, nil, nil, "Bearer "+fresh)
	if p.Status != 200 || p.Body["id"] != userID {
		t.Fatalf("me with refreshed token -> %d %v, want the same user %s", p.Status, p.Body, userID)
	}

	// ===== the refreshed token outlives the original's expiry =====
	f.vc.Advance(31 * 24 * time.Hour) // 61d past the original mint
	if me(token) != 401 {
		t.Fatal("original token still valid at 61d, want 401 (60-day life)")
	}
	if r := refresh(map[string]string{"grant_type": "ig_refresh_token", "access_token": token}, ""); r.Status != 400 ||
		igErr(t, r)["message"] != "The access token has expired" {
		t.Fatalf("refresh expired token -> %d %v, want 400 expired", r.Status, r.Body)
	}
	if me(fresh) != 200 {
		t.Fatal("refreshed token invalid at 31d past its mint, want valid for its own 60 days")
	}
}
