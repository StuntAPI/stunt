package adapters

import (
	"encoding/base64"
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

// Drives the reddit-style adapter scripts directly (lib.star preloaded) over
// a shared store and virtual clock: the User-Agent gate on both routes, the
// HTTP Basic client-credential gate on the token endpoint, the
// authorization_code (permanent -> access + refresh) and refresh_token
// (access only) grants, the Bearer gate on submit with the USER_REQUIRED
// json envelope, the t3_ thing shape, and the one-hour token expiry.
const redditUA = "stunt-vm-suite/1.0 (by /u/tester)"

// redditBasic computes the HTTP Basic client-credential header value.
func redditBasic() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("vm-client-id:vm-client-secret"))
}

type redditFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newRedditFixture(t *testing.T, start time.Time) *redditFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "reddit-style")
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
	return &redditFixture{t: t, vc: vc, host: "www.reddit.test", vms: map[string]*starlark.VM{
		"oauth": load("oauth.star"), "submit": load("submit.star"),
	}}
}

// call drives one POST handler; headers always carry the UA unless ua is
// overridden ("" means none, mirroring a request without the header).
func (f *redditFixture) call(group, handler, path string, form map[string]any, auth, ua string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if ua != "" {
		headers["User-Agent"] = ua
	}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: "POST", Path: path, Host: f.host, Headers: headers, Body: form,
		Params: map[string]string{}, Query: map[string]string{},
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// rdErrorTriple unwraps the Reddit json envelope's first error triple
// ([CODE, message, field]).
func rdErrorTriple(t *testing.T, r starlark.Response) []any {
	t.Helper()
	j, ok := r.Body["json"].(map[string]any)
	if !ok {
		t.Fatalf("body has no json envelope: %v", r.Body)
	}
	errs, ok := j["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("json.errors = %v, want a non-empty list", j["errors"])
	}
	triple, ok := errs[0].([]any)
	if !ok || len(triple) < 2 {
		t.Fatalf("error triple = %v, want [CODE, message, field]", errs[0])
	}
	return triple
}

// TestRedditTokenEndpoint: the User-Agent and HTTP Basic gates plus the
// authorization_code and refresh_token grant shapes.
func TestRedditTokenEndpoint(t *testing.T) {
	f := newRedditFixture(t, time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC))
	tokenPath := "/api/v1/access_token"

	// ===== a missing or generic User-Agent is 429 on both routes =====
	// Reddit 429s absent/generic UAs; the mock reproduces it.
	noUA := f.call("oauth", "on_access_token", tokenPath,
		map[string]any{"grant_type": "authorization_code"}, redditBasic(), "")
	if noUA.Status != 429 || noUA.Body["message"] != "Too Many Requests" {
		t.Fatalf("no UA -> %d %v, want 429 Too Many Requests", noUA.Status, noUA.Body)
	}
	if rdNum(noUA.Body["error"]) != 429 {
		t.Fatalf("UA-reject error = %v (%T), want 429", noUA.Body["error"], noUA.Body["error"])
	}
	seeded := f.call("oauth", "on_access_token", tokenPath,
		map[string]any{"grant_type": "authorization_code", "duration": "permanent"}, redditBasic(), redditUA)
	access, _ := seeded.Body["access_token"].(string)
	generic := f.call("submit", "on_submit", "/api/submit",
		map[string]any{"sr": "test", "title": "Title"}, "Bearer "+access, "python-requests/2.31.0")
	if generic.Status != 429 {
		t.Fatalf("generic UA (no parens) -> %d, want 429", generic.Status)
	}

	// ===== the token endpoint requires HTTP Basic client credentials =====
	// No Basic header -> 401 invalid_client; an unknown grant is a 400.
	if r := f.call("oauth", "on_access_token", tokenPath,
		map[string]any{"grant_type": "authorization_code"}, "", redditUA); r.Status != 401 || r.Body["error"] != "invalid_client" {
		t.Fatalf("no Basic -> %d %v, want 401 invalid_client", r.Status, r.Body)
	}
	if r := f.call("oauth", "on_access_token", tokenPath,
		map[string]any{"grant_type": "client_credentials"}, redditBasic(), redditUA); r.Status != 400 || r.Body["error"] != "unsupported_grant_type" {
		t.Fatalf("unknown grant -> %d %v, want 400 unsupported_grant_type", r.Status, r.Body)
	}

	// ===== a permanent authorization_code mints access and refresh together =====
	if seeded.Status != 200 {
		t.Fatalf("authorization_code -> %d: %v", seeded.Status, seeded.Body)
	}
	if access == "" {
		t.Fatalf("authorization_code returned no access_token: %v", seeded.Body)
	}
	refresh, _ := seeded.Body["refresh_token"].(string)
	if refresh == "" || refresh[:6] != "rdref_" {
		t.Fatalf("permanent grant refresh_token = %q, want an rdref_ token", refresh)
	}
	rdAssertTokenShape(t, seeded.Body)

	// A temporary grant (no duration) issues no refresh token at all.
	temp := f.call("oauth", "on_access_token", tokenPath,
		map[string]any{"grant_type": "authorization_code"}, redditBasic(), redditUA)
	if _, exists := temp.Body["refresh_token"]; exists {
		t.Fatalf("temporary grant issued a refresh_token: %v", temp.Body)
	}
	rdAssertTokenShape(t, temp.Body)

	// ===== a refresh grant returns a fresh access token and no new refresh =====
	// Reddit only issues a refresh token on the initial permanent grant.
	refreshed := f.call("oauth", "on_access_token", tokenPath,
		map[string]any{"grant_type": "refresh_token", "refresh_token": refresh}, redditBasic(), redditUA)
	if refreshed.Status != 200 {
		t.Fatalf("refresh grant -> %d: %v", refreshed.Status, refreshed.Body)
	}
	if _, exists := refreshed.Body["refresh_token"]; exists {
		t.Fatalf("refresh grant returned a new refresh_token: %v", refreshed.Body)
	}
	if got, _ := refreshed.Body["access_token"].(string); got == access {
		t.Fatalf("refresh grant returned the same access token %q, want a fresh one", got)
	}
	rdAssertTokenShape(t, refreshed.Body)
	if r := f.call("oauth", "on_access_token", tokenPath,
		map[string]any{"grant_type": "refresh_token", "refresh_token": "rdref_unknown"}, redditBasic(), redditUA); r.Status != 400 || r.Body["error"] != "invalid_grant" {
		t.Fatalf("unknown refresh token -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}
}

// rdAssertTokenShape checks the shared OAuth token response fields.
func rdAssertTokenShape(t *testing.T, body map[string]any) {
	t.Helper()
	if body["token_type"] != "bearer" {
		t.Fatalf("token_type = %v, want bearer", body["token_type"])
	}
	if rdNum(body["expires_in"]) != 3600 {
		t.Fatalf("expires_in = %v (%T), want 3600", body["expires_in"], body["expires_in"])
	}
	if body["scope"] != "submit identity" {
		t.Fatalf("scope = %v, want 'submit identity'", body["scope"])
	}
}

// TestRedditSubmit: the Bearer gate, the t3_ thing envelope, and the
// HTTP-200 error-triple vocabulary.
func TestRedditSubmit(t *testing.T) {
	f := newRedditFixture(t, time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC))
	mint := func() string {
		t.Helper()
		r := f.call("oauth", "on_access_token", "/api/v1/access_token",
			map[string]any{"grant_type": "authorization_code"}, redditBasic(), redditUA)
		tok, _ := r.Body["access_token"].(string)
		if r.Status != 200 || tok == "" {
			t.Fatalf("mint access token -> %d: %v", r.Status, r.Body)
		}
		return tok
	}
	access := mint()

	// ===== submit requires a bearer the adapter itself minted =====
	// Missing, unknown, and expired tokens all get Reddit's USER_REQUIRED
	// json envelope.
	noAuth := f.call("submit", "on_submit", "/api/submit",
		map[string]any{"sr": "test", "title": "Title"}, "", redditUA)
	if noAuth.Status != 401 {
		t.Fatalf("submit without bearer -> %d, want 401", noAuth.Status)
	}
	if code := rdErrorTriple(t, noAuth)[0]; code != "USER_REQUIRED" {
		t.Fatalf("no-bearer error code = %v, want USER_REQUIRED", code)
	}
	unknown := f.call("submit", "on_submit", "/api/submit",
		map[string]any{"sr": "test", "title": "Title"}, "Bearer rdtok_999", redditUA)
	if unknown.Status != 401 {
		t.Fatalf("submit with unknown bearer -> %d, want 401", unknown.Status)
	}
	if code := rdErrorTriple(t, unknown)[0]; code != "USER_REQUIRED" {
		t.Fatalf("unknown-bearer error code = %v, want USER_REQUIRED", code)
	}

	// ===== a valid submit returns the t3_ thing envelope =====
	ok := f.call("submit", "on_submit", "/api/submit", map[string]any{
		"sr": "test", "title": "Hello from the stunt VM suite", "kind": "self",
	}, "Bearer "+access, redditUA)
	if ok.Status != 200 {
		t.Fatalf("submit -> %d: %v", ok.Status, ok.Body)
	}
	j, _ := ok.Body["json"].(map[string]any)
	if errs, _ := j["errors"].([]any); len(errs) != 0 {
		t.Fatalf("valid submit errors = %v, want empty", errs)
	}
	data, _ := j["data"].(map[string]any)
	postID, _ := data["id"].(string)
	if postID == "" {
		t.Fatalf("submit data = %v, want an id", data)
	}
	if data["name"] != "t3_"+postID {
		t.Fatalf("submit name = %v, want t3_%s", data["name"], postID)
	}
	url, _ := data["url"].(string)
	if want := "/r/test/comments/" + postID + "/hello_from_the_stunt_vm_suite/"; !strings.Contains(url, want) {
		t.Fatalf("submit url = %q, want it to contain %q", url, want)
	}

	// ===== missing sr or title stay HTTP 200 with Reddit error triples =====
	noSr := f.call("submit", "on_submit", "/api/submit",
		map[string]any{"title": "Title"}, "Bearer "+access, redditUA)
	if noSr.Status != 200 {
		t.Fatalf("submit without sr -> %d, want 200 (Reddit reports field errors in-band)", noSr.Status)
	}
	if triple := rdErrorTriple(t, noSr); triple[0] != "SUBREDDIT_REQUIRED" || triple[2] != "sr" {
		t.Fatalf("no-sr triple = %v, want [SUBREDDIT_REQUIRED, ..., sr]", triple)
	}
	noTitle := f.call("submit", "on_submit", "/api/submit",
		map[string]any{"sr": "test"}, "Bearer "+access, redditUA)
	if noTitle.Status != 200 {
		t.Fatalf("submit without title -> %d, want 200", noTitle.Status)
	}
	if triple := rdErrorTriple(t, noTitle); triple[0] != "NO_TEXT" || triple[2] != "title" {
		t.Fatalf("no-title triple = %v, want [NO_TEXT, ..., title]", triple)
	}

	// ===== an access token dies after its one-hour window =====
	// The minted expires_at is honored on read (virtual clock, no sleeps).
	f.vc.Advance(3601 * time.Second)
	expired := f.call("submit", "on_submit", "/api/submit",
		map[string]any{"sr": "test", "title": "Title"}, "Bearer "+access, redditUA)
	if expired.Status != 401 {
		t.Fatalf("submit with expired bearer -> %d, want 401", expired.Status)
	}
	if code := rdErrorTriple(t, expired)[0]; code != "USER_REQUIRED" {
		t.Fatalf("expired-bearer error code = %v, want USER_REQUIRED", code)
	}
}

// rdNum reads a response number whether the adapter produced a fresh
// Starlark int or a value that round-tripped the JSON document store.
func rdNum(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return -1
}
