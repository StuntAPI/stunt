package adapters

import (
	"encoding/base64"
	"os"
	"path/filepath"
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

// Drives the apple-appstoreconnect-style adapter scripts directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock: Apple's signed-issuer
// ES256 JWT gate (structural pass, then the KV token registry), the JSON:API
// apps surface with its bracketed filter[...]/sort/fields[apps] conventions
// and cursor paging, the derive-on-read appStoreVersion review machine and
// build processing machine (windows driven by the clock, not sleeps), and
// the users / appPrices / salesReports surfaces — all against Apple's
// errors-array envelope.

// ascJWT builds the static ES256 JWT lib.star registers in the KV token
// registry on first use — the registered credential every passing call
// carries. Signature is the synthetic placeholder (structural gate only).
func ascJWT() string {
	header := `{"alg":"ES256","kid":"TESTKEY123","typ":"JWT"}`
	payload := `{"iss":"test-issuer","iat":1700000000,"exp":1700003600,"aud":"appstoreconnect-v1"}`
	return base64.RawURLEncoding.EncodeToString([]byte(header)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(payload)) +
		".c3ludGhldGljLXNpZ25hdHVyZQ"
}

// ascMintJWT builds any 3-segment JWT from plaintext header/payload JSON, for
// the negative cases at the gate.
func ascMintJWT(headerJSON, payloadJSON string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(headerJSON)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(payloadJSON)) +
		".c3ludGhldGljLXNpZ25hdHVyZQ"
}

type ascFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newAscFixture(t *testing.T, start time.Time) *ascFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "apple-appstoreconnect-style")
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
	return &ascFixture{t: t, vc: vc, host: "api.appstoreconnect.test", vms: map[string]*starlark.VM{
		"apps": load("apps.star"), "versions": load("versions.star"), "misc": load("misc.star"),
	}}
}

func (f *ascFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
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

// asc serves a route with the registered JWT ("" auth = header absent).
func (f *ascFixture) asc(group, handler, method, path string, params, query map[string]string, body map[string]any) starlark.Response {
	f.t.Helper()
	return f.call(group, handler, method, path, params, query, body, "Bearer "+ascJWT())
}

// createApp registers an app and returns its id.
func (f *ascFixture) createApp(name, bundleID string, extra map[string]any) string {
	f.t.Helper()
	attrs := map[string]any{"name": name, "bundleId": bundleID}
	for k, v := range extra {
		attrs[k] = v
	}
	resp := f.asc("apps", "on_create_app", "POST", "/v1/apps", nil, nil, map[string]any{
		"data": map[string]any{"type": "apps", "attributes": attrs},
	})
	if resp.Status != 201 {
		f.t.Fatalf("create app %s -> %d: %v", name, resp.Status, resp.Body)
	}
	ent := ascData(f.t, resp)
	id, _ := ent["id"].(string)
	if id == "" {
		f.t.Fatalf("create app %s returned no id: %v", name, resp.Body)
	}
	return id
}

// listApps runs GET /v1/apps with optional query params.
func (f *ascFixture) listApps(query map[string]string) starlark.Response {
	f.t.Helper()
	return f.asc("apps", "on_list_apps", "GET", "/v1/apps", nil, query, nil)
}

// builds lists an app's builds with optional query params.
func (f *ascFixture) builds(appID string, query map[string]string) starlark.Response {
	f.t.Helper()
	return f.asc("apps", "on_list_builds", "GET", "/v1/apps/"+appID+"/builds",
		map[string]string{"id": appID}, query, nil)
}

// submitVersion POSTs the appStoreVersionSubmissions document (the real
// "submit for review" action).
func (f *ascFixture) submitVersion(versionID string) starlark.Response {
	f.t.Helper()
	return f.asc("versions", "on_create_version_submission", "POST", "/v1/appStoreVersionSubmissions",
		nil, nil, map[string]any{
			"data": map[string]any{
				"type": "appStoreVersionSubmissions",
				"relationships": map[string]any{
					"appStoreVersion": map[string]any{
						"data": map[string]any{"type": "appStoreVersions", "id": versionID},
					},
				},
			},
		})
}

// versionState reads a version's derived appStoreState (a read advances the
// derive-on-read machine, like the engine's GET).
func (f *ascFixture) versionState(versionID string) string {
	f.t.Helper()
	resp := f.asc("versions", "on_get_version", "GET", "/v1/appStoreVersions/"+versionID,
		map[string]string{"id": versionID}, nil, nil)
	if resp.Status != 200 {
		f.t.Fatalf("get version %s -> %d: %v", versionID, resp.Status, resp.Body)
	}
	s, _ := ascAttr(f.t, ascData(f.t, resp))["appStoreState"].(string)
	return s
}

// --- assertion helpers ---

// ascData returns the JSON:API primary data object of a response.
func ascData(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	d, ok := r.Body["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %T(%v), want object", r.Body["data"], r.Body["data"])
	}
	return d
}

// ascEntityList returns the JSON:API data array of a list response as entity
// maps.
func ascEntityList(t *testing.T, r starlark.Response) []map[string]any {
	t.Helper()
	arr, ok := r.Body["data"].([]any)
	if !ok {
		t.Fatalf("data = %T(%v), want array", r.Body["data"], r.Body["data"])
	}
	out := []map[string]any{}
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("entity is %T, want object", e)
		}
		out = append(out, m)
	}
	return out
}

// ascAttr returns the attributes object of a JSON:API entity.
func ascAttr(t *testing.T, entity map[string]any) map[string]any {
	t.Helper()
	a, ok := entity["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("attributes = %T(%v), want object", entity["attributes"], entity["attributes"])
	}
	return a
}

// ascRelID digs relationships.<name>.data.id out of a JSON:API entity.
func ascRelID(t *testing.T, entity map[string]any, rel string) string {
	t.Helper()
	rels, ok := entity["relationships"].(map[string]any)
	if !ok {
		t.Fatalf("relationships = %T(%v), want object", entity["relationships"], entity["relationships"])
	}
	r, ok := rels[rel].(map[string]any)
	if !ok {
		t.Fatalf("relationships.%s = %T(%v), want object", rel, rels[rel], rels[rel])
	}
	d, ok := r["data"].(map[string]any)
	if !ok {
		t.Fatalf("relationships.%s.data = %T(%v), want object", rel, r["data"], r["data"])
	}
	id, _ := d["id"].(string)
	return id
}

// ascErrEnvelope asserts Apple's errors array — exactly one error carrying
// the HTTP status as a STRING, the code, and non-empty title/detail — and
// returns it for detail checks.
func ascErrEnvelope(t *testing.T, r starlark.Response, wantStatus int, wantCode string) map[string]any {
	t.Helper()
	if r.Status != wantStatus {
		t.Fatalf("%s: status -> %d, want %d; body %v", wantCode, r.Status, wantStatus, r.Body)
	}
	errs, ok := r.Body["errors"].([]any)
	if !ok || len(errs) != 1 {
		t.Fatalf("%s: errors = %v, want exactly one entry", wantCode, r.Body["errors"])
	}
	e, ok := errs[0].(map[string]any)
	if !ok {
		t.Fatalf("%s: errors[0] is %T, want object", wantCode, errs[0])
	}
	if s, _ := e["status"].(string); s != strconv.Itoa(wantStatus) {
		t.Fatalf("%s: errors[0].status = %v (%T), want the string %q — Apple sends the HTTP status as a string", wantCode, e["status"], e["status"], strconv.Itoa(wantStatus))
	}
	if e["code"] != wantCode {
		t.Fatalf("error code = %v, want %s (envelope %v)", e["code"], wantCode, e)
	}
	if s, _ := e["title"].(string); s == "" {
		t.Fatalf("%s: error title is empty (envelope %v)", wantCode, e)
	}
	if s, _ := e["detail"].(string); s == "" {
		t.Fatalf("%s: error detail is empty (envelope %v)", wantCode, e)
	}
	return e
}

// ascNum compares a JSON number regardless of int64/float64 width (values
// round-tripped through the collection store come back floats).
func ascNum(t *testing.T, v any, want float64, what string) {
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

// ascPaging returns the meta.paging block of a paged list response.
func ascPaging(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	meta, ok := r.Body["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta = %T(%v), want object", r.Body["meta"], r.Body["meta"])
	}
	p, ok := meta["paging"].(map[string]any)
	if !ok {
		t.Fatalf("meta.paging = %T(%v), want object", meta["paging"], meta["paging"])
	}
	return p
}

// ascLinks returns the links block of a response.
func ascLinks(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	links, ok := r.Body["links"].(map[string]any)
	if !ok {
		t.Fatalf("links = %T(%v), want object", r.Body["links"], r.Body["links"])
	}
	return links
}

// TestAppStoreConnectVMJWTGate: the two-pass credential gate — structural
// (an ES256 JOSE header carrying a kid), then registry (the exact JWT string
// must be registered and unexpired) — and Apple's 401 errors envelope, shared
// by every surface.
func TestAppStoreConnectVMJWTGate(t *testing.T) {
	f := newAscFixture(t, time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC))

	// ===== a rejected credential answers Apple's 401 errors array, not a bare status =====
	e := ascErrEnvelope(t, f.call("apps", "on_list_apps", "GET", "/v1/apps", nil, nil, nil, ""), 401, "NOT_AUTHORIZED")
	if d, _ := e["detail"].(string); !strings.Contains(d, "ES256") {
		t.Fatalf("401 detail = %q, want it to name ES256", d)
	}
	// A non-Bearer scheme is refused at the same door.
	ascErrEnvelope(t, f.call("apps", "on_list_apps", "GET", "/v1/apps", nil, nil, nil, "Basic dXNlcjpwYXNz"), 401, "NOT_AUTHORIZED")
	// A token that is not 3 dot-separated segments never reaches the JOSE check.
	ascErrEnvelope(t, f.call("apps", "on_list_apps", "GET", "/v1/apps", nil, nil, nil, "Bearer a.b"), 401, "NOT_AUTHORIZED")

	// ===== the JOSE header must declare ES256 and carry a kid =====
	// Apple only signs ASC JWTs ES256: HS256 and RS256 are refused at the door.
	for _, jwt := range []string{
		ascMintJWT(`{"alg":"HS256","typ":"JWT"}`, `{"iss":"test-issuer"}`),
		ascMintJWT(`{"alg":"RS256","kid":"TESTKEY123","typ":"JWT"}`, `{"iss":"test-issuer"}`),
		ascMintJWT(`{"alg":"ES256","typ":"JWT"}`, `{"iss":"test-issuer"}`), // no kid
	} {
		ascErrEnvelope(t, f.call("apps", "on_list_apps", "GET", "/v1/apps", nil, nil, nil, "Bearer "+jwt), 401, "NOT_AUTHORIZED")
	}

	// ===== structural validity is not enough: the exact JWT string must be registered =====
	// A second, well-formed ES256+kid token nobody registered is still 401 — the registry pass.
	stranger := ascMintJWT(`{"alg":"ES256","kid":"OTHERKEY9","typ":"JWT"}`, `{"iss":"other-issuer"}`)
	ascErrEnvelope(t, f.call("apps", "on_list_apps", "GET", "/v1/apps", nil, nil, nil, "Bearer "+stranger), 401, "NOT_AUTHORIZED")
	// The registered static JWT passes and the gate covers every script, not just apps.
	if ok := f.listApps(nil); ok.Status != 200 {
		t.Fatalf("registered JWT on /v1/apps -> %d: %v", ok.Status, ok.Body)
	}
	ascErrEnvelope(t, f.call("misc", "on_list_users", "GET", "/v1/users", nil, nil, nil, "Bearer "+stranger), 401, "NOT_AUTHORIZED")
	ascErrEnvelope(t, f.call("versions", "on_get_version", "GET", "/v1/appStoreVersions/av_x",
		map[string]string{"id": "av_x"}, nil, nil, ""), 401, "NOT_AUTHORIZED")
}

// TestAppStoreConnectVMAppsSurface: the seeded app, app create with its
// bundleId dedupe, the bracketed filter[...]/sort/fields[apps] list
// conventions, appPrices, and PATCH with its bundleId-collision guard.
func TestAppStoreConnectVMAppsSurface(t *testing.T) {
	f := newAscFixture(t, time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC))

	// ===== the seeded app reads back through list and single-get, with 404 NOT_FOUND for unknown ids =====
	apps := ascEntityList(t, f.listApps(nil))
	if len(apps) != 1 {
		t.Fatalf("seeded list = %d apps, want exactly the one default app", len(apps))
	}
	seededApp := apps[0]
	seededID, _ := seededApp["id"].(string)
	if seededApp["type"] != "apps" || seededID != "app_1500000001" {
		t.Fatalf("seeded entity = %v (id is 1_500_000_000 + seq 1, assembled arithmetically)", seededApp)
	}
	attrs := ascAttr(t, seededApp)
	if attrs["bundleId"] != "com.example.mockapp" || attrs["name"] != "Mock App" ||
		attrs["sku"] != "MOCK_SKU_001" || attrs["primaryLocale"] != "en-US" {
		t.Fatalf("seeded attributes = %v", attrs)
	}
	if links := ascLinks(t, f.listApps(nil)); links["self"] != "/v1/apps" {
		t.Fatalf("list links.self = %v", links)
	}
	if links, _ := seededApp["links"].(map[string]any); links["self"] != "/v1/apps/"+seededID {
		t.Fatalf("seeded links.self = %v, want /v1/apps/%s", links, seededID)
	}
	got := f.asc("apps", "on_get_app", "GET", "/v1/apps/"+seededID, map[string]string{"id": seededID}, nil, nil)
	if got.Status != 200 || ascData(t, got)["id"] != seededID {
		t.Fatalf("get app -> %d %v", got.Status, got.Body)
	}
	nf := ascErrEnvelope(t, f.asc("apps", "on_get_app", "GET", "/v1/apps/app_nope",
		map[string]string{"id": "app_nope"}, nil, nil), 404, "NOT_FOUND")
	if d, _ := nf["detail"].(string); !strings.Contains(d, "app_nope") {
		t.Fatalf("404 detail = %q, want it to name the missing id", d)
	}

	// ===== create assigns numeric ids, persists into the list, and dedupes bundleId =====
	betaID := f.createApp("Beta App", "com.example.beta", nil)
	if betaID != "app_1500000002" {
		t.Fatalf("created id = %q, want app_1500000002 (the second sequence number)", betaID)
	}
	dup := ascErrEnvelope(t, f.asc("apps", "on_create_app", "POST", "/v1/apps", nil, nil, map[string]any{
		"data": map[string]any{"attributes": map[string]any{"name": "Clash", "bundleId": "com.example.beta"}},
	}), 409, "ENTITY_ERROR.ATTRIBUTE.INVALID")
	if d, _ := dup["detail"].(string); !strings.Contains(d, "com.example.beta") {
		t.Fatalf("duplicate-bundleId detail = %q, want the colliding bundleId", d)
	}
	// name and bundleId are both required.
	ascErrEnvelope(t, f.asc("apps", "on_create_app", "POST", "/v1/apps", nil, nil, map[string]any{
		"data": map[string]any{"attributes": map[string]any{"name": "No Bundle"}},
	}), 409, "ENTITY_ERROR.ATTRIBUTE.REQUIRED")
	if apps := ascEntityList(t, f.listApps(nil)); len(apps) != 2 {
		t.Fatalf("list after create = %d apps, want 2 (stateful)", len(apps))
	}

	// ===== bracketed filter[...], leading-dash sort, and fields[apps] projections apply before paging =====
	f.createApp("Alpha App", "com.example.alpha", nil)
	if got := ascEntityList(t, f.listApps(map[string]string{"filter[bundleId]": "com.example.beta"})); len(got) != 1 || got[0]["id"] != betaID {
		t.Fatalf("filter[bundleId] = %v, want only Beta", got)
	}
	// A filter with no matches is 200 with an empty data array, total 0.
	none := f.listApps(map[string]string{"filter[name]": "nope"})
	if none.Status != 200 || len(ascEntityList(t, none)) != 0 {
		t.Fatalf("filter with no match -> %d %v", none.Status, none.Body)
	}
	ascNum(t, ascPaging(t, none)["total"], 0, "meta.paging.total for an empty filter")
	// sort=name asc, sort=-name desc.
	names := func(r starlark.Response) []string {
		t.Helper()
		out := []string{}
		for _, e := range ascEntityList(t, r) {
			n, _ := ascAttr(t, e)["name"].(string)
			out = append(out, n)
		}
		return out
	}
	if got := names(f.listApps(map[string]string{"sort": "name"})); strings.Join(got, ",") != "Alpha App,Beta App,Mock App" {
		t.Fatalf("sort=name = %v", got)
	}
	if got := names(f.listApps(map[string]string{"sort": "-name"})); strings.Join(got, ",") != "Mock App,Beta App,Alpha App" {
		t.Fatalf("sort=-name = %v", got)
	}
	// fields[apps]=name,bundleId keeps id/type/links and only those attributes.
	proj := ascEntityList(t, f.listApps(map[string]string{"fields[apps]": "name,bundleId"}))
	if len(proj) != 3 {
		t.Fatalf("fields projection = %d entities, want 3", len(proj))
	}
	pa := ascAttr(t, proj[0])
	if len(pa) != 2 || pa["name"] == nil || pa["bundleId"] == nil {
		t.Fatalf("fields[apps] projection = %v, want only name+bundleId", pa)
	}
	if proj[0]["type"] != "apps" || proj[0]["links"] == nil {
		t.Fatalf("projection dropped type/links: %v", proj[0])
	}
	// meta.paging.total counts the filtered set BEFORE slicing.
	limited := f.listApps(map[string]string{"limit": "1"})
	ascNum(t, ascPaging(t, limited)["total"], 3, "meta.paging.total with limit=1")
	// The appPrices surface answers one tier-0 price with an app linkage.
	prices := ascEntityList(t, f.asc("apps", "on_list_app_prices", "GET", "/v1/apps/"+betaID+"/appPrices",
		map[string]string{"id": betaID}, nil, nil))
	if len(prices) != 1 || prices[0]["type"] != "appPrices" {
		t.Fatalf("appPrices = %v", prices)
	}
	if ascRelID(t, prices[0], "app") != betaID {
		t.Fatalf("appPrices app linkage = %v", prices[0]["relationships"])
	}

	// ===== PATCH /v1/apps/{id} renames and refuses bundleId collisions =====
	patched := f.asc("apps", "on_update_app", "PATCH", "/v1/apps/"+betaID,
		map[string]string{"id": betaID}, nil, map[string]any{
			"data": map[string]any{"attributes": map[string]any{"name": "Beta Two", "sku": "SKU_B2"}},
		})
	if patched.Status != 200 || ascAttr(t, ascData(t, patched))["name"] != "Beta Two" {
		t.Fatalf("patch app -> %d %v", patched.Status, patched.Body)
	}
	reread := f.asc("apps", "on_get_app", "GET", "/v1/apps/"+betaID, map[string]string{"id": betaID}, nil, nil)
	if ascAttr(t, ascData(t, reread))["sku"] != "SKU_B2" {
		t.Fatalf("patched app not persisted: %v", reread.Body)
	}
	// Renaming onto the seeded app's bundleId is a 409, like create.
	ascErrEnvelope(t, f.asc("apps", "on_update_app", "PATCH", "/v1/apps/"+betaID,
		map[string]string{"id": betaID}, nil, map[string]any{
			"data": map[string]any{"attributes": map[string]any{"bundleId": "com.example.mockapp"}},
		}), 409, "ENTITY_ERROR.ATTRIBUTE.INVALID")
	// PATCH on an unknown app is 404, not 409.
	ascErrEnvelope(t, f.asc("apps", "on_update_app", "PATCH", "/v1/apps/app_nope",
		map[string]string{"id": "app_nope"}, nil, map[string]any{
			"data": map[string]any{"attributes": map[string]any{"name": "X"}},
		}), 404, "NOT_FOUND")
}

// TestAppStoreConnectVMPagination: ASC cursor paging over the apps list —
// meta.paging plus a links.next cursor, a walk that covers every app exactly
// once, the 50/200 limit bounds, and cursor edge cases.
func TestAppStoreConnectVMPagination(t *testing.T) {
	f := newAscFixture(t, time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC))
	f.listApps(nil) // seed
	f.createApp("Beta App", "com.example.beta", nil)
	f.createApp("Alpha App", "com.example.alpha", nil)

	// ===== limit/cursor paging surfaces meta.paging and a links.next cursor =====
	full := ascEntityList(t, f.listApps(nil)) // insertion order
	p1 := f.listApps(map[string]string{"limit": "1"})
	page1 := ascEntityList(t, p1)
	if len(page1) != 1 || page1[0]["id"] != full[0]["id"] {
		t.Fatalf("page 1 = %v, want only %v", page1, full[0]["id"])
	}
	paging := ascPaging(t, p1)
	ascNum(t, paging["total"], 3, "meta.paging.total")
	ascNum(t, paging["limit"], 1, "meta.paging.limit")
	if nc, _ := paging["next_cursor"].(string); nc != "1" {
		t.Fatalf("meta.paging.next_cursor = %v, want \"1\"", paging["next_cursor"])
	}
	links := ascLinks(t, p1)
	if links["self"] != "/v1/apps" {
		t.Fatalf("links.self = %v", links["self"])
	}
	// links.next round-trips the caller's limit alongside the cursor, so
	// following it mid-walk keeps the page size.
	if links["next"] != "/v1/apps?cursor=1&limit=1" {
		t.Fatalf("links.next = %v, want /v1/apps?cursor=1&limit=1", links["next"])
	}
	// Walking the cursors covers every app exactly once.
	seen := map[string]int{page1[0]["id"].(string): 1}
	for cursor := "1"; ; {
		resp := f.listApps(map[string]string{"limit": "1", "cursor": cursor})
		entities := ascEntityList(t, resp)
		if len(entities) != 1 {
			t.Fatalf("cursor %s page = %d entities, want 1", cursor, len(entities))
		}
		seen[entities[0]["id"].(string)]++
		nc, _ := ascPaging(t, resp)["next_cursor"].(string)
		if nc == "" {
			break
		}
		cursor = nc
	}
	if len(seen) != 3 {
		t.Fatalf("cursor walk covered %d distinct apps, want all 3", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("app %s appeared %d times across pages, want exactly once", id, n)
		}
	}

	// ===== limit defaults to 50 and clamps at the documented maximum 200 =====
	ascNum(t, ascPaging(t, f.listApps(nil))["limit"], 50, "default meta.paging.limit")
	capped := f.listApps(map[string]string{"limit": "500"})
	ascNum(t, ascPaging(t, capped)["limit"], 200, "clamped meta.paging.limit")
	if len(ascEntityList(t, capped)) != 3 {
		t.Fatalf("limit=500 page = %d apps, want all 3 in one page", len(ascEntityList(t, capped)))
	}
	if _, has := ascPaging(t, capped)["next_cursor"]; has {
		t.Fatal("single page still advertises a next cursor")
	}

	// ===== a malformed cursor is total: 200 with data null (Apple answers 400 — deviation) =====
	bad := f.listApps(map[string]string{"limit": "1", "cursor": "abc"})
	if bad.Status != 200 {
		t.Fatalf("malformed cursor -> %d, want 200 (paginate is total; documented deviation from Apple's 400)", bad.Status)
	}
	if bad.Body["data"] != nil {
		t.Fatalf("malformed cursor data = %T(%v), want null", bad.Body["data"], bad.Body["data"])
	}
	// A cursor past the end is a valid empty last page.
	past := f.listApps(map[string]string{"limit": "1", "cursor": "99"})
	if past.Status != 200 || len(ascEntityList(t, past)) != 0 {
		t.Fatalf("cursor past end -> %d %v, want 200 with an empty page", past.Status, past.Body)
	}
	if _, has := ascPaging(t, past)["next_cursor"]; has {
		t.Fatal("empty last page still advertises next_cursor")
	}
}

// TestAppStoreConnectVMVersionLifecycle: the appStoreVersion review machine —
// create validation, build adoption, editable-state PATCH, the submission
// clock walk to READY_FOR_SALE / REJECTED, and resubmission after rejection.
func TestAppStoreConnectVMVersionLifecycle(t *testing.T) {
	f := newAscFixture(t, time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC))
	f.listApps(nil) // seed the world; the app below stays separate
	appID := f.createApp("Lifecycle App", "com.example.lifecycle", nil)

	// ===== versionString is required and duplicate versionStrings collide per app =====
	ascErrEnvelope(t, f.asc("versions", "on_create_version", "POST", "/v1/apps/"+appID+"/appStoreVersions",
		map[string]string{"id": appID}, nil, map[string]any{
			"data": map[string]any{"type": "appStoreVersions", "attributes": map[string]any{"platform": "IOS"}},
		}), 409, "ENTITY_ERROR.ATTRIBUTE.REQUIRED")
	ascErrEnvelope(t, f.asc("versions", "on_create_version", "POST", "/v1/apps/app_nope/appStoreVersions",
		map[string]string{"id": "app_nope"}, nil, map[string]any{
			"data": map[string]any{"attributes": map[string]any{"versionString": "1.0"}},
		}), 404, "NOT_FOUND")

	created := f.asc("versions", "on_create_version", "POST", "/v1/apps/"+appID+"/appStoreVersions",
		map[string]string{"id": appID}, nil, map[string]any{
			"data": map[string]any{"type": "appStoreVersions",
				"attributes": map[string]any{"versionString": "2.0.0", "releaseType": "MANUAL"}},
		})
	if created.Status != 201 {
		t.Fatalf("create version -> %d: %v", created.Status, created.Body)
	}
	vent := ascData(t, created)
	versionID, _ := vent["id"].(string)
	if versionID == "" || vent["type"] != "appStoreVersions" {
		t.Fatalf("created version = %v", vent)
	}
	va := ascAttr(t, vent)
	if va["appStoreState"] != "PREPARE_FOR_SUBMISSION" || va["versionString"] != "2.0.0" ||
		va["releaseType"] != "MANUAL" || va["platform"] != "IOS" {
		t.Fatalf("created version attributes = %v", va)
	}
	if got := ascRelID(t, vent, "app"); got != appID {
		t.Fatalf("version app linkage = %q, want %q", got, appID)
	}
	ascErrEnvelope(t, f.asc("versions", "on_create_version", "POST", "/v1/apps/"+appID+"/appStoreVersions",
		map[string]string{"id": appID}, nil, map[string]any{
			"data": map[string]any{"attributes": map[string]any{"versionString": "2.0.0"}},
		}), 409, "ENTITY_ERROR.ATTRIBUTE.INVALID")

	// ===== the app's first version adopts its unattached build =====
	vb := ascEntityList(t, f.asc("versions", "on_list_version_builds", "GET",
		"/v1/appStoreVersions/"+versionID+"/builds", map[string]string{"id": versionID}, nil, nil))
	if len(vb) != 1 {
		t.Fatalf("version builds = %d, want the app's single build adopted", len(vb))
	}
	if got := ascRelID(t, vb[0], "appStoreVersion"); got != versionID {
		t.Fatalf("adopted build appStoreVersion linkage = %q, want %q", got, versionID)
	}

	// ===== PATCH works only in the editable states (PREPARE_FOR_SUBMISSION, REJECTED) =====
	patched := f.asc("versions", "on_update_version", "PATCH", "/v1/appStoreVersions/"+versionID,
		map[string]string{"id": versionID}, nil, map[string]any{
			"data": map[string]any{"attributes": map[string]any{"versionString": "2.0.1", "usesIdfa": true}},
		})
	if patched.Status != 200 {
		t.Fatalf("patch prepared version -> %d: %v", patched.Status, patched.Body)
	}
	if pa := ascAttr(t, ascData(t, patched)); pa["versionString"] != "2.0.1" || pa["usesIdfa"] != true {
		t.Fatalf("patched version attributes = %v", pa)
	}

	// ===== submission drives WAITING_FOR_REVIEW → IN_REVIEW → READY_FOR_SALE on the clock =====
	sub := f.submitVersion(versionID)
	if sub.Status != 201 {
		t.Fatalf("submit -> %d: %v", sub.Status, sub.Body)
	}
	sd := ascData(t, sub)
	if sd["type"] != "appStoreVersionSubmissions" {
		t.Fatalf("submission type = %v", sd["type"])
	}
	if got := ascRelID(t, sd, "appStoreVersion"); got != versionID {
		t.Fatalf("submission linkage = %q, want %q", got, versionID)
	}
	// A submission without the appStoreVersion relationship is a 409.
	ascErrEnvelope(t, f.asc("versions", "on_create_version_submission", "POST", "/v1/appStoreVersionSubmissions",
		nil, nil, map[string]any{"data": map[string]any{}}), 409, "ENTITY_ERROR.RELATIONSHIP.REQUIRED")

	if s := f.versionState(versionID); s != "WAITING_FOR_REVIEW" {
		t.Fatalf("state right after submit = %q, want WAITING_FOR_REVIEW", s)
	}
	// While in review the version is locked for both edits and resubmission.
	ascErrEnvelope(t, f.submitVersion(versionID), 409, "OPERATION_NOT_ALLOWED")
	ascErrEnvelope(t, f.asc("versions", "on_update_version", "PATCH", "/v1/appStoreVersions/"+versionID,
		map[string]string{"id": versionID}, nil, map[string]any{
			"data": map[string]any{"attributes": map[string]any{"versionString": "2.0.2"}},
		}), 409, "OPERATION_NOT_ALLOWED")

	f.vc.Advance(2 * time.Second) // past the +1s review hop, before the +3s decision
	if s := f.versionState(versionID); s != "IN_REVIEW" {
		t.Fatalf("state at +2s = %q, want IN_REVIEW", s)
	}
	f.vc.Advance(2 * time.Second) // past the decision
	if s := f.versionState(versionID); s != "READY_FOR_SALE" {
		t.Fatalf("state at +4s = %q, want READY_FOR_SALE", s)
	}
	// The list surface and its filter agree with the derived state.
	ready := ascEntityList(t, f.asc("versions", "on_list_app_versions", "GET",
		"/v1/apps/"+appID+"/appStoreVersions", map[string]string{"id": appID},
		map[string]string{"filter[appStoreState]": "READY_FOR_SALE"}, nil))
	if len(ready) != 1 || ready[0]["id"] != versionID {
		t.Fatalf("filter[appStoreState]=READY_FOR_SALE = %v, want the released 2.0.1", ready)
	}

	// ===== simulate_fail rejects, and a REJECTED version stays editable and resubmittable =====
	failCreated := f.asc("versions", "on_create_version", "POST", "/v1/apps/"+appID+"/appStoreVersions",
		map[string]string{"id": appID}, nil, map[string]any{
			"data": map[string]any{"attributes": map[string]any{"versionString": "3.0.0", "simulate_fail": true}},
		})
	if failCreated.Status != 201 {
		t.Fatalf("create fail version -> %d: %v", failCreated.Status, failCreated.Body)
	}
	failID, _ := ascData(t, failCreated)["id"].(string)
	if r := f.submitVersion(failID); r.Status != 201 {
		t.Fatalf("submit fail version -> %d: %v", r.Status, r.Body)
	}
	f.vc.Advance(4 * time.Second)
	if s := f.versionState(failID); s != "REJECTED" {
		t.Fatalf("simulate_fail state after decision = %q, want REJECTED", s)
	}
	// PATCH a REJECTED version (the resubmission edit) is allowed.
	if r := f.asc("versions", "on_update_version", "PATCH", "/v1/appStoreVersions/"+failID,
		map[string]string{"id": failID}, nil, map[string]any{
			"data": map[string]any{"attributes": map[string]any{"versionString": "3.0.1"}},
		}); r.Status != 200 {
		t.Fatalf("patch rejected version -> %d: %v", r.Status, r.Body)
	}
	// Resubmission restarts the clock at WAITING_FOR_REVIEW.
	if r := f.submitVersion(failID); r.Status != 201 {
		t.Fatalf("resubmit rejected version -> %d: %v", r.Status, r.Body)
	}
	if s := f.versionState(failID); s != "WAITING_FOR_REVIEW" {
		t.Fatalf("state after resubmit = %q, want WAITING_FOR_REVIEW (fresh schedule)", s)
	}
	ascErrEnvelope(t, f.asc("versions", "on_get_version", "GET", "/v1/appStoreVersions/av_nope",
		map[string]string{"id": "av_nope"}, nil, nil), 404, "NOT_FOUND")
}

// TestAppStoreConnectVMBuildLifecycle: the derive-on-read processingState
// machine — PROCESSING until the 3s window, then VALID (INVALID with the
// simulator-only simulate_fail app attribute) — with the filter and the
// single-build read agreeing.
func TestAppStoreConnectVMBuildLifecycle(t *testing.T) {
	f := newAscFixture(t, time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC))
	f.listApps(nil) // seed; the seeded app's build clock starts here too

	// ===== a fresh app's build reads PROCESSING and the processingState filter agrees =====
	appID := f.createApp("Processing App", "com.example.processing", nil)
	builds := ascEntityList(t, f.builds(appID, nil))
	if len(builds) != 1 {
		t.Fatalf("fresh app builds = %d, want exactly one", len(builds))
	}
	b := builds[0]
	buildID, _ := b["id"].(string)
	if !strings.HasPrefix(buildID, "bld_"+appID) {
		t.Fatalf("build id = %q, want bld_%s_*", buildID, appID)
	}
	ba := ascAttr(t, b)
	if ba["processingState"] != "PROCESSING" || ba["version"] != "1" || ba["usesNonExemptEncryption"] != false {
		t.Fatalf("build attributes = %v", ba)
	}
	if ud, _ := ba["uploadedDate"].(string); ud == "" {
		t.Fatal("uploadedDate is empty")
	}
	if got := ascRelID(t, b, "app"); got != appID {
		t.Fatalf("build app linkage = %q, want %q", got, appID)
	}
	if got := ascEntityList(t, f.builds(appID, map[string]string{"filter[processingState]": "PROCESSING"})); len(got) != 1 {
		t.Fatalf("filter[processingState]=PROCESSING = %d builds, want 1", len(got))
	}
	if got := ascEntityList(t, f.builds(appID, map[string]string{"filter[processingState]": "VALID"})); len(got) != 0 {
		t.Fatalf("filter[processingState]=VALID = %d builds, want 0", len(got))
	}
	if got := ascEntityList(t, f.builds(appID, map[string]string{"filter[version]": "1"})); len(got) != 1 {
		t.Fatalf("filter[version]=1 = %d builds, want 1", len(got))
	}

	// ===== the 3s window settles VALID and GET /v1/builds/{id} agrees =====
	f.vc.Advance(3 * time.Second)
	if got := ascEntityList(t, f.builds(appID, nil)); len(got) != 1 || ascAttr(t, got[0])["processingState"] != "VALID" {
		t.Fatalf("build after the window = %v, want VALID", got)
	}
	single := f.asc("apps", "on_get_build", "GET", "/v1/builds/"+buildID, map[string]string{"id": buildID}, nil, nil)
	if single.Status != 200 || ascAttr(t, ascData(t, single))["processingState"] != "VALID" {
		t.Fatalf("single build after the window -> %d %v", single.Status, single.Body)
	}
	if got := ascEntityList(t, f.builds(appID, map[string]string{"filter[processingState]": "PROCESSING"})); len(got) != 0 {
		t.Fatalf("filter[processingState]=PROCESSING after settle = %d, want 0", len(got))
	}

	// ===== a simulate_fail app settles its build INVALID =====
	failID := f.createApp("Fail App", "com.example.fail", map[string]any{"simulate_fail": true})
	if got := ascEntityList(t, f.builds(failID, nil)); len(got) != 1 || ascAttr(t, got[0])["processingState"] != "PROCESSING" {
		t.Fatalf("fail build before the window = %v", got)
	}
	f.vc.Advance(3 * time.Second)
	if got := ascEntityList(t, f.builds(failID, nil)); len(got) != 1 || ascAttr(t, got[0])["processingState"] != "INVALID" {
		t.Fatalf("fail build after the window = %v, want INVALID", got)
	}
	ascErrEnvelope(t, f.asc("apps", "on_get_build", "GET", "/v1/builds/bld_nope",
		map[string]string{"id": "bld_nope"}, nil, nil), 404, "NOT_FOUND")
}

// TestAppStoreConnectVMUsersAndSalesReports: the users surface (seeded once,
// bracketed filters, sort, the same paging envelope) and the salesReports
// surface (bracketed filter echoes with DAILY/SALES defaults).
func TestAppStoreConnectVMUsersAndSalesReports(t *testing.T) {
	f := newAscFixture(t, time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC))

	// ===== users seed once and answer the bracketed role/username filters and sort =====
	users := ascEntityList(t, f.asc("misc", "on_list_users", "GET", "/v1/users", nil, nil, nil))
	if len(users) != 2 {
		t.Fatalf("users = %d, want the 2 seeded", len(users))
	}
	byUser := map[string]map[string]any{}
	for _, u := range users {
		if u["type"] != "users" {
			t.Fatalf("user type = %v", u["type"])
		}
		ua := ascAttr(t, u)
		uname, _ := ua["username"].(string)
		byUser[uname] = ua
	}
	if admin := byUser["admin@example.com"]; admin == nil || admin["allAppsVisible"] != true {
		t.Fatalf("admin user = %v", byUser["admin@example.com"])
	}
	dev := byUser["developer@example.com"]
	if dev == nil || dev["allAppsVisible"] != false {
		t.Fatalf("developer user = %v", dev)
	}
	if roles, ok := dev["roles"].([]any); !ok || len(roles) != 1 || roles[0] != "DEVELOPER" {
		t.Fatalf("developer roles = %v, want [DEVELOPER]", dev["roles"])
	}
	// filter[roles] matches any of the comma-separated roles.
	admins := ascEntityList(t, f.asc("misc", "on_list_users", "GET", "/v1/users", nil,
		map[string]string{"filter[roles]": "ADMIN"}, nil))
	if len(admins) != 1 || ascAttr(t, admins[0])["username"] != "admin@example.com" {
		t.Fatalf("filter[roles]=ADMIN = %v", admins)
	}
	if both := ascEntityList(t, f.asc("misc", "on_list_users", "GET", "/v1/users", nil,
		map[string]string{"filter[roles]": "DEVELOPER,ADMIN"}, nil)); len(both) != 2 {
		t.Fatalf("filter[roles]=DEVELOPER,ADMIN = %d users, want both", len(both))
	}
	// filter[username] narrows to one.
	if got := ascEntityList(t, f.asc("misc", "on_list_users", "GET", "/v1/users", nil,
		map[string]string{"filter[username]": "developer@example.com"}, nil)); len(got) != 1 {
		t.Fatalf("filter[username] = %d users, want 1", len(got))
	}
	// sort=-username reverses; the paging envelope matches the apps shape.
	sorted := f.asc("misc", "on_list_users", "GET", "/v1/users", nil,
		map[string]string{"sort": "-username", "limit": "1"}, nil)
	if got := ascEntityList(t, sorted); len(got) != 1 || ascAttr(t, got[0])["username"] != "developer@example.com" {
		t.Fatalf("sort=-username page = %v", got)
	}
	ascNum(t, ascPaging(t, sorted)["total"], 2, "users meta.paging.total")
	if links := ascLinks(t, sorted); links["next"] != "/v1/users?cursor=1&limit=1" {
		t.Fatalf("users links.next = %v", links["next"])
	}

	// ===== salesReports echoes the bracketed report filters with DAILY/SALES defaults =====
	// No query params at all — the engine hands handlers an empty query dict.
	def := f.asc("misc", "on_sales_reports", "GET", "/v1/salesReports", nil, nil, nil)
	if def.Status != 200 {
		t.Fatalf("salesReports without filters -> %d: %v", def.Status, def.Body)
	}
	reports := ascEntityList(t, def)
	if len(reports) != 1 || reports[0]["type"] != "salesReports" {
		t.Fatalf("salesReports = %v", reports)
	}
	rep := ascAttr(t, reports[0])
	if rep["reportType"] != "SALES" || rep["frequency"] != "DAILY" {
		t.Fatalf("default report filters = %v", rep)
	}
	ascNum(t, rep["downloads"], 1234, "downloads")
	ascNum(t, rep["units"], 1234, "units")
	if rep["appleIdentifier"] != "1500000001" {
		t.Fatalf("appleIdentifier = %v, want 1500000001 (1_500_000_000 + 1, no float drift)", rep["appleIdentifier"])
	}
	filtered := f.asc("misc", "on_sales_reports", "GET", "/v1/salesReports", nil, map[string]string{
		"filter[frequency]": "WEEKLY", "filter[reportType]": "SUBSCRIPTION",
	}, nil)
	rep2 := ascAttr(t, ascEntityList(t, filtered)[0])
	if rep2["frequency"] != "WEEKLY" || rep2["reportType"] != "SUBSCRIPTION" {
		t.Fatalf("filtered report = %v, want the filter values echoed", rep2)
	}
}
