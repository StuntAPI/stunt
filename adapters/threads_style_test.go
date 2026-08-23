package adapters

import (
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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

// These tests drive the threads-style adapter scripts directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock: the Meta
// authorization-code OAuth flow with its single-use codes and rotating
// single-use refresh grant, the access-token gate (Graph error envelope
// {message, type, code, fbtrace_id}), the static /me profile (pinned by the
// engine conformance test — the real API binds /me to the token's user), the
// two-step Threads publish flow (create container -> publish; TEXT
// containers finish processing immediately, simulate_fail drives the error
// path), the engagement inbox (newest-first media with one synthetic reply
// child each) and the four per-media insight metrics with their ?metric=
// projection.

// One OAuth client for the whole suite; the adapter accepts any values.
const (
	thrRedirectURI   = "http://localhost:3000/callback"
	thrState         = "vm-suite-state"
	thrClientID      = "thr-vm-client-id"
	thrClientSecret  = "thr-vm-client-secret"
	thrGraphStampFmt = "2006-01-02T15:04:05-0700" // Graph media timestamps: no colon in the offset
)

// thrFixture is one shared store + virtual clock with a loaded VM per handler
// script (oauth, profile, publish, insights and engagement each get their own
// VM, but they observe the same collections/kv state, like the engine).
type thrFixture struct {
	t   *testing.T
	vc  *clock.Clock
	vms map[string]*starlark.VM
}

func newThreadsFixture(t *testing.T, start time.Time) *thrFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "threads-style")
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
	return &thrFixture{t: t, vc: vc, vms: map[string]*starlark.VM{
		"oauth": load("oauth.star"), "profile": load("profile.star"),
		"publish": load("publish.star"), "insights": load("insights.star"),
		"engagement": load("engagement.star"),
	}}
}

// call invokes handler on the named script VM; auth is the full
// Authorization header value ("" = header absent).
func (f *thrFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: "graph.threads.test",
		Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// thrAuthorize runs the authorize redirect against redirectURI and returns
// the single-use code it mints.
func (f *thrFixture) thrAuthorize(redirectURI string) string {
	f.t.Helper()
	r := f.call("oauth", "on_authorize", "GET", "/oauth/authorize", nil, map[string]string{
		"client_id": thrClientID, "redirect_uri": redirectURI, "state": thrState,
		"response_type": "code", "scope": "threads_basic",
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

// thrExchange trades a code at the token endpoint; "" omits the field.
func (f *thrFixture) thrExchange(grant, code, clientID, secret, redirectURI string) starlark.Response {
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

// thrMintUser runs the authorization-code flow end to end and returns the
// minted (token, user_id).
func (f *thrFixture) thrMintUser() (string, string) {
	f.t.Helper()
	code := f.thrAuthorize(thrRedirectURI)
	r := f.thrExchange("authorization_code", code, thrClientID, thrClientSecret, thrRedirectURI)
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

// thrPost creates a TEXT container and publishes it back-to-back (TEXT
// containers finish processing immediately, so no status poll is needed).
// Returns the media id.
func (f *thrFixture) thrPost(token, userID, text string) string {
	f.t.Helper()
	c := f.call("publish", "on_create", "POST", "/v1.0/"+userID+"/threads",
		map[string]string{"id": userID}, nil, map[string]any{"media_type": "TEXT", "text": text}, "Bearer "+token)
	if c.Status != 201 {
		f.t.Fatalf("create container -> %d: %v", c.Status, c.Body)
	}
	containerID, _ := c.Body["id"].(string)
	p := f.call("publish", "on_publish", "POST", "/v1.0/"+userID+"/threads_publish",
		map[string]string{"id": userID}, map[string]string{"creation_id": containerID}, nil, "Bearer "+token)
	if p.Status != 201 {
		f.t.Fatalf("publish %s -> %d: %v", containerID, p.Status, p.Body)
	}
	mediaID, _ := p.Body["id"].(string)
	return mediaID
}

// thrErr returns the Graph error envelope object from an error response.
func thrErr(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	e, ok := r.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response %d error = %v, want the Graph error envelope", r.Status, r.Body)
	}
	return e
}

// thrNum compares a response number against want whether it arrives as an
// int (handler literal) or a float (round-tripped through a collection).
func thrNum(got any, want int64) bool {
	switch n := got.(type) {
	case int64:
		return n == want
	case float64:
		return n == float64(want)
	}
	return false
}

// thrData returns the response's data array.
func thrData(t *testing.T, r starlark.Response) []any {
	t.Helper()
	data, ok := r.Body["data"].([]any)
	if !ok {
		t.Fatalf("response %d data = %v, want an array", r.Status, r.Body["data"])
	}
	return data
}

// TestThreadsOAuthAuthorizationCodeFlow: authorize demands its three params
// and redirects with a fresh code + state echo, the exchange is grant-type
// checked, codes are single-use, client mismatches are rejected and leave
// the code redeemable (house rule), and each successful flow mints a
// distinct user.
func TestThreadsOAuthAuthorizationCodeFlow(t *testing.T) {
	f := newThreadsFixture(t, time.Unix(1_750_000_000, 0).UTC())
	authorize := func(query map[string]string) starlark.Response {
		return f.call("oauth", "on_authorize", "GET", "/oauth/authorize", nil, query, nil, "")
	}

	// ===== authorize without redirect_uri, state or client_id is invalid_request =====
	// Each missing param alone is a 400.
	for name, query := range map[string]map[string]string{
		"no redirect_uri": {"client_id": thrClientID, "state": thrState},
		"no state":        {"client_id": thrClientID, "redirect_uri": thrRedirectURI},
		"no client_id":    {"state": thrState, "redirect_uri": thrRedirectURI},
	} {
		if r := authorize(query); r.Status != 400 || r.Body["error"] != "invalid_request" {
			t.Fatalf("authorize %s -> %d %v, want 400 invalid_request", name, r.Status, r.Body)
		}
	}

	// ===== authorize redirects back with a fresh code and the state echoed =====
	code := f.thrAuthorize(thrRedirectURI)
	if !strings.HasPrefix(code, "mock_code_") {
		t.Fatalf("authorize code = %q, want a mock_code_* mint", code)
	}
	r := authorize(map[string]string{
		"client_id": thrClientID, "redirect_uri": thrRedirectURI, "state": thrState,
	})
	loc, err := url.Parse(r.Headers["Location"])
	if err != nil {
		t.Fatalf("authorize Location %q: %v", r.Headers["Location"], err)
	}
	if loc.Query().Get("state") != thrState {
		t.Fatalf("authorize Location %q, want the caller's state echoed", r.Headers["Location"])
	}

	// ===== a redirect_uri that already carries a query is joined with & =====
	r = authorize(map[string]string{
		"client_id": thrClientID, "redirect_uri": "http://localhost:3000/cb?from=vm", "state": thrState,
	})
	if loc := r.Headers["Location"]; !strings.Contains(loc, "cb?from=vm&code=") {
		t.Fatalf("authorize Location = %q, want the existing query joined with &", loc)
	}

	// ===== the exchange demands grant_type=authorization_code =====
	// Wrong grant types are 400 unsupported_grant_type (RFC 6749 shape).
	if r := f.thrExchange("client_credentials", code, thrClientID, thrClientSecret, thrRedirectURI); r.Status != 400 ||
		r.Body["error"] != "unsupported_grant_type" {
		t.Fatalf("client_credentials grant -> %d %v, want 400 unsupported_grant_type", r.Status, r.Body)
	}

	// ===== an unknown code is 400 invalid_grant =====
	if r := f.thrExchange("authorization_code", "mock_code_nope", thrClientID, thrClientSecret, thrRedirectURI); r.Status != 400 ||
		r.Body["error"] != "invalid_grant" {
		t.Fatalf("unknown code -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}

	// ===== a good exchange mints a token, user and refresh token =====
	r = f.thrExchange("authorization_code", code, thrClientID, thrClientSecret, thrRedirectURI)
	if r.Status != 200 {
		t.Fatalf("access_token -> %d: %v", r.Status, r.Body)
	}
	token, _ := r.Body["access_token"].(string)
	userID, _ := r.Body["user_id"].(string)
	refresh, _ := r.Body["refresh_token"].(string)
	if !strings.HasPrefix(token, "mock_token_") || !strings.HasPrefix(userID, "u_") ||
		!strings.HasPrefix(refresh, "mock_refresh_") {
		t.Fatalf("exchange minted token %q user %q refresh %q, want mock_token_* / u_* / mock_refresh_*",
			token, userID, refresh)
	}
	if r.Body["token_type"] != "bearer" || !thrNum(r.Body["expires_in"], 5184000) {
		t.Fatalf("exchange response = %v, want token_type bearer expires_in 60d", r.Body)
	}

	// ===== the code is single-use: a replay is invalid_grant =====
	if r := f.thrExchange("authorization_code", code, thrClientID, thrClientSecret, thrRedirectURI); r.Status != 400 ||
		r.Body["error"] != "invalid_grant" {
		t.Fatalf("code replay -> %d %v, want 400 invalid_grant (single-use)", r.Status, r.Body)
	}

	// ===== client mismatches are 400 invalid_client =====
	// client_id, redirect_uri and client_secret must all match the authorize;
	// each case gets a fresh code (codes are single-use).
	for _, tc := range []struct{ name, clientID, secret, redirectURI string }{
		{"wrong client_id", "attacker-app", thrClientSecret, thrRedirectURI},
		{"wrong redirect_uri", thrClientID, thrClientSecret, "http://evil.test/cb"},
		{"missing secret", thrClientID, "", thrRedirectURI},
	} {
		r := f.thrExchange("authorization_code", f.thrAuthorize(thrRedirectURI), tc.clientID, tc.secret, tc.redirectURI)
		if r.Status != 400 || r.Body["error"] != "invalid_client" {
			t.Fatalf("exchange %s -> %d %v, want 400 invalid_client", tc.name, r.Status, r.Body)
		}
	}

	// ===== a mismatched attempt leaves the code redeemable (house rule) =====
	// The code is burned only on a successful exchange, so an attacker's
	// failed client-mismatch attempt cannot deny the legitimate client.
	code3 := f.thrAuthorize(thrRedirectURI)
	f.thrExchange("authorization_code", code3, "attacker-app", thrClientSecret, thrRedirectURI) // 400, above
	if r := f.thrExchange("authorization_code", code3, thrClientID, thrClientSecret, thrRedirectURI); r.Status != 200 {
		t.Fatalf("right client after a mismatched attempt -> %d %v, want 200 (code survived)", r.Status, r.Body)
	}

	// ===== a second flow mints a distinct user =====
	token2, user2 := f.thrMintUser()
	if user2 == userID || token2 == token {
		t.Fatalf("second flow minted user %q token %q, want distinct from %q/%q", user2, token2, userID, token)
	}
}

// TestThreadsAccessTokenGate: every API route validates the Bearer against the
// tokens collection — missing, malformed-scheme, unknown and expired tokens
// all answer 401 in the Graph error envelope with code 190.
func TestThreadsAccessTokenGate(t *testing.T) {
	f := newThreadsFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, userID := f.thrMintUser()

	// ===== a missing bearer is 401 in the Graph error envelope =====
	// The envelope carries message, type, code and fbtrace_id.
	r := f.call("profile", "on_profile", "GET", "/v1.0/me", nil, nil, nil, "")
	if r.Status != 401 {
		t.Fatalf("no bearer -> %d, want 401", r.Status)
	}
	e := thrErr(t, r)
	if e["message"] != "Missing or invalid access token" || e["type"] != "OAuthException" ||
		!thrNum(e["code"], 190) || e["fbtrace_id"] != "synthetic_fbtrace_id_190" {
		t.Fatalf("401 envelope = %v, want OAuthException code 190 with fbtrace_id", e)
	}

	// ===== wrong schemes and unknown bearers answer the same 190 =====
	for _, auth := range []string{
		"Token " + token, // wrong scheme
		token,            // bare token, no scheme
		"Bearer totally-fake-token",
	} {
		if r := f.call("profile", "on_profile", "GET", "/v1.0/me", nil, nil, nil, auth); r.Status != 401 ||
			!thrNum(thrErr(t, r)["code"], 190) {
			t.Fatalf("auth %q -> %d, want 401 code 190", auth, r.Status)
		}
	}

	// ===== every API route enforces the same gate =====
	// All six Graph-surface handlers reject a missing and an unknown bearer.
	routes := []struct {
		group, handler, method, path string
		params, query                map[string]string
		body                         map[string]any
	}{
		{"profile", "on_profile", "GET", "/v1.0/me", nil, nil, nil},
		{"publish", "on_create", "POST", "/v1.0/" + userID + "/threads",
			map[string]string{"id": userID}, nil, map[string]any{"media_type": "TEXT", "text": "hi"}},
		{"publish", "on_publish", "POST", "/v1.0/" + userID + "/threads_publish",
			map[string]string{"id": userID}, map[string]string{"creation_id": "c_1"}, nil},
		{"publish", "on_container_status", "GET", "/v1.0/c_1",
			map[string]string{"container_id": "c_1"}, nil, nil},
		{"insights", "on_insights", "GET", "/v1.0/m_1/insights",
			map[string]string{"id": "m_1"}, nil, nil},
		{"engagement", "on_engagement", "GET", "/v1.0/" + userID + "/threads",
			map[string]string{"id": userID}, nil, nil},
	}
	for _, rt := range routes {
		for _, auth := range []string{"", "Bearer unknown-token"} {
			r := f.call(rt.group, rt.handler, rt.method, rt.path, rt.params, rt.query, rt.body, auth)
			if r.Status != 401 {
				t.Fatalf("%s (auth %q) -> %d, want 401", rt.path, auth, r.Status)
			}
			if e := thrErr(t, r); e["type"] != "OAuthException" || !thrNum(e["code"], 190) {
				t.Fatalf("%s 401 envelope = %v, want OAuthException code 190", rt.path, e)
			}
		}
	}

	// ===== a bearer dies at its clock-derived 60-day expiry =====
	// expires_at is minted from the engine clock; the clock is virtual.
	f.vc.Advance(61 * 24 * time.Hour)
	if r := f.call("profile", "on_profile", "GET", "/v1.0/me", nil, nil, nil, "Bearer "+token); r.Status != 401 ||
		!thrNum(thrErr(t, r)["code"], 190) {
		t.Fatalf("expired bearer -> %d %v, want 401 code 190", r.Status, r.Body)
	}
}

// TestThreadsProfileMe: /me answers the static mock profile (any valid token
// sees the same shape — the engine conformance test pins this; the real API
// binds /me to the token's user) and ignores the Graph ?fields= projection.
func TestThreadsProfileMe(t *testing.T) {
	f := newThreadsFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, _ := f.thrMintUser()
	me := func(tok string, query map[string]string) starlark.Response {
		return f.call("profile", "on_profile", "GET", "/v1.0/me", nil, query, nil, "Bearer "+tok)
	}

	// ===== me returns the static mock profile =====
	r := me(token, nil)
	if r.Status != 200 {
		t.Fatalf("me -> %d: %v", r.Status, r.Body)
	}
	if r.Body["id"] != "u_me" || r.Body["username"] != "mock_user_me" {
		t.Fatalf("profile = %v, want the static u_me mock profile", r.Body)
	}
	if r.Body["threads_profile_picture_path"] != "https://mock-threads.example/pic/me.jpg" ||
		r.Body["threads_biography"] != "building in public" {
		t.Fatalf("profile = %v, want the mock picture path + biography", r.Body)
	}

	// ===== every user's token sees the same static profile (deviation, as-is) =====
	// Real Threads binds /me to the OAuth user; the mock does not.
	token2, _ := f.thrMintUser()
	if r := me(token2, nil); r.Body["id"] != "u_me" || r.Body["username"] != "mock_user_me" {
		t.Fatalf("second user's me = %v, want the same static profile (as-is)", r.Body)
	}

	// ===== ?fields= is ignored: the full profile always comes back =====
	// The container-status node projects onto fields=; the profile node does
	// not (assert as-is).
	r = me(token, map[string]string{"fields": "id,username"})
	if len(r.Body) != 4 {
		t.Fatalf("fields=id,username -> %v, want the full 4-key profile (projection ignored)", r.Body)
	}
}

// TestThreadsPublishLifecycle: the two-step publish flow — container ids, the
// code-100 create validation, TEXT containers finishing processing
// immediately (no poll needed — the derive-on-read window only lives on in
// the simulate_fail branch), the fields= status projection, the error-path
// publish gate, republishing minting a second media (no re-publish guard) and
// the catch-all 404.
func TestThreadsPublishLifecycle(t *testing.T) {
	f := newThreadsFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, userID := f.thrMintUser()
	auth := "Bearer " + token
	create := func(body map[string]any) starlark.Response {
		return f.call("publish", "on_create", "POST", "/v1.0/"+userID+"/threads",
			map[string]string{"id": userID}, nil, body, auth)
	}
	status := func(id, fields string) starlark.Response {
		query := map[string]string{}
		if fields != "" {
			query["fields"] = fields
		}
		return f.call("publish", "on_container_status", "GET", "/v1.0/"+id,
			map[string]string{"container_id": id}, query, nil, auth)
	}
	publish := func(id string) starlark.Response {
		return f.call("publish", "on_publish", "POST", "/v1.0/"+userID+"/threads_publish",
			map[string]string{"id": userID}, map[string]string{"creation_id": id}, nil, auth)
	}

	// ===== a container without media_type=TEXT and text is a code-100 400 =====
	for name, body := range map[string]map[string]any{
		"missing text":     {"media_type": "TEXT"},
		"wrong media_type": {"media_type": "IMAGE", "image_url": "https://x.test/1.jpg"},
		"empty body":       {},
	} {
		r := create(body)
		if r.Status != 400 {
			t.Fatalf("create %s -> %d, want 400", name, r.Status)
		}
		e := thrErr(t, r)
		if !thrNum(e["code"], 100) || !strings.Contains(e["message"].(string), "media_type must be TEXT") {
			t.Fatalf("create %s error = %v, want code 100 media_type TEXT validation", name, e)
		}
	}

	// ===== create assigns a c_* container id =====
	r := create(map[string]any{"media_type": "TEXT", "text": "hello threads"})
	if r.Status != 201 {
		t.Fatalf("create -> %d: %v", r.Status, r.Body)
	}
	containerID, _ := r.Body["id"].(string)
	if !strings.HasPrefix(containerID, "c_") {
		t.Fatalf("container id = %v, want c_*", r.Body["id"])
	}

	// ===== a TEXT container polls finished immediately (no processing window) =====
	// Real Threads only makes you poll video/image uploads; text is done at
	// create. A second poll agrees (the transition is persisted, not derived).
	r = status(containerID, "id,status")
	if r.Status != 200 || r.Body["status"] != "finished" || r.Body["id"] != containerID {
		t.Fatalf("fresh TEXT container status = %d %v, want finished", r.Status, r.Body)
	}
	if r := status(containerID, "status"); len(r.Body) != 1 || r.Body["status"] != "finished" {
		t.Fatalf("projected status = %v, want only status finished", r.Body)
	}
	// Unknown fields are dropped by the fields= projection.
	if r := status(containerID, "bogus"); len(r.Body) != 0 {
		t.Fatalf("fields=bogus -> %v, want an empty projection", r.Body)
	}

	// ===== back-to-back publish assigns an m_* media id =====
	r = publish(containerID)
	if r.Status != 201 {
		t.Fatalf("publish -> %d: %v", r.Status, r.Body)
	}
	mediaID, _ := r.Body["id"].(string)
	if !strings.HasPrefix(mediaID, "m_") {
		t.Fatalf("media id = %v, want m_*", r.Body["id"])
	}

	// ===== republishing the same container mints a second media (deviation, as-is) =====
	// No re-publish guard: the second threads_publish on a published container
	// succeeds with a fresh m_* id (the engine conformance test relies on it).
	r = publish(containerID)
	if r.Status != 201 {
		t.Fatalf("republish -> %d %v, want 201 (no re-publish guard, as-is)", r.Status, r.Body)
	}
	if second, _ := r.Body["id"].(string); second == mediaID || !strings.HasPrefix(second, "m_") {
		t.Fatalf("republish id = %v, want a fresh m_* distinct from %s", r.Body["id"], mediaID)
	}

	// ===== simulate_fail ends in error and never publishes =====
	// Simulator-only failure injection (documented deviation).
	r = create(map[string]any{"media_type": "TEXT", "text": "doomed", "simulate_fail": "true"})
	failID, _ := r.Body["id"].(string)
	if r.Status != 201 || failID == "" {
		t.Fatalf("simulate_fail create -> %d: %v", r.Status, r.Body)
	}
	if r := status(failID, "status"); r.Body["status"] != "error" {
		t.Fatalf("simulate_fail status = %v, want error", r.Body)
	}
	r = publish(failID)
	if r.Status != 400 {
		t.Fatalf("publish ERROR container -> %d, want 400", r.Status)
	}
	if e := thrErr(t, r); !thrNum(e["code"], 100) || !strings.Contains(e["message"].(string), "failed to process") {
		t.Fatalf("publish ERROR container error = %v, want code 100 failed-to-process", e)
	}

	// ===== unknown container ids answer the catch-all 404 =====
	for _, r := range []starlark.Response{publish("c_nope"), status("c_nope", "")} {
		if r.Status != 404 || !thrNum(thrErr(t, r)["code"], 404) {
			t.Fatalf("unknown container -> %d %v, want 404 code 404", r.Status, r.Body)
		}
	}
}

// TestThreadsEngagementInbox: the /{user}/threads edge lists the user's media
// newest first, each post carrying a parseable Graph timestamp and one
// synthetic reply child; the list is scoped to the path user and ignores the
// ?fields= projection.
func TestThreadsEngagementInbox(t *testing.T) {
	f := newThreadsFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, userID := f.thrMintUser()
	token2, user2 := f.thrMintUser()
	inbox := func(tok, user string, query map[string]string) starlark.Response {
		return f.call("engagement", "on_engagement", "GET", "/v1.0/"+user+"/threads",
			map[string]string{"id": user}, query, nil, "Bearer "+tok)
	}

	// Publish three posts an hour apart: oldest first is first, second, third.
	first := f.thrPost(token, userID, "first post")
	f.vc.Advance(time.Hour)
	second := f.thrPost(token, userID, "second post")
	f.vc.Advance(time.Hour)
	third := f.thrPost(token, userID, "third post")
	other := f.thrPost(token2, user2, "someone else's post")

	// ===== the inbox lists the user's media newest first =====
	r := inbox(token, userID, nil)
	if r.Status != 200 {
		t.Fatalf("inbox -> %d: %v", r.Status, r.Body)
	}
	data := thrData(t, r)
	if len(data) != 3 {
		t.Fatalf("inbox has %d rows, want the user's 3", len(data))
	}
	wantOrder := []string{third, second, first}
	prev := time.Now()
	for i, item := range data {
		m := item.(map[string]any)
		if m["id"] != wantOrder[i] {
			t.Fatalf("inbox[%d] id = %v, want %s (newest first)", i, m["id"], wantOrder[i])
		}
		if m["text"] == "" {
			t.Fatalf("inbox[%d] text = %v, want the publish text", i, m["text"])
		}
		ts, err := time.Parse(thrGraphStampFmt, m["timestamp"].(string))
		if err != nil {
			t.Fatalf("inbox[%d] timestamp %q unparsable as a Graph stamp: %v", i, m["timestamp"], err)
		}
		if i > 0 && !ts.Before(prev) {
			t.Fatalf("inbox[%d] timestamp %v not before inbox[%d] %v (newest first)", i, ts, i-1, prev)
		}
		prev = ts
	}

	// ===== each post carries one synthetic reply child =====
	m := data[0].(map[string]any)
	replies, ok := m["replies"].(map[string]any)
	if !ok {
		t.Fatalf("post %v replies = %v, want the Graph edge wrapper", m["id"], m["replies"])
	}
	rd := replies["data"].([]any)
	if len(rd) != 1 {
		t.Fatalf("replies.data = %v, want exactly one synthetic reply", replies["data"])
	}
	reply := rd[0].(map[string]any)
	if reply["id"] != "r_"+third || reply["text"] == "" {
		t.Fatalf("reply = %v, want id r_%s with non-empty text", reply, third)
	}
	if _, err := time.Parse(thrGraphStampFmt, reply["timestamp"].(string)); err != nil {
		t.Fatalf("reply timestamp %q unparsable as a Graph stamp: %v", reply["timestamp"], err)
	}

	// ===== the inbox is scoped to the path user =====
	r = inbox(token2, user2, nil)
	data = thrData(t, r)
	if len(data) != 1 || data[0].(map[string]any)["id"] != other {
		t.Fatalf("user2 inbox = %v, want only its own post %s", data, other)
	}

	// ===== ?fields= projection is ignored (deviation, as-is) =====
	// The handler returns the full id/text/timestamp/replies shape whatever
	// fields= asks for; only the container-status node projects.
	r = inbox(token, userID, map[string]string{"fields": "id"})
	if row := thrData(t, r)[0].(map[string]any); len(row) != 4 {
		t.Fatalf("fields=id -> %v, want the full 4-key row (projection ignored)", row)
	}
}

// TestThreadsInsightsMetrics: the insights edge returns the four manifest
// metrics (views, likes, replies, reposts) as single-value series with
// deterministic per-media values, projects onto ?metric= (canonical order,
// unknown names dropped), and 404s unknown media.
func TestThreadsInsightsMetrics(t *testing.T) {
	f := newThreadsFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, userID := f.thrMintUser()
	mediaID := f.thrPost(token, userID, "measure me")
	insights := func(metric string) starlark.Response {
		query := map[string]string{}
		if metric != "" {
			query["metric"] = metric
		}
		return f.call("insights", "on_insights", "GET", "/v1.0/"+mediaID+"/insights",
			map[string]string{"id": mediaID}, query, nil, "Bearer "+token)
	}
	// Ranges from the handler's FNV-1a bit math: views 100..100099,
	// likes 0..999, replies 0..499, reposts 0..199.
	metricRange := map[string][2]int64{
		"views": {100, 100099}, "likes": {0, 999}, "replies": {0, 499}, "reposts": {0, 199},
	}
	titles := map[string]string{
		"views": "Views", "likes": "Likes", "replies": "Replies", "reposts": "Reposts",
	}
	valuesOf := func(r starlark.Response) map[string]int64 {
		out := map[string]int64{}
		for _, item := range thrData(t, r) {
			m := item.(map[string]any)
			name := m["name"].(string)
			series := m["values"].([]any)
			if len(series) != 1 {
				t.Fatalf("metric %q values = %v, want a single-period series", name, series)
			}
			v := series[0].(map[string]any)["value"]
			n, ok := v.(int64)
			if !ok {
				if fl, isf := v.(float64); isf {
					n, ok = int64(fl), true
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
	data := thrData(t, r)
	if len(data) != 4 {
		t.Fatalf("insights data has %d rows, want 4 metrics", len(data))
	}
	for i, want := range []string{"views", "likes", "replies", "reposts"} {
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

	// ===== ?metric= projects to the requested names in canonical order =====
	r = insights("likes,views")
	names := []string{}
	for _, item := range thrData(t, r) {
		names = append(names, item.(map[string]any)["name"].(string))
	}
	if !reflect.DeepEqual(names, []string{"views", "likes"}) {
		t.Fatalf("metric=likes,views -> %v, want [views likes] (canonical order)", names)
	}
	if got := valuesOf(insights("reposts")); len(got) != 1 {
		t.Fatalf("metric=reposts -> %v, want exactly reposts", got)
	}

	// ===== an unknown metric name empties the data set (deviation, as-is) =====
	// Real Graph answers an invalid metric with error code 100; the adapter
	// drops it silently.
	if r := insights("bogus"); r.Status != 200 || len(thrData(t, r)) != 0 {
		t.Fatalf("metric=bogus -> %d %v, want 200 with empty data", r.Status, r.Body)
	}

	// ===== insights for an unknown media id are the catch-all 404 =====
	// Bare envelope without type/fbtrace_id (assert as-is).
	r = f.call("insights", "on_insights", "GET", "/v1.0/m_nope/insights",
		map[string]string{"id": "m_nope"}, nil, nil, "Bearer "+token)
	if r.Status != 404 {
		t.Fatalf("insights unknown media -> %d, want 404", r.Status)
	}
	e := thrErr(t, r)
	if !thrNum(e["code"], 404) || e["message"] != "resource not found" {
		t.Fatalf("insights 404 envelope = %v, want the catch-all shape", e)
	}
	if _, has := e["type"]; has {
		t.Fatalf("insights 404 carries type = %v, want the bare catch-all envelope", e)
	}
}

// TestThreadsRefreshGrant: POST /oauth/access_token with
// grant_type=refresh_token accepts only a previously issued refresh token
// (single-use, rotating), mints a fresh 60-day access token bound to the same
// user without invalidating the old access token — and omits the rotated
// refresh token from the response (documented deviation).
func TestThreadsRefreshGrant(t *testing.T) {
	base := time.Unix(1_750_000_000, 0).UTC()
	f := newThreadsFixture(t, base)
	code := f.thrAuthorize(thrRedirectURI)
	r := f.thrExchange("authorization_code", code, thrClientID, thrClientSecret, thrRedirectURI)
	refresh, _ := r.Body["refresh_token"].(string)
	token, _ := r.Body["access_token"].(string)
	userID, _ := r.Body["user_id"].(string)
	if refresh == "" || token == "" || userID == "" {
		t.Fatalf("code exchange = %v, want token + refresh_token + user_id", r.Body)
	}
	refreshCall := func(rt string) starlark.Response {
		return f.call("oauth", "on_access_token", "POST", "/oauth/access_token", nil, nil,
			map[string]any{"grant_type": "refresh_token", "refresh_token": rt}, "")
	}
	gate := func(tok string) int {
		return f.call("profile", "on_profile", "GET", "/v1.0/me", nil, nil, nil, "Bearer "+tok).Status
	}

	// ===== refresh demands a known refresh_token =====
	for name, rt := range map[string]string{"missing": "", "unknown": "mock_refresh_nope"} {
		if r := refreshCall(rt); r.Status != 400 || r.Body["error"] != "invalid_grant" {
			t.Fatalf("refresh %s -> %d %v, want 400 invalid_grant", name, r.Status, r.Body)
		}
	}

	// ===== a refresh mints a fresh 60-day token bound to the same user =====
	f.vc.Advance(30 * 24 * time.Hour) // half the original's life
	r = refreshCall(refresh)
	if r.Status != 200 {
		t.Fatalf("refresh -> %d: %v", r.Status, r.Body)
	}
	fresh, _ := r.Body["access_token"].(string)
	if fresh == "" || fresh == token {
		t.Fatalf("refreshed token = %v, want a fresh mint", r.Body["access_token"])
	}
	if r.Body["user_id"] != userID || r.Body["token_type"] != "bearer" || !thrNum(r.Body["expires_in"], 5184000) {
		t.Fatalf("refresh response = %v, want the same user %s, bearer, 60d", r.Body, userID)
	}
	if gate(fresh) != 200 {
		t.Fatal("refreshed token rejected at the gate, want it valid")
	}

	// ===== the response omits the rotated refresh_token (deviation, as-is) =====
	// A new mock_refresh_* binding is written but never returned, so the
	// client can only refresh once per issued token (documented).
	if _, has := r.Body["refresh_token"]; has {
		t.Fatalf("refresh response carries refresh_token = %v, want it omitted (as-is)", r.Body)
	}

	// ===== the used refresh token is single-use: a replay is invalid_grant =====
	if r := refreshCall(refresh); r.Status != 400 || r.Body["error"] != "invalid_grant" {
		t.Fatalf("refresh replay -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}

	// ===== the original access token keeps its own 60-day life =====
	// No rotation invalidation: refreshing does not revoke the old token.
	if gate(token) != 200 {
		t.Fatal("original token invalidated by refresh, want it valid until its own expiry")
	}

	// ===== the refreshed token outlives the original's expiry =====
	f.vc.Advance(31 * 24 * time.Hour) // 61d past the original mint
	if gate(token) != 401 {
		t.Fatal("original token still valid at 61d, want 401 (60-day life)")
	}
	if gate(fresh) != 200 {
		t.Fatal("refreshed token invalid at 31d past its mint, want valid for its own 60 days")
	}
}
