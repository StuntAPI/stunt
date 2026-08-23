package adapters

import (
	"fmt"
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

// These tests drive the photos-style adapter scripts directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock: the OAuth2
// authorization-code + refresh grants and the Bearer gate (with token
// expiry), the two-step uploadToken pipeline (uploads -> batchCreate ->
// byte-exact =d downloads), mediaItems list/search pagination and filters
// (mediaTypeFilter, dateFilter over the seeded creationTime), album CRUD
// with per-user scoping, and Google's {error:{code,message,status}}
// envelope. Upload tokens are single-use and authorization codes are burned
// only on a successful exchange, matching the real API.

const (
	phRedirectURI   = "http://localhost:8080/callback"
	phClientID      = "ph-test-client-id"
	phClientSecret  = "ph-test-client-secret"
	phScope         = "https://www.googleapis.com/auth/photoslibrary.appendonly"
	phCreationTime  = "2024-06-15T12:00:00Z" // every seeded media item's creationTime
	phDerivativePfx = "stunt-derivative-preview:"
)

// phNewItem is one newMediaItems entry for a batchCreate call.
type phNewItem struct {
	token       string
	fileName    string
	description string
}

// phFixture is one shared store + virtual clock with a loaded VM per
// handler script (oauth, uploads, media_items and albums each get their own
// VM but observe the same collections/kv/blob state, like the engine).
type phFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newPhotosFixture(t *testing.T, start time.Time) *phFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "photos-style")
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
	return &phFixture{
		t: t, vc: vc, host: "photos.example.test",
		vms: map[string]*starlark.VM{
			"oauth": load("oauth.star"), "uploads": load("uploads.star"),
			"media": load("media_items.star"), "albums": load("albums.star"),
		},
	}
}

// callHost invokes handler on the group's VM against an explicit host
// (baseUrl is derived from it at read time). auth/contentType "" = absent.
func (f *phFixture) callHost(host, group, handler, method, path string, params, query map[string]string, body map[string]any, rawBody, auth, contentType string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	if contentType != "" {
		headers["Content-Type"] = contentType
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: host,
		Headers: headers, Body: body, RawBody: rawBody, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

func (f *phFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, rawBody, auth, contentType string) starlark.Response {
	f.t.Helper()
	return f.callHost(f.host, group, handler, method, path, params, query, body, rawBody, auth, contentType)
}

func (f *phFixture) get(group, handler, path string, params, query map[string]string, auth string) starlark.Response {
	f.t.Helper()
	return f.call(group, handler, "GET", path, params, query, nil, "", auth, "")
}

func (f *phFixture) post(group, handler, path string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	return f.call(group, handler, "POST", path, nil, nil, body, "", auth, "")
}

func (f *phFixture) del(group, handler, path string, params map[string]string, auth string) starlark.Response {
	f.t.Helper()
	return f.call(group, handler, "DELETE", path, params, nil, nil, "", auth, "")
}

// --- OAuth2 helpers ---

// phAuthorize drives GET /o/oauth2/auth; empty args are omitted entirely.
func phAuthorize(f *phFixture, redirectURI, state, clientID string) starlark.Response {
	f.t.Helper()
	query := map[string]string{"response_type": "code"}
	if redirectURI != "" {
		query["redirect_uri"] = redirectURI
	}
	if state != "" {
		query["state"] = state
	}
	if clientID != "" {
		query["client_id"] = clientID
	}
	return f.get("oauth", "on_authorize", "/o/oauth2/auth", nil, query, "")
}

// phCodeOf extracts the code query parameter from a redirect Location.
func phCodeOf(t *testing.T, location string) string {
	t.Helper()
	i := strings.Index(location, "code=")
	if i < 0 {
		t.Fatalf("no code in Location %q", location)
	}
	rest := location[i+len("code="):]
	if j := strings.Index(rest, "&"); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		t.Fatalf("empty code in Location %q", location)
	}
	return rest
}

// phFreshCode runs one authorize round-trip and returns its code.
func phFreshCode(f *phFixture) string {
	f.t.Helper()
	r := phAuthorize(f, phRedirectURI, "st", phClientID)
	if r.Status != 302 {
		f.t.Fatalf("authorize -> %d: %v", r.Status, r.Body)
	}
	return phCodeOf(f.t, r.Headers["Location"])
}

func phExchange(f *phFixture, code, clientID, clientSecret, redirectURI string) starlark.Response {
	f.t.Helper()
	return f.post("oauth", "on_token", "/o/oauth2/token", map[string]any{
		"grant_type": "authorization_code", "code": code,
		"client_id": clientID, "client_secret": clientSecret, "redirect_uri": redirectURI,
	}, "")
}

func phRefresh(f *phFixture, refresh, clientID, clientSecret string) starlark.Response {
	f.t.Helper()
	return f.post("oauth", "on_token", "/o/oauth2/token", map[string]any{
		"grant_type": "refresh_token", "refresh_token": refresh,
		"client_id": clientID, "client_secret": clientSecret,
	}, "")
}

// phLoginPair runs the full code flow and returns (access, refresh).
func phLoginPair(f *phFixture) (string, string) {
	f.t.Helper()
	r := phExchange(f, phFreshCode(f), phClientID, phClientSecret, phRedirectURI)
	if r.Status != 200 {
		f.t.Fatalf("token exchange -> %d: %v", r.Status, r.Body)
	}
	access, _ := r.Body["access_token"].(string)
	refresh, _ := r.Body["refresh_token"].(string)
	if access == "" || refresh == "" {
		f.t.Fatalf("token response = %v", r.Body)
	}
	return access, refresh
}

func phLogin(f *phFixture) string {
	f.t.Helper()
	access, _ := phLoginPair(f)
	return access
}

// --- media helpers ---

// phUpload POSTs raw bytes to /v1/uploads and returns the plain-text token.
func phUpload(f *phFixture, auth, payload, contentType string) string {
	f.t.Helper()
	r := f.call("uploads", "on_uploads", "POST", "/v1/uploads",
		nil, nil, nil, payload, auth, contentType)
	if r.Status != 200 {
		f.t.Fatalf("uploads -> %d: %v", r.Status, r.Body)
	}
	if r.RawBody == "" {
		f.t.Fatalf("uploads returned an empty uploadToken")
	}
	return r.RawBody
}

// phBatch creates the given items in one batchCreate call (one albumId for
// the whole batch, like the real API) and returns the per-item results.
func phBatch(f *phFixture, auth, albumID string, items []phNewItem) []any {
	f.t.Helper()
	entries := make([]any, 0, len(items))
	for _, it := range items {
		entry := map[string]any{
			"simpleMediaItem": map[string]any{"uploadToken": it.token, "fileName": it.fileName},
		}
		if it.description != "" {
			entry["description"] = it.description
		}
		entries = append(entries, entry)
	}
	r := f.post("media", "on_batch_create", "/v1/mediaItems:batchCreate",
		map[string]any{"albumId": albumID, "newMediaItems": entries}, auth)
	if r.Status != 200 {
		f.t.Fatalf("batchCreate -> %d: %v", r.Status, r.Body)
	}
	results, ok := r.Body["newMediaItemResults"].([]any)
	if !ok || len(results) != len(items) {
		f.t.Fatalf("newMediaItemResults = %v, want %d entries", r.Body["newMediaItemResults"], len(items))
	}
	return results
}

// phCreateOne creates a single item and returns its result entry (either a
// mediaItem or a status error).
func phCreateOne(f *phFixture, auth, uploadToken, fileName, albumID, description string) map[string]any {
	f.t.Helper()
	res := phBatch(f, auth, albumID, []phNewItem{{token: uploadToken, fileName: fileName, description: description}})
	m, ok := res[0].(map[string]any)
	if !ok {
		f.t.Fatalf("batchCreate result[0] = %v, want an object", res[0])
	}
	return m
}

// phMediaItemOf returns the mediaItem object from a batchCreate result.
func phMediaItemOf(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	mi, ok := result["mediaItem"].(map[string]any)
	if !ok {
		t.Fatalf("batchCreate result = %v, want a mediaItem", result)
	}
	return mi
}

// phIDs pulls the ids out of a mediaItems response page.
func phIDs(t *testing.T, r starlark.Response) []string {
	t.Helper()
	items, ok := r.Body["mediaItems"].([]any)
	if !ok {
		t.Fatalf("mediaItems = %v, want a list", r.Body["mediaItems"])
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		id, _ := it.(map[string]any)["id"].(string)
		if id == "" {
			t.Fatalf("media item without id: %v", it)
		}
		ids = append(ids, id)
	}
	return ids
}

// phSearch POSTs a mediaItems:search body.
func phSearch(f *phFixture, auth string, body map[string]any) starlark.Response {
	f.t.Helper()
	return f.post("media", "on_search", "/v1/mediaItems:search", body, auth)
}

// phErr returns Google's error object from an error response.
func phErr(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	e, ok := r.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response %d error = %v, want Google's error object", r.Status, r.Body)
	}
	return e
}

// phNum compares a response number whether it arrives as int (handler-built)
// or float (round-tripped through JSON storage).
func phNum(got any, want int64) bool {
	switch n := got.(type) {
	case int64:
		return n == want
	case float64:
		return n == float64(want)
	}
	return false
}

// TestPhotosOAuthFlow: the authorization-code leg — the authorize redirect
// (302 + Location with a fresh code and the caller's state), code exchange
// validation (unknown code, client mismatch that must NOT burn the code,
// single-use on success) and the minted bearer pair's exact shape.
func TestPhotosOAuthFlow(t *testing.T) {
	f := newPhotosFixture(t, time.Unix(1_750_000_000, 0).UTC())

	// ===== authorize without redirect_uri, state or client_id is 400 invalid_request =====
	r := phAuthorize(f, "", "st-1", phClientID)
	if r.Status != 400 || r.Body["error"] != "invalid_request" {
		t.Fatalf("authorize without redirect_uri -> %d %v, want 400 invalid_request", r.Status, r.Body)
	}
	if r.Body["error_description"] != "missing redirect_uri/state/client_id" {
		t.Fatalf("invalid_request description = %v", r.Body["error_description"])
	}

	// ===== authorize redirects with a fresh code and echoes the state =====
	r = phAuthorize(f, phRedirectURI, "st-123", phClientID)
	if r.Status != 302 {
		t.Fatalf("authorize -> %d, want 302", r.Status)
	}
	if loc := r.Headers["Location"]; loc != phRedirectURI+"?code=4/mock_code_1&state=st-123" {
		t.Fatalf("authorize Location = %q", loc)
	}

	// ===== a redirect_uri that already carries a query joins with & =====
	r = phAuthorize(f, phRedirectURI+"?foo=1", "s2", phClientID)
	if loc := r.Headers["Location"]; !strings.HasPrefix(loc, phRedirectURI+"?foo=1&code=4/mock_code_2&state=s2") {
		t.Fatalf("redirect_uri with query Location = %q, want & separator", loc)
	}

	// ===== exchanging an unknown code is 400 invalid_grant =====
	r = phExchange(f, "4/nope", phClientID, phClientSecret, phRedirectURI)
	if r.Status != 400 || r.Body["error"] != "invalid_grant" ||
		r.Body["error_description"] != "invalid/used code" {
		t.Fatalf("unknown code -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}

	// ===== a client mismatch answers invalid_client but leaves the code usable =====
	code := phFreshCode(f)
	r = phExchange(f, code, "wrong-client", phClientSecret, phRedirectURI)
	if r.Status != 400 || r.Body["error"] != "invalid_client" ||
		r.Body["error_description"] != "client mismatch" {
		t.Fatalf("client mismatch -> %d %v, want 400 invalid_client", r.Status, r.Body)
	}
	// The failed exchange must not have burned the code.
	r = phExchange(f, code, phClientID, phClientSecret, phRedirectURI)
	if r.Status != 200 {
		t.Fatalf("retry after client mismatch -> %d: %v (code was burned)", r.Status, r.Body)
	}
	access, _ := r.Body["access_token"].(string)

	// ===== the happy exchange mints the bearer pair =====
	if access != "ya29.mock_access_1" {
		t.Fatalf("access_token = %q, want the first minted token", access)
	}
	if r.Body["refresh_token"] != "1//mock_refresh_1" || r.Body["token_type"] != "Bearer" {
		t.Fatalf("token response = %v", r.Body)
	}
	if !phNum(r.Body["expires_in"], 3599) {
		t.Fatalf("expires_in = %v (%T), want 3599", r.Body["expires_in"], r.Body["expires_in"])
	}
	if r.Body["scope"] != phScope {
		t.Fatalf("scope = %v", r.Body["scope"])
	}

	// ===== the minted bearer reads the data plane =====
	lr := f.get("albums", "on_list_albums", "/v1/albums", nil, nil, "Bearer "+access)
	if lr.Status != 200 || len(lr.Body["albums"].([]any)) != 0 {
		t.Fatalf("list albums with fresh bearer -> %d %v", lr.Status, lr.Body)
	}

	// ===== authorization codes are single-use =====
	if r := phExchange(f, code, phClientID, phClientSecret, phRedirectURI); r.Status != 400 ||
		r.Body["error"] != "invalid_grant" {
		t.Fatalf("second exchange -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}

	// ===== unknown grant types are unsupported_grant_type =====
	r = f.post("oauth", "on_token", "/o/oauth2/token", map[string]any{"grant_type": "password", "client_id": phClientID}, "")
	if r.Status != 400 || r.Body["error"] != "unsupported_grant_type" {
		t.Fatalf("grant_type=password -> %d %v, want 400 unsupported_grant_type", r.Status, r.Body)
	}
}

// TestPhotosRefreshGrant: the refresh leg — missing client creds and unknown
// refresh tokens keep the OAuth error shapes, a refresh mints a NEW access
// token for the SAME user while preserving the refresh token (Google does
// not rotate), and refresh-issued access tokens expire like any other.
func TestPhotosRefreshGrant(t *testing.T) {
	f := newPhotosFixture(t, time.Unix(1_750_000_000, 0).UTC())
	access1, refresh := phLoginPair(f)
	ut := phUpload(f, "Bearer "+access1, "bytes-one", "image/jpeg")
	phCreateOne(f, "Bearer "+access1, ut, "one.jpg", "", "")

	// ===== refresh without client credentials is 400 invalid_client =====
	r := phRefresh(f, refresh, "", "")
	if r.Status != 400 || r.Body["error"] != "invalid_client" ||
		r.Body["error_description"] != "missing client creds" {
		t.Fatalf("refresh without creds -> %d %v, want 400 invalid_client", r.Status, r.Body)
	}

	// ===== refresh with an unknown refresh token is 400 invalid_grant =====
	r = phRefresh(f, "1//mock_refresh_9x", phClientID, phClientSecret)
	if r.Status != 400 || r.Body["error"] != "invalid_grant" ||
		r.Body["error_description"] != "invalid refresh token" {
		t.Fatalf("unknown refresh token -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}

	// ===== refresh mints a new access token but keeps the refresh token =====
	r = phRefresh(f, refresh, phClientID, phClientSecret)
	if r.Status != 200 {
		t.Fatalf("refresh -> %d: %v", r.Status, r.Body)
	}
	access2, _ := r.Body["access_token"].(string)
	if access2 == "" || access2 == access1 {
		t.Fatalf("refreshed access_token = %q, want a fresh one", access2)
	}
	if r.Body["refresh_token"] != refresh || r.Body["token_type"] != "Bearer" ||
		r.Body["scope"] != phScope || !phNum(r.Body["expires_in"], 3599) {
		t.Fatalf("refresh response = %v", r.Body)
	}

	// ===== the refreshed token serves the same user's library =====
	lr := f.get("media", "on_list", "/v1/mediaItems", nil, nil, "Bearer "+access2)
	if lr.Status != 200 {
		t.Fatalf("list with refreshed token -> %d: %v", lr.Status, lr.Body)
	}
	if ids := phIDs(t, lr); len(ids) != 1 || ids[0] != "mock-media-1" {
		t.Fatalf("refreshed token sees %v, want the item created under the old token", ids)
	}

	// ===== refresh-issued access tokens expire like any other =====
	f.vc.Advance(2 * time.Hour)
	if r := f.get("media", "on_list", "/v1/mediaItems", nil, nil, "Bearer "+access1); r.Status != 401 {
		t.Fatalf("original token after expiry -> %d, want 401", r.Status)
	}
	if r := f.get("media", "on_list", "/v1/mediaItems", nil, nil, "Bearer "+access2); r.Status != 401 {
		t.Fatalf("refreshed token after expiry -> %d, want 401 (expires_in is enforced)", r.Status)
	}
	// The refresh token itself survives and re-authenticates after expiry.
	r = phRefresh(f, refresh, phClientID, phClientSecret)
	if r.Status != 200 {
		t.Fatalf("refresh after access-token expiry -> %d: %v", r.Status, r.Body)
	}
	access3, _ := r.Body["access_token"].(string)
	if r := f.get("media", "on_list", "/v1/mediaItems", nil, nil, "Bearer "+access3); r.Status != 200 {
		t.Fatalf("third-generation token -> %d: %v", r.Status, r.Body)
	}
}

// TestPhotosBearerGate: every data surface demands a well-formed Bearer
// token bound to a live token document — missing headers, other schemes,
// case-mangled schemes, empty and unknown tokens all answer Google's 401
// envelope, and the token dies when its expires_at passes on the virtual
// clock.
func TestPhotosBearerGate(t *testing.T) {
	f := newPhotosFixture(t, time.Unix(1_750_000_000, 0).UTC())
	tok := phLogin(f)

	surfaces := []struct {
		group, handler, method, path string
		params                       map[string]string
	}{
		{"albums", "on_list_albums", "GET", "/v1/albums", nil},
		{"albums", "on_create_album", "POST", "/v1/albums", nil},
		{"albums", "on_get_album", "GET", "/v1/albums/mock-album-1", map[string]string{"id": "mock-album-1"}},
		{"albums", "on_delete_album", "DELETE", "/v1/albums/mock-album-1", map[string]string{"id": "mock-album-1"}},
		{"media", "on_list", "GET", "/v1/mediaItems", nil},
		{"media", "on_get", "GET", "/v1/mediaItems/mock-media-1", map[string]string{"id": "mock-media-1"}},
		{"media", "on_search", "POST", "/v1/mediaItems:search", nil},
		{"media", "on_batch_create", "POST", "/v1/mediaItems:batchCreate", nil},
		{"uploads", "on_uploads", "POST", "/v1/uploads", nil},
	}

	// ===== every data surface answers 401 UNAUTHENTICATED without a bearer =====
	for _, s := range surfaces {
		r := f.call(s.group, s.handler, s.method, s.path, s.params, nil, nil, "", "", "")
		if r.Status != 401 {
			t.Fatalf("%s without bearer -> %d, want 401", s.handler, r.Status)
		}
		e := phErr(t, r)
		if !phNum(e["code"], 401) || e["status"] != "UNAUTHENTICATED" || e["message"] != "Invalid credentials" {
			t.Fatalf("%s 401 error = %v, want Google's envelope", s.handler, e)
		}
	}

	// ===== non-Bearer schemes and unknown or empty tokens are equally 401 =====
	for _, auth := range []string{
		"Token " + tok,       // wrong scheme
		"Basic dXNlcjpwYXNz", // wrong scheme entirely
		"bearer " + tok,      // case-mangled scheme
		"Bearer ",            // empty token
		"Bearer nope",        // never minted
		tok,                  // no scheme at all
	} {
		if r := f.get("media", "on_list", "/v1/mediaItems", nil, nil, auth); r.Status != 401 {
			t.Fatalf("auth %q -> %d, want 401", auth, r.Status)
		}
	}

	// ===== the minted bearer passes the gate on every surface =====
	for _, s := range surfaces {
		r := f.call(s.group, s.handler, s.method, s.path, s.params, nil, nil, "", "Bearer "+tok, "")
		if r.Status == 401 {
			t.Fatalf("%s with valid bearer -> 401", s.handler)
		}
	}
	if r := f.get("media", "on_list", "/v1/mediaItems", nil, nil, "Bearer "+tok); r.Status != 200 {
		t.Fatalf("list with valid bearer -> %d", r.Status)
	}

	// ===== the access token dies at its expiry on the virtual clock =====
	f.vc.Advance(3600 * time.Second) // expires_at = mint + 3599
	r := f.get("media", "on_list", "/v1/mediaItems", nil, nil, "Bearer "+tok)
	if r.Status != 401 || phErr(t, r)["status"] != "UNAUTHENTICATED" {
		t.Fatalf("expired bearer -> %d %v, want 401 UNAUTHENTICATED", r.Status, r.Body)
	}
}

// TestPhotosUploadTokenFlow: the two-step upload pipeline — /v1/uploads
// answers a plain-text uploadToken, batchCreate turns tokens into media
// items (mime from the file extension, metadata shape, per-item status
// errors for unknown tokens), tokens are single-use, and albumId is not
// validated (recorded as-is on the item).
func TestPhotosUploadTokenFlow(t *testing.T) {
	f := newPhotosFixture(t, time.Unix(1_750_000_000, 0).UTC())
	auth := "Bearer " + phLogin(f)

	// ===== /v1/uploads answers a plain-text uploadToken =====
	r := f.call("uploads", "on_uploads", "POST", "/v1/uploads", nil, nil, nil,
		"jpeg-bytes-alpha", auth, "image/jpeg")
	if r.Status != 200 || r.RawBody != "CAISI1mockUploadToken" {
		t.Fatalf("uploads -> %d %q, want 200 with the first minted token", r.Status, r.RawBody)
	}
	if r.Body != nil {
		t.Fatalf("uploads body = %v, want plain text (no JSON body)", r.Body)
	}
	ut1 := r.RawBody

	// ===== batchCreate links the token to a media item =====
	res := phCreateOne(f, auth, ut1, "sunset.jpg", "", "Sunset over the bay")
	mi := phMediaItemOf(t, res)
	if mi["id"] != "mock-media-1" || mi["filename"] != "sunset.jpg" || mi["mimeType"] != "image/jpeg" {
		t.Fatalf("mediaItem = %v", mi)
	}
	if mi["baseUrl"] != "http://"+f.host+"/v1/media-dl/mock-media-1" {
		t.Fatalf("baseUrl = %v, want the request-host download URL", mi["baseUrl"])
	}
	if mi["productUrl"] != "https://photos.google.com/mock/mock-media-1" {
		t.Fatalf("productUrl = %v", mi["productUrl"])
	}
	mm, _ := mi["mediaMetadata"].(map[string]any)
	if mm["creationTime"] != phCreationTime || mm["width"] != "1920" || mm["height"] != "1080" {
		t.Fatalf("mediaMetadata = %v", mm)
	}
	// The description is accepted but not echoed (real API returns it).
	if _, has := mi["description"]; has {
		t.Fatalf("mediaItem carries description = %v, want it dropped (as-is)", mi["description"])
	}

	// ===== mime types follow the file extension =====
	tokens := make([]string, 4)
	for i := range tokens {
		tokens[i] = phUpload(f, auth, fmt.Sprintf("mime-payload-%d", i), "application/octet-stream")
	}
	items := []phNewItem{
		{token: tokens[0], fileName: "photo.png"},
		{token: tokens[1], fileName: "clip.mov"},
		{token: tokens[2], fileName: "clip.mp4"},
		{token: tokens[3], fileName: "data.bin"},
	}
	results := phBatch(f, auth, "", items)
	wantMime := []string{"image/png", "video/quicktime", "video/mp4", "application/octet-stream"}
	for i, w := range wantMime {
		mi = phMediaItemOf(t, results[i].(map[string]any))
		if mi["mimeType"] != w {
			t.Fatalf("item %d mimeType = %v, want %s", i, mi["mimeType"], w)
		}
	}

	// ===== an unknown upload token answers the per-item status error =====
	res = phCreateOne(f, auth, "bogus-token", "bad.jpg", "", "")
	st, ok := res["status"].(map[string]any)
	if !ok || !phNum(st["code"], 3) || st["message"] != "Invalid upload token" {
		t.Fatalf("bogus token result = %v, want Google code 3", res)
	}
	if _, has := res["mediaItem"]; has {
		t.Fatalf("bogus token result carries a mediaItem: %v", res)
	}

	// ===== mixed batches keep request order =====
	ut2 := phUpload(f, auth, "jpeg-bytes-beta", "image/jpeg")
	results = phBatch(f, auth, "", []phNewItem{
		{token: ut2, fileName: "good.jpg"},
		{token: "bogus-2", fileName: "bad.jpg"},
	})
	if id := phMediaItemOf(t, results[0].(map[string]any))["id"]; id != "mock-media-6" {
		t.Fatalf("mixed batch result[0] id = %v, want mock-media-6", id)
	}
	if st, ok = results[1].(map[string]any)["status"].(map[string]any); !ok || !phNum(st["code"], 3) {
		t.Fatalf("mixed batch result[1] = %v, want the status error", results[1])
	}

	// ===== upload tokens are single-use =====
	res = phCreateOne(f, auth, ut1, "again.jpg", "", "")
	if _, has := res["mediaItem"]; has {
		t.Fatalf("reused upload token created %v, want code 3", res["mediaItem"])
	}
	if st, ok := res["status"].(map[string]any); !ok || !phNum(st["code"], 3) {
		t.Fatalf("reused upload token result = %v, want code 3", res)
	}

	// ===== an albumId is accepted without validation (as-is) =====
	ut3 := phUpload(f, auth, "jpeg-bytes-gamma", "image/jpeg")
	res = phCreateOne(f, auth, ut3, "albumed.jpg", "mock-album-nope", "")
	if id := phMediaItemOf(t, res)["id"]; id != "mock-media-7" {
		t.Fatalf("batchCreate with unknown albumId -> %v, want it accepted (as-is)", res)
	}

	// ===== an empty batch answers an empty result list =====
	r = f.post("media", "on_batch_create", "/v1/mediaItems:batchCreate", map[string]any{}, auth)
	if r.Status != 200 || len(r.Body["newMediaItemResults"].([]any)) != 0 {
		t.Fatalf("empty batch -> %d %v, want 200 with an empty list", r.Status, r.Body)
	}
}

// TestPhotosMediaDownload: the baseUrl media plane — =d and =dv serve the
// ORIGINAL uploaded bytes with the recorded Content-Type and need no bearer
// (the real baseUrl is pre-authorized), a bare baseUrl serves a distinct
// derivative payload, and baseUrl tracks the request host at read time.
func TestPhotosMediaDownload(t *testing.T) {
	f := newPhotosFixture(t, time.Unix(1_750_000_000, 0).UTC())
	auth := "Bearer " + phLogin(f)
	payload := "original-png-bytes-123"
	ut := phUpload(f, auth, payload, "image/png")
	id, _ := phMediaItemOf(t, phCreateOne(f, auth, ut, "shot.png", "", ""))["id"].(string)

	// ===== =d serves the original bytes with the recorded content type =====
	r := f.get("media", "on_media_dl", "/v1/media-dl/"+id+"=d",
		map[string]string{"id": id + "=d"}, nil, "")
	if r.Status != 200 || r.RawBody != payload {
		t.Fatalf("=d download -> %d %q, want the uploaded bytes", r.Status, r.RawBody)
	}
	if r.Headers["Content-Type"] != "image/png" {
		t.Fatalf("=d Content-Type = %q, want the recorded image/png", r.Headers["Content-Type"])
	}

	// ===== =dv serves the same original bytes =====
	r = f.get("media", "on_media_dl", "/v1/media-dl/"+id+"=dv",
		map[string]string{"id": id + "=dv"}, nil, "")
	if r.Status != 200 || r.RawBody != payload {
		t.Fatalf("=dv download -> %d %q, want the uploaded bytes", r.Status, r.RawBody)
	}

	// ===== a bare baseUrl serves the derivative payload instead =====
	r = f.get("media", "on_media_dl", "/v1/media-dl/"+id, map[string]string{"id": id}, nil, "")
	if r.Status != 200 {
		t.Fatalf("bare baseUrl -> %d, want 200", r.Status)
	}
	if want := phDerivativePfx + id + ":not-the-original-bytes"; r.RawBody != want {
		t.Fatalf("bare baseUrl payload = %q, want %q", r.RawBody, want)
	}
	if r.RawBody == payload {
		t.Fatal("bare baseUrl served the original bytes; want the derivative")
	}
	if r.Headers["Content-Type"] != "image/jpeg" {
		t.Fatalf("derivative Content-Type = %q, want image/jpeg", r.Headers["Content-Type"])
	}

	// ===== baseUrl is computed from the request host at read time =====
	r = f.callHost("media-dl.example.test", "media", "on_get", "GET", "/v1/mediaItems/"+id,
		map[string]string{"id": id}, nil, nil, "", auth, "")
	if r.Status != 200 {
		t.Fatalf("get with other host -> %d: %v", r.Status, r.Body)
	}
	if got := r.Body["baseUrl"]; got != "http://media-dl.example.test/v1/media-dl/"+id {
		t.Fatalf("baseUrl under other host = %v", got)
	}

	// ===== unknown ids answer the 404 envelope =====
	r = f.get("media", "on_media_dl", "/v1/media-dl/mock-media-999=d",
		map[string]string{"id": "mock-media-999=d"}, nil, "")
	if r.Status != 404 {
		t.Fatalf("unknown media-dl id -> %d, want 404", r.Status)
	}
	e := phErr(t, r)
	if !phNum(e["code"], 404) || e["status"] != "NOT_FOUND" || e["message"] != "Media item not found" {
		t.Fatalf("media-dl 404 error = %v", e)
	}
}

// TestPhotosMediaItemsPagination: list pages by pageSize/pageToken with a
// 25-item default and a 100-item clamp, the final partial page carries no
// nextPageToken, and items are private to the user that created them.
func TestPhotosMediaItemsPagination(t *testing.T) {
	f := newPhotosFixture(t, time.Unix(1_750_000_000, 0).UTC())
	auth := "Bearer " + phLogin(f)
	list := func(query map[string]string, a string) starlark.Response {
		return f.get("media", "on_list", "/v1/mediaItems", nil, query, a)
	}
	create := func(n, first int) {
		t.Helper()
		items := make([]phNewItem, 0, n)
		for i := 0; i < n; i++ {
			ut := phUpload(f, auth, fmt.Sprintf("payload-%03d", first+i), "image/jpeg")
			items = append(items, phNewItem{token: ut, fileName: fmt.Sprintf("p%03d.jpg", first+i)})
		}
		phBatch(f, auth, "", items)
	}

	create(3, 1) // mock-media-1..3

	// ===== pages walk by pageToken and the last partial page has no next =====
	r := list(map[string]string{"pageSize": "2"}, auth)
	if ids := phIDs(t, r); len(ids) != 2 || ids[0] != "mock-media-1" || ids[1] != "mock-media-2" {
		t.Fatalf("page 1 = %v", ids)
	}
	if r.Body["nextPageToken"] != "2" {
		t.Fatalf("page 1 nextPageToken = %v, want 2", r.Body["nextPageToken"])
	}
	r = list(map[string]string{"pageSize": "2", "pageToken": "2"}, auth)
	if ids := phIDs(t, r); len(ids) != 1 || ids[0] != "mock-media-3" {
		t.Fatalf("page 2 = %v", ids)
	}
	if _, has := r.Body["nextPageToken"]; has {
		t.Fatalf("final page carries nextPageToken = %v", r.Body["nextPageToken"])
	}
	// Off the end: empty page, no token.
	r = list(map[string]string{"pageToken": "99"}, auth)
	if len(phIDs(t, r)) != 0 {
		t.Fatalf("pageToken past end = %v, want empty", r.Body["mediaItems"])
	}

	// ===== the default page holds 25 items =====
	create(23, 4) // mock-media-4..26
	r = list(nil, auth)
	if ids := phIDs(t, r); len(ids) != 25 || ids[0] != "mock-media-1" {
		t.Fatalf("default page = %d items starting %v, want 25 from mock-media-1", len(ids), ids[0])
	}
	if r.Body["nextPageToken"] != "25" {
		t.Fatalf("default page nextPageToken = %v, want 25", r.Body["nextPageToken"])
	}
	r = list(map[string]string{"pageToken": "25"}, auth)
	if ids := phIDs(t, r); len(ids) != 1 || ids[0] != "mock-media-26" {
		t.Fatalf("tail page = %v", ids)
	}

	// ===== pageSize clamps at 100 =====
	r = list(map[string]string{"pageSize": "500"}, auth)
	if ids := phIDs(t, r); len(ids) != 26 {
		t.Fatalf("pageSize=500 -> %d items, want all 26 (clamped to 100)", len(ids))
	}
	if _, has := r.Body["nextPageToken"]; has {
		t.Fatalf("whole library in one page carries nextPageToken = %v", r.Body["nextPageToken"])
	}

	// ===== a single item fetch and the 404 envelope =====
	r = f.get("media", "on_get", "/v1/mediaItems/mock-media-1", map[string]string{"id": "mock-media-1"}, nil, auth)
	if r.Status != 200 || r.Body["id"] != "mock-media-1" {
		t.Fatalf("get item -> %d %v", r.Status, r.Body)
	}
	r = f.get("media", "on_get", "/v1/mediaItems/mock-media-999", map[string]string{"id": "mock-media-999"}, nil, auth)
	if r.Status != 404 || phErr(t, r)["status"] != "NOT_FOUND" {
		t.Fatalf("get unknown item -> %d %v, want 404 NOT_FOUND", r.Status, r.Body)
	}

	// ===== items are private to their user =====
	auth2 := "Bearer " + phLogin(f)
	if r := list(nil, auth2); len(phIDs(t, r)) != 0 {
		t.Fatalf("second user sees %v, want an empty library", r.Body["mediaItems"])
	}
	if r := f.get("media", "on_get", "/v1/mediaItems/mock-media-1", map[string]string{"id": "mock-media-1"}, nil, auth2); r.Status != 404 {
		t.Fatalf("second user get of another's item -> %d, want 404", r.Status)
	}
}

// TestPhotosSearchFilters: search honors the body's filters —
// mediaTypeFilter (PHOTO/VIDEO by mime prefix; naming both keeps only the
// last, as-is), dateFilter dates and ranges over the RFC 3339
// creationTime (with JSON floats), partial dates spanning their whole
// period, OR'd ranges preserving upload order, composition with the media
// type filter, albumId scoping, and body-driven pagination.
func TestPhotosSearchFilters(t *testing.T) {
	f := newPhotosFixture(t, time.Unix(1_750_000_000, 0).UTC())
	auth := "Bearer " + phLogin(f)
	search := func(body map[string]any) starlark.Response {
		return phSearch(f, auth, body)
	}

	// Four items: two photos, two videos.
	tokens := make([]string, 4)
	for i := range tokens {
		tokens[i] = phUpload(f, auth, fmt.Sprintf("filter-payload-%d", i), "application/octet-stream")
	}
	phBatch(f, auth, "", []phNewItem{
		{token: tokens[0], fileName: "a.jpg"},
		{token: tokens[1], fileName: "b.png"},
		{token: tokens[2], fileName: "c.mov"},
		{token: tokens[3], fileName: "d.mp4"},
	})
	all := []string{"mock-media-1", "mock-media-2", "mock-media-3", "mock-media-4"}
	photos := all[:2]
	videos := all[2:]

	// ===== an unfiltered search returns everything in creation order =====
	if ids := phIDs(t, search(map[string]any{})); len(ids) != 4 || ids[0] != all[0] || ids[3] != all[3] {
		t.Fatalf("unfiltered search = %v", ids)
	}

	// ===== mediaTypeFilter selects PHOTO or VIDEO by mime prefix =====
	mtf := func(types ...string) map[string]any {
		anyTypes := make([]any, len(types))
		for i, t := range types {
			anyTypes[i] = t
		}
		return map[string]any{"filters": map[string]any{"mediaTypeFilter": map[string]any{"mediaTypes": anyTypes}}}
	}
	if ids := phIDs(t, search(mtf("PHOTO"))); len(ids) != 2 || ids[0] != photos[0] || ids[1] != photos[1] {
		t.Fatalf("PHOTO filter = %v, want %v", ids, photos)
	}
	if ids := phIDs(t, search(mtf("VIDEO"))); len(ids) != 2 || ids[0] != videos[0] || ids[1] != videos[1] {
		t.Fatalf("VIDEO filter = %v, want %v", ids, videos)
	}
	if ids := phIDs(t, search(mtf("ALL_MEDIA"))); len(ids) != 4 {
		t.Fatalf("ALL_MEDIA filter = %v, want the no-op full list", ids)
	}
	// Naming both types keeps only the last (real API rejects with 400).
	if ids := phIDs(t, search(mtf("PHOTO", "VIDEO"))); len(ids) != 2 || ids[0] != videos[0] {
		t.Fatalf("PHOTO+VIDEO filter = %v, want only VIDEO (as-is)", ids)
	}

	// ===== dateFilter ranges and exact dates bound creationTime =====
	// JSON numbers arrive as floats, so dates use float64 like a raw body.
	df := func(body map[string]any) map[string]any {
		return map[string]any{"filters": map[string]any{"dateFilter": body}}
	}
	r := search(df(map[string]any{"ranges": []any{map[string]any{
		"startDate": map[string]any{"year": 2024.0, "month": 6.0, "day": 15.0},
		"endDate":   map[string]any{"year": 2024.0, "month": 6.0, "day": 15.0},
	}}}))
	if ids := phIDs(t, r); len(ids) != 4 {
		t.Fatalf("same-day range = %v, want all 4", ids)
	}
	r = search(df(map[string]any{"ranges": []any{map[string]any{
		"startDate": map[string]any{"year": 2023.0},
		"endDate":   map[string]any{"year": 2023.0},
	}}}))
	if ids := phIDs(t, r); len(ids) != 0 {
		t.Fatalf("2023 range = %v, want none", ids)
	}
	r = search(df(map[string]any{"dates": []any{map[string]any{"year": 2024.0, "month": 6.0, "day": 15.0}}}))
	if ids := phIDs(t, r); len(ids) != 4 {
		t.Fatalf("exact date = %v, want all 4", ids)
	}

	// ===== partial dates span their whole period =====
	for _, tc := range []struct {
		date map[string]any
		want int
	}{
		{map[string]any{"year": 2024.0}, 4},               // whole year
		{map[string]any{"year": 2024.0, "month": 6.0}, 4}, // whole June
		{map[string]any{"year": 2024.0, "month": 7.0}, 0}, // July starts after 06-15
		{map[string]any{"year": 2025.0}, 0},               // next year
	} {
		r = search(df(map[string]any{"dates": []any{tc.date}}))
		if ids := phIDs(t, r); len(ids) != tc.want {
			t.Fatalf("date %v -> %v, want %d items", tc.date, ids, tc.want)
		}
	}

	// ===== multiple ranges are OR'd and keep upload order =====
	r = search(df(map[string]any{"ranges": []any{
		map[string]any{"startDate": map[string]any{"year": 2023.0}, "endDate": map[string]any{"year": 2023.0}},
		map[string]any{"startDate": map[string]any{"year": 2024.0, "month": 6.0, "day": 15.0},
			"endDate": map[string]any{"year": 2024.0, "month": 6.0, "day": 15.0}},
	}}))
	if ids := phIDs(t, r); len(ids) != 4 || ids[0] != all[0] || ids[3] != all[3] {
		t.Fatalf("OR'd ranges = %v, want all 4 in upload order", ids)
	}

	// ===== media type and date filters compose =====
	r = search(map[string]any{"filters": map[string]any{
		"mediaTypeFilter": map[string]any{"mediaTypes": []any{"PHOTO"}},
		"dateFilter":      map[string]any{"dates": []any{map[string]any{"year": 2024.0}}},
	}})
	if ids := phIDs(t, r); len(ids) != 2 || ids[0] != photos[0] || ids[1] != photos[1] {
		t.Fatalf("composed filters = %v, want the 2 photos", ids)
	}

	// ===== albumId scopes the search =====
	album := f.post("albums", "on_create_album", "/v1/albums",
		map[string]any{"album": map[string]any{"title": "Trip"}}, auth)
	if album.Status != 200 {
		t.Fatalf("create album -> %d: %v", album.Status, album.Body)
	}
	albumID := album.Body["id"].(string)
	albumTokens := []string{phUpload(f, auth, "album-payload-1", "image/jpeg"), phUpload(f, auth, "album-payload-2", "image/jpeg")}
	phBatch(f, auth, albumID, []phNewItem{
		{token: albumTokens[0], fileName: "t1.jpg"}, {token: albumTokens[1], fileName: "t2.jpg"},
	})
	if ids := phIDs(t, search(map[string]any{"albumId": albumID})); len(ids) != 2 ||
		ids[0] != "mock-media-5" || ids[1] != "mock-media-6" {
		t.Fatalf("albumId search = %v, want the two album items", ids)
	}
	if ids := phIDs(t, search(map[string]any{})); len(ids) != 6 {
		t.Fatalf("unscoped search after album adds = %d items, want 6", len(ids))
	}

	// ===== search paginates from the JSON body =====
	r = search(map[string]any{"pageSize": 2.0}) // float, as decoded JSON
	if ids := phIDs(t, r); len(ids) != 2 || r.Body["nextPageToken"] != "2" {
		t.Fatalf("search page 1 = %v next %v", ids, r.Body["nextPageToken"])
	}
	r = search(map[string]any{"pageSize": 2, "pageToken": "2"})
	if ids := phIDs(t, r); len(ids) != 2 || r.Body["nextPageToken"] != "4" {
		t.Fatalf("search page 2 = %v next %v", ids, r.Body["nextPageToken"])
	}
	r = search(map[string]any{"pageSize": 2, "pageToken": "4"})
	if ids := phIDs(t, r); len(ids) != 2 {
		t.Fatalf("search page 3 = %v", ids)
	}
	if _, has := r.Body["nextPageToken"]; has {
		t.Fatalf("final search page carries nextPageToken = %v", r.Body["nextPageToken"])
	}
}

// TestPhotosAlbumsLifecycle: album CRUD — the created shape (mediaItemsCount
// as a string, no shareInfo: sharing is not modeled), the Untitled default,
// per-user scoping of list/get/delete, list pagination with the 20 default
// and 50 clamp, and delete removing the album while leaving its media in
// the library (they keep pointing at the dead albumId, as-is).
func TestPhotosAlbumsLifecycle(t *testing.T) {
	f := newPhotosFixture(t, time.Unix(1_750_000_000, 0).UTC())
	auth := "Bearer " + phLogin(f)
	list := func(query map[string]string, a string) []any {
		t.Helper()
		r := f.get("albums", "on_list_albums", "/v1/albums", nil, query, a)
		if r.Status != 200 {
			t.Fatalf("list albums -> %d: %v", r.Status, r.Body)
		}
		return r.Body["albums"].([]any)
	}

	// ===== create returns the album shape =====
	r := f.post("albums", "on_create_album", "/v1/albums",
		map[string]any{"album": map[string]any{"title": "Summer 2024"}}, auth)
	if r.Status != 200 {
		t.Fatalf("create album -> %d: %v", r.Status, r.Body)
	}
	album := r.Body
	if album["id"] != "mock-album-1" || album["title"] != "Summer 2024" ||
		album["isWriteable"] != true || album["mediaItemsCount"] != "0" {
		t.Fatalf("album = %v", album)
	}
	if album["productUrl"] != "https://photos.google.com/album/mock-album-1" ||
		album["coverPhotoBaseUrl"] != "https://mock-photos.example/cover/mock-album-1" {
		t.Fatalf("album URLs = %v", album)
	}
	// Sharing is not modeled: no shareInfo anywhere (as-is).
	if _, has := album["shareInfo"]; has {
		t.Fatalf("album carries shareInfo = %v, want it absent (not modeled)", album["shareInfo"])
	}

	// ===== a missing album title defaults to Untitled Album =====
	r = f.post("albums", "on_create_album", "/v1/albums", map[string]any{}, auth)
	if r.Status != 200 || r.Body["title"] != "Untitled Album" {
		t.Fatalf("album without title -> %d %v, want Untitled Album", r.Status, r.Body)
	}

	// ===== unknown albums answer the 404 envelope =====
	r = f.get("albums", "on_get_album", "/v1/albums/mock-album-404", map[string]string{"id": "mock-album-404"}, nil, auth)
	if r.Status != 404 {
		t.Fatalf("unknown album -> %d, want 404", r.Status)
	}
	e := phErr(t, r)
	if !phNum(e["code"], 404) || e["status"] != "NOT_FOUND" || e["message"] != "Album not found: mock-album-404" {
		t.Fatalf("album 404 error = %v", e)
	}

	// ===== list walks pages with the 20 default and the 50 clamp =====
	for i := 0; i < 19; i++ {
		if r := f.post("albums", "on_create_album", "/v1/albums",
			map[string]any{"album": map[string]any{"title": "Bulk"}}, auth); r.Status != 200 {
			t.Fatalf("bulk album %d -> %d: %v", i, r.Status, r.Body)
		}
	}
	if got := list(nil, auth); len(got) != 20 {
		t.Fatalf("default album page = %d, want 20", len(got))
	}
	lr := f.get("albums", "on_list_albums", "/v1/albums", nil, nil, auth)
	if lr.Body["nextPageToken"] != "20" {
		t.Fatalf("default album page nextPageToken = %v, want 20", lr.Body["nextPageToken"])
	}
	if got := list(map[string]string{"pageToken": "20"}, auth); len(got) != 1 {
		t.Fatalf("album tail page = %d, want 1", len(got))
	}
	if got := list(map[string]string{"pageSize": "500"}, auth); len(got) != 21 {
		t.Fatalf("pageSize=500 -> %d albums, want all 21 (clamped to 50)", len(got))
	}

	// ===== albums are private to their user =====
	auth2 := "Bearer " + phLogin(f)
	if got := list(nil, auth2); len(got) != 0 {
		t.Fatalf("second user sees %d albums, want none", len(got))
	}
	if r := f.get("albums", "on_get_album", "/v1/albums/mock-album-1", map[string]string{"id": "mock-album-1"}, nil, auth2); r.Status != 404 {
		t.Fatalf("second user get of another's album -> %d, want 404", r.Status)
	}
	if r := f.del("albums", "on_delete_album", "/v1/albums/mock-album-1", map[string]string{"id": "mock-album-1"}, auth2); r.Status != 404 {
		t.Fatalf("second user delete of another's album -> %d, want 404", r.Status)
	}

	// ===== delete removes the album but leaves its media =====
	created := f.post("albums", "on_create_album", "/v1/albums",
		map[string]any{"album": map[string]any{"title": "Bye"}}, auth)
	byeID := created.Body["id"].(string)
	ut := phUpload(f, auth, "bye-payload", "image/jpeg")
	if res := phCreateOne(f, auth, ut, "bye.jpg", byeID, ""); res["mediaItem"] == nil {
		t.Fatalf("create into album -> %v", res)
	}
	// A linked item never bumps mediaItemsCount (as-is: stays "0").
	if r := f.get("albums", "on_get_album", "/v1/albums/"+byeID, map[string]string{"id": byeID}, nil, auth); r.Body["mediaItemsCount"] != "0" {
		t.Fatalf("mediaItemsCount with a linked item = %v, want 0 (as-is)", r.Body["mediaItemsCount"])
	}
	if r := f.del("albums", "on_delete_album", "/v1/albums/"+byeID, map[string]string{"id": byeID}, auth); r.Status != 200 || len(r.Body) != 0 {
		t.Fatalf("delete album -> %d %v, want 200 with an empty body", r.Status, r.Body)
	}
	if r := f.get("albums", "on_get_album", "/v1/albums/"+byeID, map[string]string{"id": byeID}, nil, auth); r.Status != 404 {
		t.Fatalf("get deleted album -> %d, want 404", r.Status)
	}
	if r := f.del("albums", "on_delete_album", "/v1/albums/"+byeID, map[string]string{"id": byeID}, auth); r.Status != 404 {
		t.Fatalf("delete deleted album -> %d, want 404", r.Status)
	}
	// The media survives in the library...
	if ids := phIDs(t, f.get("media", "on_list", "/v1/mediaItems", nil, nil, auth)); len(ids) != 1 {
		t.Fatalf("library after album delete = %v, want the media item", ids)
	}
	// ...and still answers to the dead album's id (as-is).
	if ids := phIDs(t, phSearch(f, auth, map[string]any{"albumId": byeID})); len(ids) != 1 {
		t.Fatalf("albumId search after album delete = %v, want the orphaned item (as-is)", ids)
	}
}
