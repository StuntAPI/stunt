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

// These tests drive the linkedin-style adapter scripts directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock: the Arctic-style OAuth2
// authorization-code flow with body-param client credentials, single-use
// refresh-token rotation, the Bearer gate (LinkedIn's flat {status, code,
// message} service error envelope, 60-day clock-derived expiry), the
// token-bound /v2/userinfo profile, ugcPosts publishing with its author
// authorization and KV-armed rate-limit injection, post resolution (ugcPost
// -> share URN), the comments ingest/reply pair with q=author scoping and
// count/start paging whose next link round-trips the query, and the
// memberCreatorPostAnalytics metric buckets.

// One OAuth client for the whole suite; the adapter accepts any values.
const (
	liRedirectURI   = "http://localhost:3000/callback"
	liState         = "vm-suite-state"
	liClientID      = "li-vm-client-id"
	liClientSecret  = "li-vm-client-secret"
	liShareContent  = "com.linkedin.ugc.ShareContent"
	liMetricTypeKey = "com.linkedin.adsexternalapi.memberanalytics.v1.CreatorPostAnalyticsMetricTypeV1"
)

// liFixture is one shared store + virtual clock with a loaded VM per handler
// script (oauth, userinfo, posts, comments and analytics each get their own
// VM, but they observe the same collections/kv state, like the engine).
type liFixture struct {
	t   *testing.T
	vc  *clock.Clock
	kv  *kv.KV
	vms map[string]*starlark.VM
}

func newLinkedinFixture(t *testing.T, start time.Time) *liFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "linkedin-style")
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
	return &liFixture{t: t, vc: vc, kv: kvStore, vms: map[string]*starlark.VM{
		"oauth": load("oauth.star"), "userinfo": load("userinfo.star"),
		"posts": load("posts.star"), "comments": load("comments.star"),
		"analytics": load("analytics.star"),
	}}
}

// call invokes handler on the named script VM; auth is the full
// Authorization header value ("" = header absent).
func (f *liFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: "api.linkedin.test",
		Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// liNum compares a response number against want whether it arrives as an
// int (handler literal) or a float (round-tripped through a collection).
func liNum(got any, want int64) bool {
	switch n := got.(type) {
	case int64:
		return n == want
	case float64:
		return n == float64(want)
	}
	return false
}

// liInt coerces a response number (int or round-tripped float) to int64.
func liInt(got any) (int64, bool) {
	switch n := got.(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}

// liAuthorize runs the authorization redirect against redirectURI and
// returns the single-use code it mints.
func (f *liFixture) liAuthorize(redirectURI string) string {
	f.t.Helper()
	r := f.call("oauth", "on_authorize", "GET", "/oauth/v2/authorization", nil, map[string]string{
		"client_id": liClientID, "redirect_uri": redirectURI, "state": liState,
		"response_type": "code", "scope": "openid profile w_member_social email",
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

// liExchangeCode trades an authorization code (body-param client creds, the
// Arctic convention — no HTTP Basic Auth).
func (f *liFixture) liExchangeCode(code, clientID, secret, redirectURI string) starlark.Response {
	f.t.Helper()
	body := map[string]any{"grant_type": "authorization_code", "code": code}
	if clientID != "" {
		body["client_id"] = clientID
	}
	if secret != "" {
		body["client_secret"] = secret
	}
	if redirectURI != "" {
		body["redirect_uri"] = redirectURI
	}
	return f.call("oauth", "on_access_token", "POST", "/oauth/v2/accessToken", nil, nil, body, "")
}

// liRefresh presents a refresh token for rotation.
func (f *liFixture) liRefresh(token string, withCreds bool) starlark.Response {
	f.t.Helper()
	body := map[string]any{"grant_type": "refresh_token", "refresh_token": token}
	if withCreds {
		body["client_id"] = liClientID
		body["client_secret"] = liClientSecret
	}
	return f.call("oauth", "on_access_token", "POST", "/oauth/v2/accessToken", nil, nil, body, "")
}

// liUserinfo resolves a bearer to its member document.
func (f *liFixture) liUserinfo(token string) starlark.Response {
	f.t.Helper()
	return f.call("userinfo", "on_userinfo", "GET", "/v2/userinfo", nil, nil, nil, "Bearer "+token)
}

// liMint runs the authorization-code flow end to end and returns the minted
// (token, member sub).
func (f *liFixture) liMint() (string, string) {
	f.t.Helper()
	code := f.liAuthorize(liRedirectURI)
	r := f.liExchangeCode(code, liClientID, liClientSecret, liRedirectURI)
	if r.Status != 200 {
		f.t.Fatalf("accessToken -> %d: %v", r.Status, r.Body)
	}
	token, _ := r.Body["access_token"].(string)
	if token == "" {
		f.t.Fatalf("accessToken response = %v, want an access_token", r.Body)
	}
	u := f.liUserinfo(token)
	if u.Status != 200 {
		f.t.Fatalf("userinfo after exchange -> %d: %v", u.Status, u.Body)
	}
	sub, _ := u.Body["sub"].(string)
	return token, sub
}

// liPublish posts ugcPosts content as author; author may deliberately
// mismatch the token's member.
func (f *liFixture) liPublish(token, author, text string) starlark.Response {
	f.t.Helper()
	return f.call("posts", "on_ugc_posts", "POST", "/v2/ugcPosts", nil, nil, map[string]any{
		"author":         author,
		"lifecycleState": "PUBLISHED",
		"specificContent": map[string]any{
			liShareContent: map[string]any{
				"shareCommentary":    map[string]any{"text": text},
				"shareMediaCategory": "NONE",
			},
		},
		"visibility": map[string]any{"com.linkedin.ugc.MemberNetworkVisibility": "PUBLIC"},
	}, "Bearer "+token)
}

// liResolvePost GETs /rest/posts/{urlencoded urn}.
func (f *liFixture) liResolvePost(token, urn string) starlark.Response {
	f.t.Helper()
	return f.call("posts", "on_resolve_post", "GET", "/rest/posts/"+url.PathEscape(urn),
		map[string]string{"urn": urn}, nil, nil, "Bearer "+token)
}

// liComment posts a comment on objectUrn; actor may deliberately mismatch.
func (f *liFixture) liComment(token, actor, objectUrn, text string) starlark.Response {
	f.t.Helper()
	return f.call("comments", "on_post_comment", "POST", "/rest/comments", nil, nil, map[string]any{
		"actor":   actor,
		"object":  objectUrn,
		"message": map[string]any{"text": text},
	}, "Bearer "+token)
}

// liIngest lists the token member's comments (q=author by default).
func (f *liFixture) liIngest(token string, query map[string]string) starlark.Response {
	f.t.Helper()
	if query == nil {
		query = map[string]string{"q": "author"}
	}
	return f.call("comments", "on_list_comments", "GET", "/rest/comments", nil, query, nil, "Bearer "+token)
}

// liElements returns the response's elements array.
func liElements(t *testing.T, r starlark.Response) []any {
	t.Helper()
	elements, ok := r.Body["elements"].([]any)
	if !ok {
		t.Fatalf("response %d elements = %v, want an array", r.Status, r.Body["elements"])
	}
	return elements
}

// liPaging returns the response's paging block.
func liPaging(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	paging, ok := r.Body["paging"].(map[string]any)
	if !ok {
		t.Fatalf("response %d carries no paging block: %v", r.Status, r.Body)
	}
	return paging
}

// TestLinkedinOAuthAuthorizationCodeFlow: the authorize redirect demands its
// three params and redirects with a fresh code + state echo, the exchange is
// grant-type checked with body-param client credentials, codes are
// single-use, and client mismatches are rejected without burning the code.
func TestLinkedinOAuthAuthorizationCodeFlow(t *testing.T) {
	f := newLinkedinFixture(t, time.Unix(1_750_000_000, 0).UTC())
	authorize := func(query map[string]string) starlark.Response {
		return f.call("oauth", "on_authorize", "GET", "/oauth/v2/authorization", nil, query, nil, "")
	}

	// ===== authorize without redirect_uri, state or client_id is invalid_request =====
	// Each missing param alone is a 400.
	for name, query := range map[string]map[string]string{
		"no redirect_uri": {"client_id": liClientID, "state": liState},
		"no state":        {"client_id": liClientID, "redirect_uri": liRedirectURI},
		"no client_id":    {"state": liState, "redirect_uri": liRedirectURI},
	} {
		if r := authorize(query); r.Status != 400 || r.Body["error"] != "invalid_request" {
			t.Fatalf("authorize %s -> %d %v, want 400 invalid_request", name, r.Status, r.Body)
		}
	}

	// ===== authorize redirects back with a fresh code and the state echoed =====
	code := f.liAuthorize(liRedirectURI)
	if !strings.HasPrefix(code, "mock_code_") {
		t.Fatalf("authorize code = %q, want a mock_code_* mint", code)
	}
	r := authorize(map[string]string{
		"client_id": liClientID, "redirect_uri": liRedirectURI, "state": liState,
	})
	loc, err := url.Parse(r.Headers["Location"])
	if err != nil {
		t.Fatalf("authorize Location %q: %v", r.Headers["Location"], err)
	}
	if loc.Query().Get("state") != liState {
		t.Fatalf("authorize Location %q, want the caller's state echoed", r.Headers["Location"])
	}

	// ===== a redirect_uri that already carries a query is joined with & =====
	r = authorize(map[string]string{
		"client_id": liClientID, "redirect_uri": "http://localhost:3000/cb?from=vm", "state": liState,
	})
	if loc := r.Headers["Location"]; !strings.Contains(loc, "cb?from=vm&code=") {
		t.Fatalf("authorize Location = %q, want the existing query joined with &", loc)
	}

	// ===== the exchange demands grant_type=authorization_code =====
	// Wrong grant types are 400 unsupported_grant_type (RFC 6749 shape).
	if r := f.call("oauth", "on_access_token", "POST", "/oauth/v2/accessToken", nil, nil,
		map[string]any{"grant_type": "client_credentials", "code": code}, ""); r.Status != 400 ||
		r.Body["error"] != "unsupported_grant_type" {
		t.Fatalf("client_credentials grant -> %d %v, want 400 unsupported_grant_type", r.Status, r.Body)
	}

	// ===== an unknown code is 400 invalid_grant =====
	if r := f.liExchangeCode("mock_code_nope", liClientID, liClientSecret, liRedirectURI); r.Status != 400 ||
		r.Body["error"] != "invalid_grant" {
		t.Fatalf("unknown code -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}

	// ===== a good exchange mints a 60-day token pair for a fresh member =====
	// Client creds ride the body (Arctic), never HTTP Basic Auth.
	fresh := f.liAuthorize(liRedirectURI)
	r = f.liExchangeCode(fresh, liClientID, liClientSecret, liRedirectURI)
	if r.Status != 200 {
		t.Fatalf("access_token -> %d: %v", r.Status, r.Body)
	}
	access, _ := r.Body["access_token"].(string)
	refresh, _ := r.Body["refresh_token"].(string)
	if !strings.HasPrefix(access, "mock_access_") || !strings.HasPrefix(refresh, "mock_refresh_") {
		t.Fatalf("exchange minted %q / %q, want mock_access_* / mock_refresh_*", access, refresh)
	}
	if !liNum(r.Body["expires_in"], 5184000) {
		t.Fatalf("expires_in = %v (%T), want 5184000 (60 days)", r.Body["expires_in"], r.Body["expires_in"])
	}
	if r.Body["scope"] != "openid profile w_member_social email" {
		t.Fatalf("scope = %v, want the member/social scope set", r.Body["scope"])
	}

	// ===== the code is single-use: a replay is invalid_grant =====
	if r := f.liExchangeCode(fresh, liClientID, liClientSecret, liRedirectURI); r.Status != 400 ||
		r.Body["error"] != "invalid_grant" {
		t.Fatalf("code replay -> %d %v, want 400 invalid_grant (single-use)", r.Status, r.Body)
	}

	// ===== client mismatches are 400 invalid_client =====
	// client_id, redirect_uri and client_secret must all match the authorize;
	// each case gets a fresh code (codes are single-use).
	for _, tc := range []struct{ name, clientID, secret, redirectURI string }{
		{"wrong client_id", "attacker-app", liClientSecret, liRedirectURI},
		{"wrong redirect_uri", liClientID, liClientSecret, "http://evil.test/cb"},
		{"missing secret", liClientID, "", liRedirectURI},
	} {
		r := f.liExchangeCode(f.liAuthorize(liRedirectURI), tc.clientID, tc.secret, tc.redirectURI)
		if r.Status != 400 || r.Body["error"] != "invalid_client" {
			t.Fatalf("exchange %s -> %d %v, want 400 invalid_client", tc.name, r.Status, r.Body)
		}
	}

	// ===== a mismatched attempt must not burn the code =====
	// The wrong client's failed exchange leaves the code redeemable by the
	// right one (the delete happens on the matched path only).
	code3 := f.liAuthorize(liRedirectURI)
	f.liExchangeCode(code3, "attacker-app", liClientSecret, liRedirectURI) // 400, above
	if r := f.liExchangeCode(code3, liClientID, liClientSecret, liRedirectURI); r.Status != 200 {
		t.Fatalf("right client after a mismatched attempt -> %d %v, want 200 (code not burned)", r.Status, r.Body)
	}

	// ===== a second flow mints a distinct member =====
	_, sub1 := f.liMint()
	_, sub2 := f.liMint()
	if sub1 == "" || sub2 == "" || sub1 == sub2 {
		t.Fatalf("flows minted subs %q / %q, want distinct non-empty members", sub1, sub2)
	}
}

// TestLinkedinRefreshTokenRotation: the refresh grant demands body-param
// client creds, rotates the pair single-use while keeping the same member,
// leaves the old access token valid until its own expiry, and chains.
func TestLinkedinRefreshTokenRotation(t *testing.T) {
	f := newLinkedinFixture(t, time.Unix(1_750_000_000, 0).UTC())
	code := f.liAuthorize(liRedirectURI)
	r := f.liExchangeCode(code, liClientID, liClientSecret, liRedirectURI)
	if r.Status != 200 {
		t.Fatalf("access_token -> %d: %v", r.Status, r.Body)
	}
	access1, _ := r.Body["access_token"].(string)
	refresh1, _ := r.Body["refresh_token"].(string)
	sub1, _ := f.liUserinfo(access1).Body["sub"].(string)

	// ===== the refresh grant demands client creds =====
	if r := f.liRefresh(refresh1, false); r.Status != 400 || r.Body["error"] != "invalid_client" {
		t.Fatalf("refresh without client creds -> %d %v, want 400 invalid_client", r.Status, r.Body)
	}

	// ===== an unknown refresh token is invalid_grant =====
	if r := f.liRefresh("mock_refresh_nope", true); r.Status != 400 || r.Body["error"] != "invalid_grant" {
		t.Fatalf("unknown refresh token -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}

	// ===== refresh rotates the pair and keeps the member =====
	r = f.liRefresh(refresh1, true)
	if r.Status != 200 {
		t.Fatalf("refresh -> %d: %v", r.Status, r.Body)
	}
	access2, _ := r.Body["access_token"].(string)
	refresh2, _ := r.Body["refresh_token"].(string)
	if access2 == "" || access2 == access1 || refresh2 == "" || refresh2 == refresh1 {
		t.Fatalf("refresh minted %q / %q, want a fresh pair distinct from %q / %q", access2, refresh2, access1, refresh1)
	}
	if !liNum(r.Body["expires_in"], 5184000) {
		t.Fatalf("refresh expires_in = %v, want 5184000", r.Body["expires_in"])
	}
	u := f.liUserinfo(access2)
	if u.Status != 200 || u.Body["sub"] != sub1 {
		t.Fatalf("userinfo with rotated token -> %d %v, want the same member %q", u.Status, u.Body, sub1)
	}
	// Rotation consumes only the refresh token: the old access token stays
	// valid until its own expiry, like real LinkedIn.
	if u := f.liUserinfo(access1); u.Status != 200 {
		t.Fatalf("original access token after refresh -> %d, want still valid", u.Status)
	}

	// ===== the presented refresh token is single-use =====
	if r := f.liRefresh(refresh1, true); r.Status != 400 || r.Body["error"] != "invalid_grant" {
		t.Fatalf("refresh replay -> %d %v, want 400 invalid_grant (single-use rotation)", r.Status, r.Body)
	}

	// ===== rotation chains: the new refresh token refreshes again =====
	if r := f.liRefresh(refresh2, true); r.Status != 200 {
		t.Fatalf("second-generation refresh -> %d %v, want 200", r.Status, r.Body)
	}
}

// TestLinkedinBearerGate: every API route validates the Bearer against the
// tokens collection — missing, wrong-scheme, unknown and expired tokens all
// answer 401 in LinkedIn's service error envelope {status, code: AUTHORIZED}.
func TestLinkedinBearerGate(t *testing.T) {
	f := newLinkedinFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, _ := f.liMint()

	// ===== a missing bearer is 401 in the service error envelope =====
	// The envelope is flat {status, code, message} (no nested "error").
	r := f.liUserinfo("")
	if r.Status != 401 {
		t.Fatalf("no bearer -> %d, want 401", r.Status)
	}
	if !liNum(r.Body["status"], 401) || r.Body["code"] != "AUTHORIZED" || r.Body["message"] == nil {
		t.Fatalf("401 envelope = %v, want status 401 code AUTHORIZED with a message", r.Body)
	}
	if _, nested := r.Body["error"]; nested {
		t.Fatalf("401 envelope nests an error object = %v, want the flat shape", r.Body)
	}

	// ===== wrong schemes and unknown bearers answer the same 401 =====
	for _, auth := range []string{
		"Token " + token, // wrong scheme
		token,            // bare token, no scheme
		"Bearer totally-fake-token",
	} {
		r := f.call("userinfo", "on_userinfo", "GET", "/v2/userinfo", nil, nil, nil, auth)
		if r.Status != 401 || r.Body["code"] != "AUTHORIZED" {
			t.Fatalf("auth %q -> %d %v, want 401 AUTHORIZED", auth, r.Status, r.Body)
		}
	}

	// ===== every API route enforces the same gate =====
	// All six API handlers reject a missing and an unknown bearer. The 401
	// message string varies by route (userinfo's longer text vs "token") —
	// asserted as-is.
	routes := []struct {
		group, handler, method, path string
		params, query                map[string]string
		body                         map[string]any
	}{
		{"userinfo", "on_userinfo", "GET", "/v2/userinfo", nil, nil, nil},
		{"posts", "on_ugc_posts", "POST", "/v2/ugcPosts", nil, nil, nil},
		{"posts", "on_resolve_post", "GET", "/rest/posts/urn:li:ugcPost:1",
			map[string]string{"urn": "urn:li:ugcPost:1"}, nil, nil},
		{"comments", "on_list_comments", "GET", "/rest/comments", nil, map[string]string{"q": "author"}, nil},
		{"comments", "on_post_comment", "POST", "/rest/comments", nil, nil, nil},
		{"analytics", "on_analytics", "GET", "/rest/memberCreatorPostAnalytics",
			nil, map[string]string{"q": "entity", "entity": "(ugcPost:urn:li:ugcPost:1)", "queryType": "REACTION"}, nil},
	}
	for _, rt := range routes {
		for _, auth := range []string{"", "Bearer unknown-token"} {
			r := f.call(rt.group, rt.handler, rt.method, rt.path, rt.params, rt.query, rt.body, auth)
			if r.Status != 401 {
				t.Fatalf("%s (auth %q) -> %d, want 401", rt.path, auth, r.Status)
			}
			if r.Body["code"] != "AUTHORIZED" || !liNum(r.Body["status"], 401) {
				t.Fatalf("%s 401 envelope = %v, want the AUTHORIZED shape", rt.path, r.Body)
			}
		}
	}

	// ===== a bearer dies at its clock-derived 60-day expiry =====
	// expires_at is minted from the engine clock; the clock is virtual.
	f.vc.Advance(61 * 24 * time.Hour)
	if r := f.liUserinfo(token); r.Status != 401 || r.Body["code"] != "AUTHORIZED" {
		t.Fatalf("expired bearer -> %d %v, want 401 AUTHORIZED", r.Status, r.Body)
	}
}

// TestLinkedinUserinfoAndPublish: /v2/userinfo answers the member bound to
// the bearer, ugcPosts authorizes the author against that member, mints a
// urn:li:ugcPost echoed in the x-linkedin-id header, and /rest/posts/{urn}
// resolves it to a share URN carrying the post's own author.
func TestLinkedinUserinfoAndPublish(t *testing.T) {
	f := newLinkedinFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, sub := f.liMint()
	person := "urn:li:person:" + sub

	// ===== userinfo returns the OAuth member profile =====
	u := f.liUserinfo(token)
	if u.Status != 200 {
		t.Fatalf("userinfo -> %d: %v", u.Status, u.Body)
	}
	if u.Body["sub"] != sub || !strings.HasPrefix(sub, "mock-member-") {
		t.Fatalf("userinfo sub = %v, want the minted mock-member-*", u.Body["sub"])
	}
	name, _ := u.Body["name"].(string)
	email, _ := u.Body["email"].(string)
	picture, _ := u.Body["picture"].(string)
	if !strings.HasPrefix(name, "Mock Member ") || !strings.Contains(email, "@example.test") ||
		!strings.Contains(picture, sub) {
		t.Fatalf("userinfo profile = %v, want the minted name/email/picture", u.Body)
	}

	// ===== publishing as anyone but the token's member is a 403 =====
	for _, author := range []string{"urn:li:person:someone-else", ""} {
		r := f.liPublish(token, author, "forged")
		if r.Status != 403 {
			t.Fatalf("publish as %q -> %d, want 403", author, r.Status)
		}
		if r.Body["code"] != "FIELDS_DATA_VALIDATION_EXCEPTION" || !liNum(r.Body["status"], 403) {
			t.Fatalf("publish 403 envelope = %v, want FIELDS_DATA_VALIDATION_EXCEPTION", r.Body)
		}
	}

	// ===== a good publish mints a ugcPost urn echoed in x-linkedin-id =====
	r := f.liPublish(token, person, "hello from the VM suite")
	if r.Status != 201 {
		t.Fatalf("publish -> %d: %v", r.Status, r.Body)
	}
	urn, _ := r.Body["id"].(string)
	if !strings.HasPrefix(urn, "urn:li:ugcPost:") {
		t.Fatalf("publish id = %v, want a urn:li:ugcPost:* mint", r.Body["id"])
	}
	if r.Headers["x-linkedin-id"] != urn {
		t.Fatalf("x-linkedin-id = %q, want the created urn %q", r.Headers["x-linkedin-id"], urn)
	}

	// ===== the post resolves to a share urn carrying its own author =====
	// Any member may resolve any post; the author is the post's, not the
	// caller's.
	seq := strings.TrimPrefix(urn, "urn:li:ugcPost:")
	res := f.liResolvePost(token, urn)
	if res.Status != 200 {
		t.Fatalf("resolve -> %d: %v", res.Status, res.Body)
	}
	if res.Body["id"] != "urn:li:share:"+seq || res.Body["author"] != person {
		t.Fatalf("resolve = %v, want share urn for seq %s authored by %s", res.Body, seq, person)
	}
	token2, _ := f.liMint()
	if res := f.liResolvePost(token2, urn); res.Status != 200 || res.Body["author"] != person {
		t.Fatalf("resolve by a second member = %v, want the post's own author %s", res.Body, person)
	}

	// ===== resolving an unknown urn is a 404 =====
	if r := f.liResolvePost(token, "urn:li:ugcPost:999999"); r.Status != 404 ||
		!liNum(r.Body["status"], 404) || r.Body["message"] != "post not found" {
		t.Fatalf("resolve unknown urn -> %d %v, want 404 post not found", r.Status, r.Body)
	}
}

// TestLinkedinPublishRateLimit: the publish path counts posts per member URN
// and, once the linkedin/fail_after KV knob is armed, answers 429
// REQUEST_LIMIT_EXCEEDED past the threshold without consuming a post seq.
func TestLinkedinPublishRateLimit(t *testing.T) {
	f := newLinkedinFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, sub := f.liMint()
	person := "urn:li:person:" + sub

	seqOf := func(r starlark.Response) int64 {
		f.t.Helper()
		urn, _ := r.Body["id"].(string)
		n, err := strconv.ParseInt(strings.TrimPrefix(urn, "urn:li:ugcPost:"), 10, 64)
		if err != nil {
			f.t.Fatalf("post urn %q carries no numeric seq: %v", urn, err)
		}
		return n
	}

	// ===== unconfigured, publishing is unthrottled =====
	lastSeq := int64(0)
	for i := 0; i < 5; i++ {
		r := f.liPublish(token, person, "burst")
		if r.Status != 201 {
			t.Fatalf("publish %d with no fail_after -> %d %v, want 201", i+1, r.Status, r.Body)
		}
		lastSeq = seqOf(r)
	}
	if lastSeq < 5 {
		t.Fatalf("five publishes advanced the seq to %d, want >= 5", lastSeq)
	}

	// ===== arming fail_after injects 429 REQUEST_LIMIT_EXCEEDED =====
	if err := f.kv.Set("linkedin", "fail_after", "1"); err != nil {
		t.Fatalf("arm fail_after: %v", err)
	}
	for i := 0; i < 2; i++ {
		r := f.liPublish(token, person, "over the limit")
		if r.Status != 429 {
			t.Fatalf("publish past fail_after=1 (#%d) -> %d %v, want 429", i+1, r.Status, r.Body)
		}
		if r.Body["code"] != "REQUEST_LIMIT_EXCEEDED" || !liNum(r.Body["status"], 429) {
			t.Fatalf("429 envelope = %v, want REQUEST_LIMIT_EXCEEDED", r.Body)
		}
	}

	// ===== the limit is per member =====
	token2, sub2 := f.liMint()
	r2 := f.liPublish(token2, "urn:li:person:"+sub2, "unaffected")
	if r2.Status != 201 {
		t.Fatalf("second member publish while the first is throttled -> %d %v, want 201", r2.Status, r2.Body)
	}
	bSeq := seqOf(r2)

	// ===== a throttled attempt creates no post =====
	// The 429 path returns before post_seq is consumed, so the next accepted
	// post (after disarming) takes the very next seq after B's.
	if err := f.kv.Delete("linkedin", "fail_after"); err != nil {
		t.Fatalf("disarm fail_after: %v", err)
	}
	next := seqOf(f.liPublish(token, person, "back online"))
	if next != bSeq+1 {
		t.Fatalf("first post after the 429s has seq %d, want %d (no seqs burned)", next, bSeq+1)
	}
	if r := f.liResolvePost(token, "urn:li:ugcPost:"+strconv.FormatInt(next+1, 10)); r.Status != 404 {
		t.Fatalf("resolve the seq after the last created -> %d, want 404 (throttled posts were never stored)", r.Status)
	}
}

// TestLinkedinCommentsIngestReply: POST /rest/comments resolves
// urn:li:person:me to the authenticated member (and refuses any other actor),
// 404s unknown objects, and GET /rest/comments?q=author lists only the token
// member's comments with clock-stamped createdOn and count/start paging whose
// next link round-trips the query.
func TestLinkedinCommentsIngestReply(t *testing.T) {
	f := newLinkedinFixture(t, time.Unix(1_750_000_000, 0).UTC())
	tokenA, subA := f.liMint()
	personA := "urn:li:person:" + subA
	tokenB, subB := f.liMint()
	personB := "urn:li:person:" + subB

	post, _ := f.liPublish(tokenA, personA, "the commented post").Body["id"].(string)
	if post == "" {
		t.Fatal("setup publish failed")
	}

	// ===== q must be author =====
	for _, q := range []string{"", "reader"} {
		query := map[string]string{}
		if q != "" {
			query["q"] = q
		}
		if r := f.liIngest(tokenA, query); r.Status != 400 || r.Body["message"] != "unsupported query" {
			t.Fatalf("q=%q -> %d %v, want 400 unsupported query", q, r.Status, r.Body)
		}
	}

	// ===== reply resolves urn:li:person:me to the authenticated member =====
	// Three comments for A an hour apart (virtual clock); the ids round-trip
	// through ingest below.
	var idsA []string
	for i, text := range []string{"first", "second", "third"} {
		if i > 0 {
			f.vc.Advance(time.Hour)
		}
		r := f.liComment(tokenA, "urn:li:person:me", post, text)
		if r.Status != 201 {
			t.Fatalf("comment %d -> %d: %v", i+1, r.Status, r.Body)
		}
		id, _ := r.Body["id"].(string)
		if !strings.HasPrefix(id, "urn:li:comment:") {
			t.Fatalf("comment id = %v, want a urn:li:comment:* mint", r.Body["id"])
		}
		idsA = append(idsA, id)
	}
	rB := f.liComment(tokenB, "urn:li:person:me", post, "from member B")
	if rB.Status != 201 {
		t.Fatalf("member B comment -> %d: %v", rB.Status, rB.Body)
	}
	idB, _ := rB.Body["id"].(string)

	// ===== commenting as anyone but the caller is a 403 =====
	for _, actor := range []string{personB, ""} {
		r := f.liComment(tokenA, actor, post, "forged")
		if r.Status != 403 || r.Body["code"] != "FIELDS_DATA_VALIDATION_EXCEPTION" {
			t.Fatalf("comment as %q -> %d %v, want 403 FIELDS_DATA_VALIDATION_EXCEPTION", actor, r.Status, r.Body)
		}
	}

	// ===== replying to an unknown object is a 404 =====
	if r := f.liComment(tokenA, "urn:li:person:me", "urn:li:ugcPost:999999", "orphan"); r.Status != 404 ||
		r.Body["message"] != "object not found" {
		t.Fatalf("comment on unknown object -> %d %v, want 404 object not found", r.Status, r.Body)
	}

	// ===== ingest lists only the token member's comments =====
	r := f.liIngest(tokenA, nil)
	if r.Status != 200 {
		t.Fatalf("ingest -> %d: %v", r.Status, r.Body)
	}
	elements := liElements(t, r)
	if len(elements) != 3 {
		t.Fatalf("member A ingest has %d elements, want its 3", len(elements))
	}
	for i, item := range elements {
		c := item.(map[string]any)
		if c["id"] != idsA[i] {
			t.Fatalf("elements[%d].id = %v, want %s (insertion order)", i, c["id"], idsA[i])
		}
		if c["actor"] != personA || c["object"] != post {
			t.Fatalf("elements[%d] actor/object = %v / %v, want %s / %s", i, c["actor"], c["object"], personA, post)
		}
		msg, _ := c["message"].(map[string]any)
		if msg["text"] == "" {
			t.Fatalf("elements[%d].message.text = %v, want non-empty", i, msg["text"])
		}
		if _, has := c["lastModified"]; !has {
			t.Fatalf("elements[%d] carries no lastModified: %v", i, c)
		}
	}

	// ===== member B's comment resolved me and lists only under B =====
	elementsB := liElements(t, f.liIngest(tokenB, nil))
	if len(elementsB) != 1 || elementsB[0].(map[string]any)["id"] != idB {
		t.Fatalf("member B ingest = %v, want only its comment %s", elementsB, idB)
	}
	if got := elementsB[0].(map[string]any)["actor"]; got != personB {
		t.Fatalf("member B comment actor = %v, want %s (me resolved to the member)", got, personB)
	}

	// ===== createdOn is clock-stamped and monotonic =====
	// The stamps advance with the virtual clock (one hour per comment), not a
	// frozen constant.
	stamps := make([]int64, 0, len(elements))
	for i, item := range elements {
		lm := item.(map[string]any)["lastModified"].(map[string]any)
		n, ok := liInt(lm["createdOn"])
		if !ok {
			t.Fatalf("createdOn = %v (%T), want an epoch-ms number", lm["createdOn"], lm["createdOn"])
		}
		stamps = append(stamps, n)
		if stamps[0] < 1_750_000_000_000 {
			t.Fatalf("createdOn[%d] = %d, want at least the virtual clock start in ms", i, n)
		}
	}
	for i := 1; i < len(stamps); i++ {
		if stamps[i] <= stamps[i-1] {
			t.Fatalf("createdOn not monotonic: %v", stamps)
		}
	}
	if stamps[2]-stamps[0] < 2*3600*1000 {
		t.Fatalf("createdOn spread = %d ms, want at least the two 1h advances", stamps[2]-stamps[0])
	}

	// ===== count pages with a next link that round-trips the query =====
	r = f.liIngest(tokenA, map[string]string{"q": "author", "count": "2"})
	page1 := liElements(t, r)
	if len(page1) != 2 || page1[0].(map[string]any)["id"] != idsA[0] {
		t.Fatalf("page 1 = %v, want the first two comments", page1)
	}
	paging := liPaging(t, r)
	if !liNum(paging["count"], 2) || !liNum(paging["start"], 0) {
		t.Fatalf("page 1 paging = %v, want count 2 start 0", paging)
	}
	links, _ := paging["links"].([]any)
	if len(links) != 1 {
		t.Fatalf("page 1 links = %v, want exactly the next link", paging["links"])
	}
	link := links[0].(map[string]any)
	wantHref := "/rest/comments?q=author&count=2&start=2"
	if link["rel"] != "next" || link["href"] != wantHref {
		t.Fatalf("next link = %v, want %q (the query survives the hop)", link, wantHref)
	}
	// Following the advertised href verbatim must not 400 on the q check.
	r = f.liIngest(tokenA, map[string]string{"q": "author", "count": "2", "start": "2"})
	page2 := liElements(t, r)
	if len(page2) != 1 || page2[0].(map[string]any)["id"] != idsA[2] {
		t.Fatalf("page 2 = %v, want the last comment", page2)
	}
	paging = liPaging(t, r)
	if !liNum(paging["count"], 1) || !liNum(paging["start"], 2) {
		t.Fatalf("page 2 paging = %v, want count 1 start 2", paging)
	}
	if links, _ := paging["links"].([]any); len(links) != 0 {
		t.Fatalf("final page links = %v, want none", paging["links"])
	}

	// ===== without count the whole list returns unpaged =====
	r = f.liIngest(tokenA, nil)
	if got := len(liElements(t, r)); got != 3 {
		t.Fatalf("unpaged ingest has %d elements, want all 3", got)
	}
	if links, _ := liPaging(t, r)["links"].([]any); len(links) != 0 {
		t.Fatalf("unpaged ingest links = %v, want none", liPaging(t, r)["links"])
	}

	// ===== a malformed start cursor is a 400 =====
	if r := f.liIngest(tokenA, map[string]string{"q": "author", "count": "2", "start": "zzz"}); r.Status != 400 ||
		r.Body["message"] != "Invalid start parameter." {
		t.Fatalf("bad start -> %d %v, want 400 Invalid start parameter.", r.Status, r.Body)
	}
}

// TestLinkedinMemberCreatorPostAnalytics: the analytics endpoint verifies the
// ugcPost entity, totals per queryType off the post seq (REACTION +3,
// COMMENT +5, RESHARE +7, IMPRESSION +11) split across two daily buckets,
// accepts the parenthesized and bare entity forms, and pages to empty past
// the data.
func TestLinkedinMemberCreatorPostAnalytics(t *testing.T) {
	f := newLinkedinFixture(t, time.Unix(1_750_000_000, 0).UTC())
	token, sub := f.liMint()
	post, _ := f.liPublish(token, "urn:li:person:"+sub, "the measured post").Body["id"].(string)
	if post == "" {
		t.Fatal("setup publish failed")
	}
	base, err := strconv.ParseInt(strings.TrimPrefix(post, "urn:li:ugcPost:"), 10, 64)
	if err != nil {
		t.Fatalf("post urn %q carries no numeric seq: %v", post, err)
	}
	analytics := func(entity, queryType, start string) starlark.Response {
		query := map[string]string{
			"q": "entity", "entity": entity, "queryType": queryType,
			"timeGranularity": "DAY", "dateRange": "(start:20250601,end:20250630)",
		}
		if start != "" {
			query["start"] = start
		}
		return f.call("analytics", "on_analytics", "GET", "/rest/memberCreatorPostAnalytics",
			nil, query, nil, "Bearer "+token)
	}
	bucketTotal := func(r starlark.Response) int64 {
		f.t.Helper()
		total := int64(0)
		for _, item := range liElements(t, r) {
			n, ok := liInt(item.(map[string]any)["count"])
			if !ok {
				f.t.Fatalf("bucket count = %v, want a number", item.(map[string]any)["count"])
			}
			total += n
		}
		return total
	}

	// ===== an unknown entity is a 404 =====
	if r := analytics("(ugcPost:urn:li:ugcPost:999999)", "REACTION", ""); r.Status != 404 ||
		r.Body["message"] != "entity not found" {
		t.Fatalf("unknown entity -> %d %v, want 404 entity not found", r.Status, r.Body)
	}

	// ===== each queryType totals base+3/5/7/11 split across two daily buckets =====
	for queryType, delta := range map[string]int64{"REACTION": 3, "COMMENT": 5, "RESHARE": 7, "IMPRESSION": 11} {
		r := analytics("(ugcPost:"+post+")", queryType, "")
		if r.Status != 200 {
			t.Fatalf("%s -> %d: %v", queryType, r.Status, r.Body)
		}
		elements := liElements(t, r)
		if len(elements) != 2 {
			t.Fatalf("%s returned %d buckets, want 2 daily buckets", queryType, len(elements))
		}
		for i, item := range elements {
			b := item.(map[string]any)
			metric := b["metricType"].(map[string]any)[liMetricTypeKey]
			if metric != queryType {
				t.Fatalf("%s bucket[%d].metricType = %v, want %q under the long key", queryType, i, metric, queryType)
			}
			target := b["targetEntity"].(map[string]any)
			if target["ugcPost"] != post {
				t.Fatalf("%s bucket[%d].targetEntity = %v, want the post %s", queryType, i, target, post)
			}
			// Synthetic daily ranges: consecutive June-2026 days.
			dr := b["dateRange"].(map[string]any)
			if day, ok := liInt(dr["start"].(map[string]any)["day"]); !ok || day != int64(1+i) {
				t.Fatalf("%s bucket[%d].dateRange.start.day = %v, want %d", queryType, i, dr["start"], 1+i)
			}
		}
		if total := bucketTotal(r); total != base+delta {
			t.Fatalf("%s buckets total %d, want base %d + %d", queryType, total, base, delta)
		}
		paging := liPaging(t, r)
		if !liNum(paging["count"], 2) || !liNum(paging["start"], 0) {
			t.Fatalf("%s paging = %v, want count 2 start 0", queryType, paging)
		}
	}

	// ===== entity accepts both the parenthesized and bare urn forms =====
	paren := analytics("(ugcPost:"+post+")", "REACTION", "").Body["elements"]
	bare := analytics(post, "REACTION", "").Body["elements"]
	if !reflect.DeepEqual(paren, bare) {
		t.Fatalf("parenthesized vs bare entity disagree: %v vs %v", paren, bare)
	}

	// ===== start past the data returns an empty page =====
	r := analytics("(ugcPost:"+post+")", "REACTION", "1")
	if got := len(liElements(t, r)); got != 0 {
		t.Fatalf("start=1 -> %d elements, want an empty page", got)
	}
	if paging := liPaging(t, r); !liNum(paging["count"], 0) || !liNum(paging["start"], 1) {
		t.Fatalf("empty page paging = %v, want count 0 start 1", paging)
	}

	// ===== an unknown queryType falls back to the base total (deviation, as-is) =====
	// Real LinkedIn rejects an invalid queryType; the adapter totals base.
	r = analytics("(ugcPost:"+post+")", "BOGUS", "")
	if r.Status != 200 {
		t.Fatalf("queryType=BOGUS -> %d %v, want 200 (lenient fallback)", r.Status, r.Body)
	}
	if total := bucketTotal(r); total != base {
		t.Fatalf("queryType=BOGUS total = %d, want the bare base %d", total, base)
	}
}
