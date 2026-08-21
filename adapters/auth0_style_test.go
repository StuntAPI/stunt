package adapters

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// These tests drive the auth0-style adapter scripts directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock, covering the
// time-dependent behaviors an HTTP-level test cannot reach without waiting:
// access-token expiry (1 hour), refresh-token expiry (30 days), and the
// time-independent contract points of the code flow, the Management API v2
// surface, and authorize validation.

const (
	auth0Host         = "auth0-style.test"
	auth0ClientID     = "mock-web-app-client"
	auth0ClientSecret = "mock-web-app-secret"
	auth0RedirectURI  = "http://localhost:3000/callback"
)

// auth0Fixture is one shared store + virtual clock with a loaded VM per
// handler script (oidc.star and management.star each need their own VM, but
// they observe the same collections/kv/blob state, like the engine).
type auth0Fixture struct {
	t      *testing.T
	vc     *clock.Clock
	vmOIDC *starlark.VM
	vmMgmt *starlark.VM
	store  *primitives.Store
}

func newAuth0Fixture(t *testing.T, start time.Time) *auth0Fixture {
	t.Helper()
	dir := repoAdaptersDir(t)
	root := filepath.Join(dir, "auth0-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}

	tmp := t.TempDir()
	store, err := primitives.Open(filepath.Join(tmp, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	kvStore, err := kv.Open(filepath.Join(tmp, "a.kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kvStore.Close() })
	blobStore, err := blob.Open(filepath.Join(tmp, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store:       store,
		KV:          kvStore,
		Blob:        blobStore,
		Clock:       vc,
		ServiceName: "test",
	})

	// Seed exactly what the engine boots from adapter.yaml (Collection.Seed
	// is a no-op on a non-empty collection).
	for _, name := range []string{"users", "roles", "clients"} {
		col, err := store.Collection(name)
		if err != nil {
			t.Fatalf("collection %s: %v", name, err)
		}
		if err := col.Seed(filepath.Join(root, "fixtures", name+".jsonl")); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

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

	return &auth0Fixture{
		t:      t,
		vc:     vc,
		vmOIDC: load("oidc.star"),
		vmMgmt: load("management.star"),
		store:  store,
	}
}

// oidcCall invokes an authentication-domain handler.
func (f *auth0Fixture) oidcCall(handler, method, path string, query, body, headers map[string]any) starlark.Response {
	f.t.Helper()
	resp, err := f.vmOIDC.Call(handler, starlark.Request{
		Method:  method,
		Path:    path,
		Host:    auth0Host,
		Headers: anyMapToStringMap(f.t, headers),
		Query:   anyMapToStringMap(f.t, query),
		Body:    body,
	})
	if err != nil {
		f.t.Fatalf("oidc %s: %v", handler, err)
	}
	return resp
}

// mgmtCall invokes a Management API handler. The body travels in raw_body
// only (req.body stays empty), exercising the handlers' raw-body-first
// decoding, and params carries the {id} route capture the engine normally
// extracts.
func (f *auth0Fixture) mgmtCall(handler, method, path, id, bearer string, body map[string]any, query map[string]string) starlark.Response {
	f.t.Helper()
	raw := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		raw = string(b)
	}
	headers := map[string]string{}
	if bearer != "" {
		headers["Authorization"] = "Bearer " + bearer
	}
	params := map[string]string{}
	if id != "" {
		params["id"] = id
	}
	resp, err := f.vmMgmt.Call(handler, starlark.Request{
		Method:  method,
		Path:    path,
		Host:    auth0Host,
		Headers: headers,
		Query:   query,
		Params:  params,
		RawBody: raw,
	})
	if err != nil {
		f.t.Fatalf("mgmt %s: %v", handler, err)
	}
	return resp
}

// anyMapToStringMap narrows a map[string]any of strings to map[string]string.
func anyMapToStringMap(t *testing.T, m map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("header/query %q is %T, want string", k, v)
		}
		out[k] = s
	}
	return out
}

// codeFlow runs authorize → token for loginHint ("" binds the default
// seeded user) and returns the token endpoint body.
func (f *auth0Fixture) codeFlow(loginHint string) map[string]any {
	f.t.Helper()
	authQuery := map[string]any{
		"client_id":     auth0ClientID,
		"redirect_uri":  auth0RedirectURI,
		"response_type": "code",
		"state":         "s0",
	}
	if loginHint != "" {
		authQuery["login_hint"] = loginHint
	}
	authResp := f.oidcCall("on_authorize", "GET", "/authorize", authQuery, nil, nil)
	if authResp.Status != 302 {
		f.t.Fatalf("authorize -> %d: %v", authResp.Status, authResp.Body)
	}
	location := authResp.Headers["Location"]
	code := locationQueryValue(location, "code")
	if code == "" {
		f.t.Fatalf("authorize Location %q has no code", location)
	}
	tokResp := f.oidcCall("on_token", "POST", "/oauth/token", nil, map[string]any{
		"grant_type":    "authorization_code",
		"code":          code,
		"client_id":     auth0ClientID,
		"client_secret": auth0ClientSecret,
		"redirect_uri":  auth0RedirectURI,
	}, nil)
	if tokResp.Status != 200 {
		f.t.Fatalf("token -> %d: %v", tokResp.Status, tokResp.Body)
	}
	return tokResp.Body
}

// mgmtToken mints a machine-to-machine access token via the
// client_credentials grant (HTTP Basic client auth).
func (f *auth0Fixture) mgmtToken() string {
	f.t.Helper()
	basic := base64.StdEncoding.EncodeToString([]byte(auth0ClientID + ":" + auth0ClientSecret))
	resp := f.oidcCall("on_token", "POST", "/oauth/token", nil, map[string]any{
		"grant_type": "client_credentials",
		"audience":   "https://" + auth0Host + "/api/v2/",
	}, map[string]any{"Authorization": "Basic " + basic})
	if resp.Status != 200 {
		f.t.Fatalf("client_credentials -> %d: %v", resp.Status, resp.Body)
	}
	return resp.Body["access_token"].(string)
}

// usersQ runs GET /api/v2/users?q=... through the list handler.
func (f *auth0Fixture) usersQ(t *testing.T, token, q string) []any {
	t.Helper()
	resp := f.mgmtCall("on_users_list", "GET", "/api/v2/users", "", token, nil, map[string]string{"q": q})
	if resp.Status != 200 {
		t.Fatalf("users list q=%q -> %d: %v", q, resp.Status, resp.Body)
	}
	return resp.BodyList
}

// locationQueryValue extracts a query param from a Location header value.
func locationQueryValue(location, key string) string {
	if i := strings.Index(location, "?"); i >= 0 {
		location = location[i+1:]
	}
	for _, pair := range strings.Split(location, "&") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 && parts[0] == key {
			return parts[1]
		}
	}
	return ""
}

// TestAuth0CodeFlowEndToEnd: discovery + JWKS are self-consistent, the
// authorization-code flow issues a real RS256 access/id pair bound to the
// login_hint user, userinfo resolves it, and the code is single-use.
func TestAuth0CodeFlowEndToEnd(t *testing.T) {
	f := newAuth0Fixture(t, time.Unix(1_750_000_000, 0).UTC())

	disc := f.oidcCall("on_discovery", "GET", "/.well-known/openid-configuration", nil, nil, nil)
	if disc.Status != 200 {
		t.Fatalf("discovery -> %d: %v", disc.Status, disc.Body)
	}
	if got := disc.Body["issuer"]; got != "https://"+auth0Host+"/" {
		t.Fatalf("issuer = %v, want https://%s/ (derived from Host)", got, auth0Host)
	}
	if got := disc.Body["jwks_uri"]; got != "https://"+auth0Host+"/.well-known/jwks.json" {
		t.Fatalf("jwks_uri = %v", got)
	}

	jwks := f.oidcCall("on_jwks", "GET", "/.well-known/jwks.json", nil, nil, nil)
	if jwks.Status != 200 {
		t.Fatalf("jwks -> %d: %v", jwks.Status, jwks.Body)
	}
	keys := jwks.Body["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("jwks has %d keys, want 1", len(keys))
	}
	key := keys[0].(map[string]any)
	if key["kid"] != "mock-auth0-key-1" || key["kty"] != "RSA" || key["alg"] != "RS256" {
		t.Fatalf("jwks key = %v", key)
	}
	if key["n"] == "" || key["e"] == "" {
		t.Fatalf("jwks key missing modulus/exponent: %v", key)
	}

	auth := f.oidcCall("on_authorize", "GET", "/authorize", map[string]any{
		"client_id":     auth0ClientID,
		"redirect_uri":  auth0RedirectURI,
		"response_type": "code",
		"state":         "xyz",
	}, nil, nil)
	if auth.Status != 302 {
		t.Fatalf("authorize -> %d: %v", auth.Status, auth.Body)
	}
	location := auth.Headers["Location"]
	if !strings.HasPrefix(location, auth0RedirectURI+"?") {
		t.Fatalf("authorize Location %q, want prefix %s?", location, auth0RedirectURI)
	}
	code := locationQueryValue(location, "code")
	if code == "" {
		t.Fatalf("authorize Location %q carries no code", location)
	}
	if got := locationQueryValue(location, "state"); got != "xyz" {
		t.Fatalf("state = %q, want xyz (echoed verbatim)", got)
	}

	tokResp := f.oidcCall("on_token", "POST", "/oauth/token", nil, map[string]any{
		"grant_type":    "authorization_code",
		"code":          code,
		"client_id":     auth0ClientID,
		"client_secret": auth0ClientSecret,
		"redirect_uri":  auth0RedirectURI,
	}, nil)
	if tokResp.Status != 200 {
		t.Fatalf("token -> %d: %v", tokResp.Status, tokResp.Body)
	}
	access, _ := tokResp.Body["access_token"].(string)
	idToken, _ := tokResp.Body["id_token"].(string)
	if access == "" || strings.Count(access, ".") != 2 {
		t.Fatalf("access_token %q is not a JWS compact token", access)
	}
	if idToken == "" {
		t.Fatalf("code grant returned no id_token: %v", tokResp.Body)
	}
	if tokResp.Body["token_type"] != "Bearer" || tokResp.Body["expires_in"] != int64(3600) {
		t.Fatalf("token envelope = %v", tokResp.Body)
	}
	if _, has := tokResp.Body["refresh_token"]; !has {
		t.Fatalf("code grant returned no refresh_token: %v", tokResp.Body)
	}

	// The default authorize subject is the first seeded user.
	ui := f.oidcCall("on_userinfo", "GET", "/userinfo", nil, nil,
		map[string]any{"Authorization": "Bearer " + access})
	if ui.Status != 200 {
		t.Fatalf("userinfo -> %d: %v", ui.Status, ui.Body)
	}
	if ui.Body["sub"] != "auth0|a1b2c3" {
		t.Fatalf("userinfo sub = %v, want auth0|a1b2c3 (default seeded user)", ui.Body["sub"])
	}
	if ui.Body["email"] != "ada@example.test" {
		t.Fatalf("userinfo email = %v (composed from seed parts)", ui.Body["email"])
	}
	if ui.Body["email_verified"] != true {
		t.Fatalf("userinfo email_verified = %v", ui.Body["email_verified"])
	}

	// login_hint selects a different user.
	other := f.codeFlow("grace@example.test")
	ui2 := f.oidcCall("on_userinfo", "GET", "/userinfo", nil, nil,
		map[string]any{"Authorization": "Bearer " + other["access_token"].(string)})
	if ui2.Status != 200 || ui2.Body["sub"] != "auth0|d4e5f6" {
		t.Fatalf("login_hint flow userinfo = %d %v", ui2.Status, ui2.Body)
	}

	// The authorization code is single-use.
	replay := f.oidcCall("on_token", "POST", "/oauth/token", nil, map[string]any{
		"grant_type":    "authorization_code",
		"code":          code,
		"client_id":     auth0ClientID,
		"client_secret": auth0ClientSecret,
		"redirect_uri":  auth0RedirectURI,
	}, nil)
	if replay.Status != 400 || replay.Body["error"] != "invalid_grant" {
		t.Fatalf("code replay -> %d %v, want 400 invalid_grant", replay.Status, replay.Body)
	}
}

// TestAuth0AuthorizeValidation: unknown client, unregistered redirect_uri,
// unsupported response_type, and a login_hint for a nonexistent user are
// answered with OAuth2 error redirects; a missing redirect_uri is a 400.
func TestAuth0AuthorizeValidation(t *testing.T) {
	f := newAuth0Fixture(t, time.Unix(1_750_000_000, 0).UTC())

	authorize := func(query map[string]any) starlark.Response {
		return f.oidcCall("on_authorize", "GET", "/authorize", query, nil, nil)
	}

	badRedirect := authorize(map[string]any{
		"client_id":     auth0ClientID,
		"redirect_uri":  "https://evil.example.test/cb",
		"response_type": "code",
		"state":         "s1",
	})
	if badRedirect.Status != 302 {
		t.Fatalf("unregistered redirect_uri -> %d, want 302 error redirect", badRedirect.Status)
	}
	loc := badRedirect.Headers["Location"]
	if locationQueryValue(loc, "error") != "invalid_request" || locationQueryValue(loc, "code") != "" {
		t.Fatalf("unregistered redirect_uri Location = %q, want error=invalid_request and no code", loc)
	}
	if got := locationQueryValue(loc, "state"); got != "s1" {
		t.Fatalf("error redirect state = %q, want s1", got)
	}

	unknownClient := authorize(map[string]any{
		"client_id":     "no-such-client",
		"redirect_uri":  auth0RedirectURI,
		"response_type": "code",
	})
	if unknownClient.Status != 302 || locationQueryValue(unknownClient.Headers["Location"], "error") == "" {
		t.Fatalf("unknown client -> %d %q", unknownClient.Status, unknownClient.Headers["Location"])
	}

	badType := authorize(map[string]any{
		"client_id":     auth0ClientID,
		"redirect_uri":  auth0RedirectURI,
		"response_type": "token",
	})
	if got := locationQueryValue(badType.Headers["Location"], "error"); got != "unsupported_response_type" {
		t.Fatalf("response_type=token error = %q, want unsupported_response_type", got)
	}

	unknownUser := authorize(map[string]any{
		"client_id":     auth0ClientID,
		"redirect_uri":  auth0RedirectURI,
		"response_type": "code",
		"login_hint":    "nobody@example.test",
	})
	if got := locationQueryValue(unknownUser.Headers["Location"], "error"); got != "access_denied" {
		t.Fatalf("unknown login_hint error = %q, want access_denied", got)
	}

	missing := authorize(map[string]any{"client_id": auth0ClientID})
	if missing.Status != 400 || missing.Body["error"] != "invalid_request" {
		t.Fatalf("missing redirect_uri -> %d %v, want 400 invalid_request", missing.Status, missing.Body)
	}
}

// TestAuth0RefreshGrantAndRevoke: refresh tokens are reusable (rotation off
// — Auth0's default), the grant returns a fresh access/id pair only, and
// /oauth/revoke is idempotent and actually kills the token.
func TestAuth0RefreshGrantAndRevoke(t *testing.T) {
	f := newAuth0Fixture(t, time.Unix(1_750_000_000, 0).UTC())

	issued := f.codeFlow("")
	refresh, _ := issued["refresh_token"].(string)
	if refresh == "" {
		t.Fatal("code flow returned no refresh_token")
	}

	grant := func() starlark.Response {
		return f.oidcCall("on_token", "POST", "/oauth/token", nil, map[string]any{
			"grant_type":    "refresh_token",
			"refresh_token": refresh,
			"client_id":     auth0ClientID,
			"client_secret": auth0ClientSecret,
		}, nil)
	}

	first := grant()
	if first.Status != 200 {
		t.Fatalf("refresh grant -> %d: %v", first.Status, first.Body)
	}
	if _, rotated := first.Body["refresh_token"]; rotated {
		t.Fatalf("refresh grant returned a refresh_token (%v) — rotation must be off", first.Body["refresh_token"])
	}
	if first.Body["id_token"] == "" {
		t.Fatal("refresh grant returned no id_token")
	}
	if first.Body["access_token"] == issued["access_token"] {
		t.Fatal("access token did not rotate between grants")
	}

	second := grant()
	if second.Status != 200 {
		t.Fatalf("second refresh grant with the SAME token -> %d: %v (refresh tokens must be reusable)", second.Status, second.Body)
	}

	revoke := func() starlark.Response {
		return f.oidcCall("on_revoke", "POST", "/oauth/revoke", nil, map[string]any{
			"token":         refresh,
			"client_id":     auth0ClientID,
			"client_secret": auth0ClientSecret,
		}, nil)
	}
	if r := revoke(); r.Status != 200 {
		t.Fatalf("revoke -> %d: %v", r.Status, r.Body)
	}
	if r := revoke(); r.Status != 200 {
		t.Fatalf("revoke replay -> %d, want 200 (idempotent)", r.Status)
	}

	if after := grant(); after.Status != 400 || after.Body["error"] != "invalid_grant" {
		t.Fatalf("grant after revoke -> %d %v, want 400 invalid_grant", after.Status, after.Body)
	}
}

// TestAuth0TokenExpiryWithVirtualClock: the access token dies at exp (1
// hour) while the refresh token survives it (30 days) and dies at its own
// expiry — all via the virtual clock, no real waiting.
func TestAuth0TokenExpiryWithVirtualClock(t *testing.T) {
	f := newAuth0Fixture(t, time.Unix(1_750_000_000, 0).UTC())

	issued := f.codeFlow("")
	access := issued["access_token"].(string)
	refresh := issued["refresh_token"].(string)
	bearer := map[string]any{"Authorization": "Bearer " + access}

	if ui := f.oidcCall("on_userinfo", "GET", "/userinfo", nil, nil, bearer); ui.Status != 200 {
		t.Fatalf("userinfo before expiry -> %d: %v", ui.Status, ui.Body)
	}

	f.vc.Advance(2 * time.Hour)

	ui := f.oidcCall("on_userinfo", "GET", "/userinfo", nil, nil, bearer)
	if ui.Status != 401 || ui.Body["error"] != "invalid_token" {
		t.Fatalf("userinfo after expiry -> %d %v, want 401 invalid_token", ui.Status, ui.Body)
	}
	mgmt := f.mgmtCall("on_users_list", "GET", "/api/v2/users", "", access, nil, nil)
	if mgmt.Status != 401 || mgmt.Body["statusCode"] != int64(401) {
		t.Fatalf("management after expiry -> %d %v, want 401 envelope", mgmt.Status, mgmt.Body)
	}
	if mgmt.Body["message"] != "Token expired" {
		t.Fatalf("expired-token message = %v, want \"Token expired\"", mgmt.Body["message"])
	}

	// The refresh grant still works after the access token expired.
	refreshed := f.oidcCall("on_token", "POST", "/oauth/token", nil, map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": refresh,
		"client_id":     auth0ClientID,
		"client_secret": auth0ClientSecret,
	}, nil)
	if refreshed.Status != 200 {
		t.Fatalf("refresh grant after access expiry -> %d: %v", refreshed.Status, refreshed.Body)
	}
	ui2 := f.oidcCall("on_userinfo", "GET", "/userinfo", nil, nil,
		map[string]any{"Authorization": "Bearer " + refreshed.Body["access_token"].(string)})
	if ui2.Status != 200 {
		t.Fatalf("userinfo with refreshed token -> %d: %v", ui2.Status, ui2.Body)
	}

	f.vc.Advance(31 * 24 * time.Hour)

	expired := f.oidcCall("on_token", "POST", "/oauth/token", nil, map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": refresh,
		"client_id":     auth0ClientID,
		"client_secret": auth0ClientSecret,
	}, nil)
	if expired.Status != 400 || expired.Body["error"] != "invalid_grant" {
		t.Fatalf("refresh grant after refresh expiry -> %d %v, want 400 invalid_grant", expired.Status, expired.Body)
	}
}

// TestAuth0ClientCredentialsGrant: the M2M grant needs valid client
// credentials (body or Basic), returns an access token with no id_token or
// refresh_token, and the token authorizes Management API calls — but cannot
// call userinfo (its subject is a client, not a user).
func TestAuth0ClientCredentialsGrant(t *testing.T) {
	f := newAuth0Fixture(t, time.Unix(1_750_000_000, 0).UTC())

	cc := f.oidcCall("on_token", "POST", "/oauth/token", nil, map[string]any{
		"grant_type":    "client_credentials",
		"client_id":     auth0ClientID,
		"client_secret": auth0ClientSecret,
	}, nil)
	if cc.Status != 200 {
		t.Fatalf("client_credentials -> %d: %v", cc.Status, cc.Body)
	}
	if _, has := cc.Body["id_token"]; has {
		t.Fatal("client_credentials returned an id_token")
	}
	if _, has := cc.Body["refresh_token"]; has {
		t.Fatal("client_credentials returned a refresh_token")
	}
	access := cc.Body["access_token"].(string)
	if strings.Count(access, ".") != 2 {
		t.Fatalf("client_credentials access_token %q is not a JWS", access)
	}

	list := f.mgmtCall("on_users_list", "GET", "/api/v2/users", "", access, nil, nil)
	if list.Status != 200 {
		t.Fatalf("users list with M2M token -> %d: %v", list.Status, list.Body)
	}

	ui := f.oidcCall("on_userinfo", "GET", "/userinfo", nil, nil,
		map[string]any{"Authorization": "Bearer " + access})
	if ui.Status != 401 || ui.Body["error"] != "invalid_token" {
		t.Fatalf("userinfo with M2M token -> %d %v, want 401 invalid_token", ui.Status, ui.Body)
	}

	badSecret := f.oidcCall("on_token", "POST", "/oauth/token", nil, map[string]any{
		"grant_type":    "client_credentials",
		"client_id":     auth0ClientID,
		"client_secret": "wrong-secret",
	}, nil)
	if badSecret.Status != 401 || badSecret.Body["error"] != "invalid_client" {
		t.Fatalf("wrong secret -> %d %v, want 401 invalid_client", badSecret.Status, badSecret.Body)
	}
}

// TestAuth0ManagementUsersCRUD: create/retrieve/patch/delete users, the q
// search forms, page/per_page paging with include_totals, validation and
// 404 envelopes, and that the password never leaks into a response.
func TestAuth0ManagementUsersCRUD(t *testing.T) {
	f := newAuth0Fixture(t, time.Unix(1_750_000_000, 0).UTC())
	token := f.mgmtToken()

	list := f.mgmtCall("on_users_list", "GET", "/api/v2/users", "", token, nil, nil)
	if list.Status != 200 {
		t.Fatalf("users list -> %d: %v", list.Status, list.Body)
	}
	if got := len(list.BodyList); got != 3 {
		t.Fatalf("users list has %d entries, want the 3 seeded users", got)
	}
	firstUser := list.BodyList[0].(map[string]any)
	if _, leak := firstUser["password"]; leak {
		t.Fatal("user response leaks the password field")
	}
	if _, leak := firstUser["email_local"]; leak {
		t.Fatal("user response leaks the seed email_local field")
	}

	byID := f.usersQ(t, token, `user_id:"auth0|a1b2c3"`)
	if len(byID) != 1 || byID[0].(map[string]any)["email"] != "ada@example.test" {
		t.Fatalf("q=user_id field search = %v", byID)
	}
	term := f.usersQ(t, token, "grace")
	if len(term) != 1 || term[0].(map[string]any)["user_id"] != "auth0|d4e5f6" {
		t.Fatalf("q=grace substring search = %v", term)
	}
	if none := f.usersQ(t, token, "does-not-match"); len(none) != 0 {
		t.Fatalf("q=does-not-match = %v, want empty", none)
	}

	paged := f.mgmtCall("on_users_list", "GET", "/api/v2/users", "", token, nil,
		map[string]string{"page": "2", "per_page": "2", "include_totals": "true"})
	if paged.Status != 200 {
		t.Fatalf("paged users -> %d: %v", paged.Status, paged.Body)
	}
	if paged.Body["length"] != int64(3) || paged.Body["limit"] != int64(2) || paged.Body["start"] != int64(2) {
		t.Fatalf("include_totals envelope = %v", paged.Body)
	}
	users2, _ := paged.Body["users"].([]any)
	if len(users2) != 1 {
		t.Fatalf("page 2 of per_page=2 has %d users, want 1", len(users2))
	}
	badPage := f.mgmtCall("on_users_list", "GET", "/api/v2/users", "", token, nil,
		map[string]string{"per_page": "101"})
	if badPage.Status != 400 || badPage.Body["error"] != "Bad Request" {
		t.Fatalf("per_page=101 -> %d %v, want 400 Bad Request", badPage.Status, badPage.Body)
	}

	created := f.mgmtCall("on_user_create", "POST", "/api/v2/users", "", token, map[string]any{
		"email":         "bob@example.test",
		"password":      "BobPass123!",
		"connection":    "Username-Password-Authentication",
		"user_metadata": map[string]any{"team": "mock"},
	}, nil)
	if created.Status != 201 {
		t.Fatalf("create user -> %d: %v", created.Status, created.Body)
	}
	uid, _ := created.Body["user_id"].(string)
	if !strings.HasPrefix(uid, "auth0|") {
		t.Fatalf("created user_id = %q, want auth0|... prefix", uid)
	}
	if created.Body["email"] != "bob@example.test" {
		t.Fatalf("created email = %v", created.Body["email"])
	}

	got := f.mgmtCall("on_user_get", "GET", "/api/v2/users/"+uid, uid, token, nil, nil)
	if got.Status != 200 || got.Body["email"] != "bob@example.test" {
		t.Fatalf("get user -> %d %v", got.Status, got.Body)
	}
	if _, leak := got.Body["password"]; leak {
		t.Fatal("get user leaks password")
	}
	if meta, _ := got.Body["user_metadata"].(map[string]any); meta["team"] != "mock" {
		t.Fatalf("user_metadata = %v, want the create payload block", got.Body["user_metadata"])
	}

	patched := f.mgmtCall("on_user_patch", "PATCH", "/api/v2/users/"+uid, uid, token,
		map[string]any{"nickname": "bobby", "email_verified": true}, nil)
	if patched.Status != 200 || patched.Body["nickname"] != "bobby" {
		t.Fatalf("patch user -> %d %v", patched.Status, patched.Body)
	}
	if patched.Body["email"] != "bob@example.test" {
		t.Fatalf("patch dropped email: %v", patched.Body)
	}

	dup := f.mgmtCall("on_user_create", "POST", "/api/v2/users", "", token,
		map[string]any{"email": "BOB@example.test"}, nil)
	if dup.Status != 400 || dup.Body["message"] != "The user already exists." {
		t.Fatalf("duplicate email -> %d %v, want 400 already exists", dup.Status, dup.Body)
	}
	missing := f.mgmtCall("on_user_create", "POST", "/api/v2/users", "", token,
		map[string]any{"name": "No Email"}, nil)
	if missing.Status != 400 || missing.Body["error"] != "Bad Request" {
		t.Fatalf("create without email -> %d %v, want 400", missing.Status, missing.Body)
	}

	del := f.mgmtCall("on_user_delete", "DELETE", "/api/v2/users/"+uid, uid, token, nil, nil)
	if del.Status != 204 {
		t.Fatalf("delete user -> %d, want 204", del.Status)
	}
	gone := f.mgmtCall("on_user_get", "GET", "/api/v2/users/"+uid, uid, token, nil, nil)
	if gone.Status != 404 || gone.Body["error"] != "Not Found" || gone.Body["statusCode"] != int64(404) {
		t.Fatalf("get deleted user -> %d %v, want 404 envelope", gone.Status, gone.Body)
	}
}

// TestAuth0ManagementRoles: role list/create, per-user assignment
// (idempotent, validated), and unassignment.
func TestAuth0ManagementRoles(t *testing.T) {
	f := newAuth0Fixture(t, time.Unix(1_750_000_000, 0).UTC())
	token := f.mgmtToken()

	roles := f.mgmtCall("on_roles_list", "GET", "/api/v2/roles", "", token, nil, nil)
	if roles.Status != 200 || len(roles.BodyList) != 2 {
		t.Fatalf("roles list -> %d %v, want the 2 seeded roles", roles.Status, roles.BodyList)
	}

	created := f.mgmtCall("on_role_create", "POST", "/api/v2/roles", "", token,
		map[string]any{"name": "auditor", "description": "Mock audit access"}, nil)
	if created.Status != 200 || created.Body["name"] != "auditor" {
		t.Fatalf("create role -> %d %v", created.Status, created.Body)
	}
	roleID := created.Body["id"].(string)

	dup := f.mgmtCall("on_role_create", "POST", "/api/v2/roles", "", token,
		map[string]any{"name": "auditor"}, nil)
	if dup.Status != 409 || dup.Body["error"] != "Conflict" {
		t.Fatalf("duplicate role -> %d %v, want 409", dup.Status, dup.Body)
	}
	unnamed := f.mgmtCall("on_role_create", "POST", "/api/v2/roles", "", token,
		map[string]any{"description": "no name"}, nil)
	if unnamed.Status != 400 {
		t.Fatalf("role without name -> %d %v, want 400", unnamed.Status, unnamed.Body)
	}

	// ada is seeded with rol_mock_admin_1; assign auditor next to it.
	const ada = "auth0|a1b2c3"
	assign := f.mgmtCall("on_user_roles_assign", "POST", "/api/v2/users/"+ada+"/roles", ada, token,
		map[string]any{"roles": []any{roleID}}, nil)
	if assign.Status != 204 {
		t.Fatalf("assign role -> %d: %v", assign.Status, assign.Body)
	}

	have := f.mgmtCall("on_user_roles_list", "GET", "/api/v2/users/"+ada+"/roles", ada, token, nil, nil)
	if have.Status != 200 {
		t.Fatalf("user roles -> %d: %v", have.Status, have.Body)
	}
	names := map[string]bool{}
	for _, r := range have.BodyList {
		names[r.(map[string]any)["id"].(string)] = true
	}
	if !names["rol_mock_admin_1"] || !names[roleID] {
		t.Fatalf("assigned roles = %v, want seeded admin + auditor", names)
	}

	// Idempotent re-assign must not duplicate.
	reassign := f.mgmtCall("on_user_roles_assign", "POST", "/api/v2/users/"+ada+"/roles", ada, token,
		map[string]any{"roles": []any{roleID}}, nil)
	if reassign.Status != 204 {
		t.Fatalf("re-assign role -> %d: %v", reassign.Status, reassign.Body)
	}
	have2 := f.mgmtCall("on_user_roles_list", "GET", "/api/v2/users/"+ada+"/roles", ada, token, nil, nil)
	if len(have2.BodyList) != 2 {
		t.Fatalf("re-assign duplicated roles: %v", have2.BodyList)
	}

	unassign := f.mgmtCall("on_user_roles_delete", "DELETE", "/api/v2/users/"+ada+"/roles", ada, token,
		map[string]any{"roles": []any{roleID}}, nil)
	if unassign.Status != 204 {
		t.Fatalf("unassign role -> %d: %v", unassign.Status, unassign.Body)
	}
	have3 := f.mgmtCall("on_user_roles_list", "GET", "/api/v2/users/"+ada+"/roles", ada, token, nil, nil)
	if len(have3.BodyList) != 1 || have3.BodyList[0].(map[string]any)["id"] != "rol_mock_admin_1" {
		t.Fatalf("roles after unassign = %v, want only the seeded admin role", have3.BodyList)
	}

	unknownRole := f.mgmtCall("on_user_roles_assign", "POST", "/api/v2/users/"+ada+"/roles", ada, token,
		map[string]any{"roles": []any{"rol_missing"}}, nil)
	if unknownRole.Status != 400 || unknownRole.Body["error"] != "Bad Request" {
		t.Fatalf("assign unknown role -> %d %v, want 400", unknownRole.Status, unknownRole.Body)
	}
	unknownUser := f.mgmtCall("on_user_roles_assign", "POST", "/api/v2/users/auth0|nope/roles", "auth0|nope", token,
		map[string]any{"roles": []any{roleID}}, nil)
	if unknownUser.Status != 404 || unknownUser.Body["message"] != "The user does not exist." {
		t.Fatalf("assign to unknown user -> %d %v, want 404", unknownUser.Status, unknownUser.Body)
	}
}

// TestAuth0DbConnectionsSignup: signup creates an unverified user visible
// to the Management API; duplicates, weak passwords, and unknown clients
// are rejected.
func TestAuth0DbConnectionsSignup(t *testing.T) {
	f := newAuth0Fixture(t, time.Unix(1_750_000_000, 0).UTC())

	signup := f.oidcCall("on_signup", "POST", "/dbconnections/signup", nil, map[string]any{
		"client_id": auth0ClientID,
		"email":     "pat@example.test",
		"password":  "PatPass123!",
	}, nil)
	if signup.Status != 200 {
		t.Fatalf("signup -> %d: %v", signup.Status, signup.Body)
	}
	if signup.Body["email"] != "pat@example.test" || signup.Body["email_verified"] != false {
		t.Fatalf("signup response = %v, want unverified user + email", signup.Body)
	}
	if _, has := signup.Body["_id"]; !has {
		t.Fatalf("signup response missing _id: %v", signup.Body)
	}

	token := f.mgmtToken()
	found := f.usersQ(t, token, `email:"pat@example.test"`)
	if len(found) != 1 {
		t.Fatalf("q=email field search for signup user = %v", found)
	}

	dup := f.oidcCall("on_signup", "POST", "/dbconnections/signup", nil, map[string]any{
		"client_id": auth0ClientID,
		"email":     "pat@example.test",
		"password":  "PatPass123!",
	}, nil)
	if dup.Status != 400 || dup.Body["error"] != "user_exists" {
		t.Fatalf("duplicate signup -> %d %v, want 400 user_exists", dup.Status, dup.Body)
	}
	weak := f.oidcCall("on_signup", "POST", "/dbconnections/signup", nil, map[string]any{
		"client_id": auth0ClientID,
		"email":     "sam@example.test",
		"password":  "short",
	}, nil)
	if weak.Status != 400 || weak.Body["error"] != "invalid_password" {
		t.Fatalf("weak password signup -> %d %v, want 400 invalid_password", weak.Status, weak.Body)
	}
	badClient := f.oidcCall("on_signup", "POST", "/dbconnections/signup", nil, map[string]any{
		"client_id": "no-such-client",
		"email":     "sam@example.test",
		"password":  "SamPass123!",
	}, nil)
	if badClient.Status != 403 {
		t.Fatalf("unknown client signup -> %d %v, want 403", badClient.Status, badClient.Body)
	}
}
