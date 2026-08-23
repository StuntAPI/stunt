package adapters

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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

// Drives the braze-style adapter scripts directly (lib.star preloaded) over
// a shared store and a VIRTUAL clock: the app-group API-key gate (Bearer and
// the x-authorization header), /users/track ingestion (external ids and
// aliases, dedup, partial-success errors), /users/export/ids aggregation and
// projection, the alias -> identify merge lifecycle, the messaging fatal
// error vocabulary with real dispatch ids, the derive-on-read scheduled-send
// transition, and the unsigned-by-design outbound webhooks.
const (
	brazeHost = "rest.iad-01.braze.test"
	brazeKey  = "test-app-group-api-key" // seeded into the KV store by lib.star
	brazeAuth = "Bearer " + brazeKey
)

// Real id shapes: dispatch_id is 32-char lowercase hex, braze_id 24-char hex,
// schedule_id UUID-shaped.
var (
	brazeDispatchRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	brazeIDRe       = regexp.MustCompile(`^[0-9a-f]{24}$`)
	brazeScheduleRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// brazeFixture is one shared store + virtual clock with a loaded VM per
// handler script, plus a real local sink capturing the emitter's deliveries
// (the same emitter the engine hands handlers).
type brazeFixture struct {
	t       *testing.T
	vc      *clock.Clock
	vms     map[string]*starlark.VM
	mu      sync.Mutex
	sink    []map[string]any // parsed {type, payload} envelopes
	sinkURL string
}

func newBrazeFixture(t *testing.T, start time.Time) *brazeFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "braze-style")
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

	f := &brazeFixture{t: t, vc: clock.NewVirtualClock(start)}
	em := events.NewEmitter()
	t.Cleanup(em.Close)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var env map[string]any
		json.Unmarshal(b, &env)
		f.mu.Lock()
		f.sink = append(f.sink, env)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sink.Close)
	f.sinkURL = sink.URL

	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: f.vc, ServiceName: "test", Emitter: em,
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
	f.vms = map[string]*starlark.VM{
		"users": load("users.star"), "messages": load("messages.star"),
		"campaigns": load("campaigns.star"), "segments": load("segments.star"),
		"hooks": load("webhooks.star"),
	}
	return f
}

// call invokes a handler on the named script VM. A nil headers map carries
// the fixture's Bearer app-group key; an explicit (even empty) map stands
// for exactly what the client sent.
func (f *brazeFixture) call(group, handler, method, path string, query map[string]string, body map[string]any, headers map[string]string) starlark.Response {
	f.t.Helper()
	if headers == nil {
		headers = map[string]string{"Authorization": brazeAuth}
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: brazeHost, Headers: headers, Body: body, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s (%s): %v", method, path, handler, err)
	}
	return resp
}

// callRaw drives the raw_body path (what the engine does when the request
// body is not decodable JSON).
func (f *brazeFixture) callRaw(group, handler, method, path, raw string) starlark.Response {
	f.t.Helper()
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: brazeHost,
		Headers: map[string]string{"Authorization": brazeAuth}, RawBody: raw,
	})
	if err != nil {
		f.t.Fatalf("%s %s (%s): %v", method, path, handler, err)
	}
	return resp
}

// deliveries returns a copy of the sink's captured envelopes.
func (f *brazeFixture) deliveries() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.sink))
	copy(out, f.sink)
	return out
}

// registerHook registers the fixture's sink as an outbound webhook target.
func (f *brazeFixture) registerHook(eventTypes []any) {
	f.t.Helper()
	r := f.call("hooks", "on_create_webhook", "POST", "/webhooks", nil,
		map[string]any{"url": f.sinkURL, "events": eventTypes}, nil)
	if r.Status != 200 {
		f.t.Fatalf("register webhook -> %d: %v", r.Status, r.Body)
	}
}

// user finds the exported profile carrying externalID.
func brazeUser(t *testing.T, resp starlark.Response, externalID string) map[string]any {
	t.Helper()
	users := brazeUsers(t, resp)
	for _, u := range users {
		if u["external_id"] == externalID {
			return u
		}
	}
	t.Fatalf("no exported user with external_id %q (users: %v)", externalID, users)
	return nil
}

// brazeUsers returns the exported profiles (index 0 is the only way to reach
// an alias-only profile, whose external_id is null).
func brazeUsers(t *testing.T, resp starlark.Response) []map[string]any {
	t.Helper()
	raw, ok := resp.Body["users"].([]any)
	if !ok {
		t.Fatalf("export users = %v, want array", resp.Body["users"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, u := range raw {
		if m, ok := u.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// brazeAttr digs into an exported profile (custom_attributes, user_aliases...).
func brazeAttr(t *testing.T, user map[string]any, field string) map[string]any {
	t.Helper()
	m, ok := user[field].(map[string]any)
	if !ok {
		t.Fatalf("user[%q] = %v (%T), want object", field, user[field], user[field])
	}
	return m
}

func TestBrazeAPIKeyGate(t *testing.T) {
	f := newBrazeFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== a missing credential is the 401 message envelope =====
	// No credential at all -> 401 with Braze's plain message envelope.
	if r := f.call("segments", "on_list_segments", "GET", "/segments/list", nil, nil,
		map[string]string{}); r.Status != 401 || r.Body["message"] != "Unauthorized. A valid API key is required." {
		t.Fatalf("no credential -> %d %v, want 401 unauthorized message", r.Status, r.Body)
	}

	// ===== an unknown app-group key is rejected =====
	// A well-formed Bearer token that is not the seeded key -> 401.
	if r := f.call("segments", "on_list_segments", "GET", "/segments/list", nil, nil,
		map[string]string{"Authorization": "Bearer someone-elses-key"}); r.Status != 401 {
		t.Fatalf("unknown bearer -> %d, want 401", r.Status)
	}

	// ===== x-authorization carries the raw app-group key =====
	// The documented alternate credential: the bare key in x-authorization,
	// no Bearer prefix. A wrong value there is still a 401.
	if r := f.call("segments", "on_list_segments", "GET", "/segments/list", nil, nil,
		map[string]string{"x-authorization": brazeKey}); r.Status != 200 || r.Body["message"] != "success" {
		t.Fatalf("x-authorization key -> %d %v, want 200 success", r.Status, r.Body)
	}
	if r := f.call("segments", "on_list_segments", "GET", "/segments/list", nil, nil,
		map[string]string{"x-authorization": "not-the-key"}); r.Status != 401 {
		t.Fatalf("x-authorization wrong key -> %d, want 401", r.Status)
	}
}

func TestBrazeUsersTrackExport(t *testing.T) {
	f := newBrazeFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	const (
		t1 = "2026-02-03T10:00:00Z"
		t2 = "2026-02-03T11:30:00Z"
	)

	// ===== track ingests attributes, events, and purchases =====
	// One call, all three arrays: reserved fields land at the top level of
	// the profile, everything else in custom_attributes. Numbers arrive as
	// JSON floats (price/quantity) and are coerced by the script.
	track := f.call("users", "on_track", "POST", "/users/track", nil, map[string]any{
		"attributes": []any{map[string]any{
			"external_id": "u1", "first_name": "Ada", "email": "ada@example.com",
			"favorite_color": "blue",
		}},
		"events": []any{map[string]any{
			"external_id": "u1", "name": "played_level", "time": t1,
			"properties": map[string]any{"level": 3},
		}},
		"purchases": []any{map[string]any{
			"external_id": "u1", "product_id": "gold", "currency": "USD",
			"price": 9.99, "quantity": 2.0, "time": t1,
		}},
	}, nil)
	if track.Status != 200 || track.Body["message"] != "success" {
		t.Fatalf("track -> %d %v", track.Status, track.Body)
	}
	if n, ok := track.Body["attributes_processed"].(int64); !ok || n != 1 {
		t.Fatalf("attributes_processed = %v (%T), want 1", track.Body["attributes_processed"], track.Body["attributes_processed"])
	}
	if n, ok := track.Body["events_processed"].(int64); !ok || n != 1 {
		t.Fatalf("events_processed = %v, want 1", track.Body["events_processed"])
	}
	if n, ok := track.Body["purchases_processed"].(int64); !ok || n != 1 {
		t.Fatalf("purchases_processed = %v, want 1", track.Body["purchases_processed"])
	}
	if _, has := track.Body["errors"]; has {
		t.Fatalf("clean track reported errors: %v", track.Body["errors"])
	}

	// ===== re-tracking the same external_id updates one profile =====
	// A second record for u1 overwrites the custom attribute (new values
	// win, like the real API) and never creates a second profile.
	retrack := f.call("users", "on_track", "POST", "/users/track", nil, map[string]any{
		"attributes": []any{map[string]any{"external_id": "u1", "favorite_color": "red"}},
		"events":     []any{map[string]any{"external_id": "u1", "name": "played_level", "time": t2}},
	}, nil)
	if retrack.Status != 200 {
		t.Fatalf("retrack -> %d: %v", retrack.Status, retrack.Body)
	}

	// ===== export aggregates custom events and purchases, dedups ids =====
	// Duplicate external_ids export once; unknown ids land in
	// invalid_user_ids; events fold into {name, first, last, count}.
	exp := f.call("users", "on_export_ids", "POST", "/users/export/ids", nil, map[string]any{
		"external_ids": []any{"u1", "u1", "ghost"},
	}, nil)
	if exp.Status != 200 || exp.Body["message"] != "success" {
		t.Fatalf("export -> %d %v", exp.Status, exp.Body)
	}
	if users := exp.Body["users"].([]any); len(users) != 1 {
		t.Fatalf("export returned %d users, want 1 (dedup)", len(users))
	}
	if inv := exp.Body["invalid_user_ids"].([]any); len(inv) != 1 || inv[0] != "ghost" {
		t.Fatalf("invalid_user_ids = %v, want [ghost]", inv)
	}
	u := brazeUser(t, exp, "u1")
	if u["first_name"] != "Ada" || u["email"] != "ada@example.com" {
		t.Fatalf("reserved fields = %v/%v, want Ada / ada@example.com", u["first_name"], u["email"])
	}
	if ca := brazeAttr(t, u, "custom_attributes"); ca["favorite_color"] != "red" {
		t.Fatalf("favorite_color = %v, want red (last write wins)", ca["favorite_color"])
	}
	if bid, ok := u["braze_id"].(string); !ok || !brazeIDRe.MatchString(bid) {
		t.Fatalf("braze_id = %v, want 24-char hex", u["braze_id"])
	}
	evs := u["custom_events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("custom_events = %v, want one aggregated entry", evs)
	}
	ev := evs[0].(map[string]any)
	if ev["name"] != "played_level" || ev["first"] != t1 || ev["last"] != t2 || ev["count"] != int64(2) {
		t.Fatalf("aggregated event = %v, want played_level t1..t2 count 2", ev)
	}
	pur := u["purchases"].([]any)
	if len(pur) != 1 || pur[0].(map[string]any)["name"] != "gold" || pur[0].(map[string]any)["count"] != int64(1) {
		t.Fatalf("aggregated purchases = %v, want gold count 1", pur)
	}

	// ===== fields_to_export projects the profile =====
	// The endpoint's real default exports everything; an explicit list
	// projects only known fields.
	proj := f.call("users", "on_export_ids", "POST", "/users/export/ids", nil, map[string]any{
		"external_ids":     []any{"u1"},
		"fields_to_export": []any{"external_id", "custom_events"},
	}, nil)
	if proj.Status != 200 {
		t.Fatalf("projected export -> %d: %v", proj.Status, proj.Body)
	}
	pu := brazeUser(t, proj, "u1")
	if len(pu) != 2 {
		t.Fatalf("projected profile has %d fields (%v), want exactly external_id + custom_events", len(pu), pu)
	}

	// ===== per-record errors keep partial success =====
	// Braze's contract: message stays "success", processed counts reflect
	// what ingested, rejected records are listed in errors naming the array
	// and index.
	mixed := f.call("users", "on_track", "POST", "/users/track", nil, map[string]any{
		"attributes": []any{
			map[string]any{"external_id": "bad", "email": "not-an-email"},
			map[string]any{"external_id": "good", "email": "good@example.com"},
		},
		"events": []any{map[string]any{"external_id": "u1", "name": "x", "time": "not-a-date"}},
	}, nil)
	if mixed.Status != 200 || mixed.Body["message"] != "success" {
		t.Fatalf("partial track -> %d %v", mixed.Status, mixed.Body)
	}
	if n, ok := mixed.Body["attributes_processed"].(int64); !ok || n != 1 {
		t.Fatalf("partial attributes_processed = %v, want 1", mixed.Body["attributes_processed"])
	}
	errs, ok := mixed.Body["errors"].([]any)
	if !ok || len(errs) != 2 {
		t.Fatalf("partial errors = %v, want 2 entries", mixed.Body["errors"])
	}
	e0 := errs[0].(map[string]any)
	if _, ok := e0["EMAIL_BAD_FORMAT"]; !ok || !strings.Contains(e0["EMAIL_BAD_FORMAT"].(string), "attributes[0]") {
		t.Fatalf("errors[0] = %v, want EMAIL_BAD_FORMAT naming attributes[0]", e0)
	}
	e1 := errs[1].(map[string]any)
	if _, ok := e1["BAD_REQUEST"]; !ok || !strings.Contains(e1["BAD_REQUEST"].(string), "events[0]") {
		t.Fatalf("errors[1] = %v, want BAD_REQUEST naming events[0]", e1)
	}

	// ===== _update_existing_only never creates =====
	// Update-only records targeting unknown users are skipped silently —
	// no error, no profile.
	uo := f.call("users", "on_track", "POST", "/users/track", nil, map[string]any{
		"attributes": []any{map[string]any{
			"external_id": "ghost2", "_update_existing_only": true, "first_name": "Nope",
		}},
	}, nil)
	if uo.Status != 200 {
		t.Fatalf("update-only -> %d %v", uo.Status, uo.Body)
	}
	if n, ok := uo.Body["attributes_processed"].(int64); !ok || n != 0 {
		t.Fatalf("update-only attributes_processed = %v, want 0", uo.Body["attributes_processed"])
	}
	if _, has := uo.Body["errors"]; has {
		t.Fatalf("update-only reported errors: %v", uo.Body["errors"])
	}
	gone := f.call("users", "on_export_ids", "POST", "/users/export/ids", nil, map[string]any{
		"external_ids": []any{"ghost2"},
	}, nil)
	if u := gone.Body["users"].([]any); len(u) != 0 {
		t.Fatalf("update-only created a profile: %v", u)
	}

	// ===== fatal envelopes: the 75-id cap and undecodable JSON =====
	// More than 75 distinct external ids in one call is the documented
	// whole-request fatal; an undecodable body is the Bad Request fatal.
	many := make([]any, 0, 76)
	for i := 0; i < 76; i++ {
		many = append(many, map[string]any{"external_id": "bulk-" + string(rune('a'+i%26)) + "-" + string(rune('a'+i/26))})
	}
	over := f.call("users", "on_track", "POST", "/users/track", nil, map[string]any{
		"attributes": many,
	}, nil)
	if over.Status != 400 || over.Body["message"] != "Max Input Length Exceeded" {
		t.Fatalf("76-id track -> %d %v, want 400 Max Input Length Exceeded", over.Status, over.Body)
	}
	bad := f.callRaw("users", "on_track", "POST", "/users/track", "{not json")
	if bad.Status != 400 || bad.Body["message"] != "Bad Request" {
		t.Fatalf("bad body -> %d %v, want 400 Bad Request", bad.Status, bad.Body)
	}
	if e := bad.Body["errors"].([]any)[0].(map[string]any); !strings.Contains(e["Bad Request"].(string), "Bad syntax") {
		t.Fatalf("bad body errors = %v, want Bad syntax detail", e)
	}
}

func TestBrazeAliasIdentify(t *testing.T) {
	f := newBrazeFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	const evTime = "2026-02-03T10:00:00Z"
	alias := func(label, name string) map[string]any {
		return map[string]any{"alias_label": label, "alias_name": name}
	}
	exportAliases := func(aliases ...map[string]any) starlark.Response {
		list := make([]any, len(aliases))
		for i, a := range aliases {
			list[i] = a
		}
		return f.call("users", "on_export_ids", "POST", "/users/export/ids", nil,
			map[string]any{"user_aliases": list}, nil)
	}

	// ===== alias/new creates an alias-only profile track can target =====
	// Without an external_id the alias becomes its own (anonymous) profile;
	// /users/track against user_alias writes through it. Re-adding the same
	// alias is a no-op success, not a duplicate.
	an := f.call("users", "on_alias_new", "POST", "/users/alias/new", nil, map[string]any{
		"user_aliases": []any{alias("support", "case-1")},
	}, nil)
	if an.Status != 200 || an.Body["message"] != "success" {
		t.Fatalf("alias/new -> %d %v", an.Status, an.Body)
	}
	if n, ok := an.Body["aliases_processed"].(int64); !ok || n != 1 {
		t.Fatalf("alias/new aliases_processed = %v, want 1", an.Body["aliases_processed"])
	}
	tr := f.call("users", "on_track", "POST", "/users/track", nil, map[string]any{
		"attributes": []any{map[string]any{
			"user_alias": alias("support", "case-1"), "first_name": "Ann", "tier": "free",
		}},
	}, nil)
	if tr.Status != 200 {
		t.Fatalf("track by alias -> %d %v", tr.Status, tr.Body)
	}
	if n, ok := tr.Body["attributes_processed"].(int64); !ok || n != 1 {
		t.Fatalf("track by alias attributes_processed = %v, want 1", tr.Body["attributes_processed"])
	}
	exp := exportAliases(alias("support", "case-1"))
	if users := brazeUsers(t, exp); len(users) != 1 {
		t.Fatalf("alias export returned %d users, want the one alias-only profile", len(users))
	}
	anon := brazeUsers(t, exp)[0]
	if anon["first_name"] != "Ann" {
		t.Fatalf("alias-only first_name = %v, want Ann", anon["first_name"])
	}
	if ca := brazeAttr(t, anon, "custom_attributes"); ca["tier"] != "free" {
		t.Fatalf("alias-only custom_attributes = %v, want tier free", ca)
	}
	if als := anon["user_aliases"].([]any); len(als) != 1 {
		t.Fatalf("alias-only user_aliases = %v, want the one alias", als)
	}
	if again := f.call("users", "on_alias_new", "POST", "/users/alias/new", nil, map[string]any{
		"user_aliases": []any{alias("support", "case-1")},
	}, nil); again.Status != 200 {
		t.Fatalf("re-add alias -> %d: %v", again.Status, again.Body)
	}
	if users := brazeUsers(t, exportAliases(alias("support", "case-1"))); len(users) != 1 {
		t.Fatalf("re-added alias produced %d profiles, want 1", len(users))
	}

	// ===== identify re-keys an alias-only profile when the external_id is new =====
	// No user carries the external_id: the alias-only profile becomes the
	// identified user and its tracked events follow the re-key.
	f.call("users", "on_alias_new", "POST", "/users/alias/new", nil, map[string]any{
		"user_aliases": []any{alias("promo", "p-1")},
	}, nil)
	f.call("users", "on_track", "POST", "/users/track", nil, map[string]any{
		"events": []any{map[string]any{"user_alias": alias("promo", "p-1"), "name": "signed_up", "time": evTime}},
	}, nil)
	idn := f.call("users", "on_identify", "POST", "/users/identify", nil, map[string]any{
		"aliases_to_identify": []any{map[string]any{
			"external_id": "id-100", "user_alias": alias("promo", "p-1"),
		}},
	}, nil)
	if idn.Status != 200 {
		t.Fatalf("identify (re-key) -> %d %v", idn.Status, idn.Body)
	}
	if n, ok := idn.Body["aliases_processed"].(int64); !ok || n != 1 {
		t.Fatalf("identify (re-key) aliases_processed = %v, want 1", idn.Body["aliases_processed"])
	}
	exp100 := f.call("users", "on_export_ids", "POST", "/users/export/ids", nil,
		map[string]any{"external_ids": []any{"id-100"}}, nil)
	u100 := brazeUser(t, exp100, "id-100")
	if evs := u100["custom_events"].([]any); len(evs) != 1 || evs[0].(map[string]any)["name"] != "signed_up" {
		t.Fatalf("re-keyed events = %v, want signed_up on id-100", evs)
	}

	// ===== identify merges an alias-only profile into an existing one =====
	// Both sides exist: aliases, custom attributes (without overwrite), and
	// events/purchases move to the identified profile; the alias still
	// resolves — to the merged user now.
	f.call("users", "on_track", "POST", "/users/track", nil, map[string]any{
		"attributes": []any{map[string]any{"external_id": "id-200", "first_name": "Bob", "tier": "gold"}},
	}, nil)
	f.call("users", "on_alias_new", "POST", "/users/alias/new", nil, map[string]any{
		"user_aliases": []any{alias("web", "w-1")},
	}, nil)
	f.call("users", "on_track", "POST", "/users/track", nil, map[string]any{
		"attributes": []any{map[string]any{"user_alias": alias("web", "w-1"), "first_name": "Ann", "source": "web"}},
		"events":     []any{map[string]any{"user_alias": alias("web", "w-1"), "name": "browsed", "time": evTime}},
	}, nil)
	mrg := f.call("users", "on_identify", "POST", "/users/identify", nil, map[string]any{
		"aliases_to_identify": []any{map[string]any{
			"external_id": "id-200", "user_alias": alias("web", "w-1"),
		}},
	}, nil)
	if mrg.Status != 200 {
		t.Fatalf("identify (merge) -> %d: %v", mrg.Status, mrg.Body)
	}
	exp200 := f.call("users", "on_export_ids", "POST", "/users/export/ids", nil,
		map[string]any{"external_ids": []any{"id-200"}}, nil)
	u200 := brazeUser(t, exp200, "id-200")
	if u200["first_name"] != "Bob" {
		t.Fatalf("merged first_name = %v, want Bob (target wins)", u200["first_name"])
	}
	ca := brazeAttr(t, u200, "custom_attributes")
	if ca["tier"] != "gold" || ca["source"] != "web" {
		t.Fatalf("merged custom_attributes = %v, want tier gold + source web", ca)
	}
	if evs := u200["custom_events"].([]any); len(evs) != 1 || evs[0].(map[string]any)["name"] != "browsed" {
		t.Fatalf("merged events = %v, want browsed on id-200", evs)
	}
	// The alias now resolves to the merged profile.
	if users := brazeUsers(t, exportAliases(alias("web", "w-1"))); len(users) != 1 {
		t.Fatalf("alias after merge -> %d users, want the merged one", len(users))
	}
}

func TestBrazeMessaging(t *testing.T) {
	f := newBrazeFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== send fatals: message, variant, and recipient rules =====
	// The documented fatal-error vocabulary, each as {message, errors}.
	fatal := func(body map[string]any, want string) {
		t.Helper()
		r := f.call("messages", "on_send", "POST", "/messages/send", nil, body, nil)
		if r.Status != 400 || r.Body["message"] != want {
			t.Fatalf("send %v -> %d %v, want 400 %q", body["campaign_id"], r.Status, r.Body, want)
		}
	}
	fatal(map[string]any{"external_user_ids": []any{"u1"}}, "No message to send")
	fatal(map[string]any{
		"campaign_id": "cmp001", "external_user_ids": []any{"u1"},
		"messages": map[string]any{"email": map[string]any{}},
	}, "Message Variant Unspecified")
	fatal(map[string]any{
		"campaign_id": "cmp001", "external_user_ids": []any{"u1"},
		"messages": map[string]any{"email": map[string]any{"message_variation_id": "variant-9"}},
	}, "Invalid Message Variant")
	fatal(map[string]any{
		"campaign_id": "cmp999", "external_user_ids": []any{"u1"},
		"messages": map[string]any{"email": map[string]any{}},
	}, "Invalid Campaign ID")
	fatal(map[string]any{
		"broadcast": true, "external_user_ids": []any{"u1"},
		"messages": map[string]any{"email": map[string]any{}},
	}, "Bad Request")
	fatal(map[string]any{"messages": map[string]any{"email": map[string]any{}}}, "No Recipients")

	// ===== send succeeds with a 32-hex dispatch id and emits message.sent =====
	// The dispatch is recorded and the unsigned-by-design webhook fires with
	// the dispatch id, channels, and recipient count. The hook's events list
	// subscribes to both send surfaces.
	f.registerHook([]any{"message.sent", "campaign.sent"})
	send := f.call("messages", "on_send", "POST", "/messages/send", nil, map[string]any{
		"external_user_ids": []any{"u1", "u2"},
		"messages":          map[string]any{"email": map[string]any{"from": "hi@shop.test"}},
		"send_id":           "s-1",
	}, nil)
	if send.Status != 200 || send.Body["message"] != "success" {
		t.Fatalf("send -> %d %v", send.Status, send.Body)
	}
	did, ok := send.Body["dispatch_id"].(string)
	if !ok || !brazeDispatchRe.MatchString(did) {
		t.Fatalf("dispatch_id = %v, want 32-char lowercase hex", send.Body["dispatch_id"])
	}
	if send.Body["send_id"] != "s-1" {
		t.Fatalf("send_id echo = %v, want s-1", send.Body["send_id"])
	}
	delivered := f.deliveries()
	if len(delivered) != 1 {
		t.Fatalf("sink got %d deliveries, want 1: %+v", len(delivered), delivered)
	}
	env := delivered[0]
	if env["type"] != "message.sent" {
		t.Fatalf("webhook type = %v, want message.sent", env["type"])
	}
	pl, _ := env["payload"].(map[string]any)
	// Sink numbers are JSON-decoded, so they arrive as float64.
	if pl["dispatch_id"] != did || pl["recipients"] != float64(2) || pl["campaign_id"] != nil {
		t.Fatalf("message.sent payload = %v, want dispatch %s / 2 recipients / no campaign", pl, did)
	}
	if ch := pl["channels"].([]any); len(ch) != 1 || ch[0] != "email" {
		t.Fatalf("payload channels = %v, want [email]", pl["channels"])
	}

	// ===== campaigns/trigger/send validates the campaign id =====
	// campaign_id is required and must be one of the seeded campaigns.
	bad := f.call("campaigns", "on_trigger_send", "POST", "/campaigns/trigger/send", nil,
		map[string]any{"campaign_id": "cmp999", "external_user_ids": []any{"u1"}}, nil)
	if bad.Status != 400 || bad.Body["message"] != "Invalid Campaign ID" {
		t.Fatalf("trigger unknown campaign -> %d %v", bad.Status, bad.Body)
	}
	trig := f.call("campaigns", "on_trigger_send", "POST", "/campaigns/trigger/send", nil,
		map[string]any{"campaign_id": "cmp001", "external_user_ids": []any{"u1"}}, nil)
	if trig.Status != 200 || trig.Body["message"] != "success" {
		t.Fatalf("trigger cmp001 -> %d %v", trig.Status, trig.Body)
	}
	if did, ok := trig.Body["dispatch_id"].(string); !ok || !brazeDispatchRe.MatchString(did) {
		t.Fatalf("trigger dispatch_id = %v, want 32-char lowercase hex", trig.Body["dispatch_id"])
	}
	// The campaign.sent delivery carries the campaign's seeded channels.
	envs := f.deliveries()
	if len(envs) != 2 || envs[1]["type"] != "campaign.sent" {
		t.Fatalf("sink after trigger = %+v, want a campaign.sent second", envs)
	}
	cpl, _ := envs[1]["payload"].(map[string]any)
	if cpl["campaign_id"] != "cmp001" || cpl["dispatch_id"] != trig.Body["dispatch_id"] {
		t.Fatalf("campaign.sent payload = %v", cpl)
	}
}

func TestBrazeScheduledMessages(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newBrazeFixture(t, base)
	f.registerHook([]any{"message.sent"})
	const (
		sendA  = "2026-02-03T13:00:00Z" // base + 1h
		sendB  = "2026-02-03T14:00:00Z" // base + 2h
		endAll = "2026-02-03T15:00:00Z" // base + 3h
	)
	list := func(end string) []map[string]any {
		t.Helper()
		query := map[string]string{}
		if end != "" {
			query["end_time"] = end
		}
		r := f.call("messages", "on_scheduled", "GET", "/messages/scheduled", query, nil, nil)
		if r.Status != 200 {
			t.Fatalf("scheduled list -> %d: %v", r.Status, r.Body)
		}
		raw, _ := r.Body["scheduled_broadcasts"].([]any)
		out := make([]map[string]any, 0, len(raw))
		for _, b := range raw {
			out = append(out, b.(map[string]any))
		}
		return out
	}

	// ===== schedule/create mints a UUID-shaped schedule id =====
	a := f.call("messages", "on_schedule_create", "POST", "/messages/schedule/create", nil, map[string]any{
		"schedule":          map[string]any{"time": sendA},
		"campaign_id":       "cmp001",
		"external_user_ids": []any{"u1"},
	}, nil)
	if a.Status != 200 || a.Body["message"] != "success" {
		t.Fatalf("schedule A -> %d %v", a.Status, a.Body)
	}
	if sid, ok := a.Body["schedule_id"].(string); !ok || !brazeScheduleRe.MatchString(sid) {
		t.Fatalf("schedule_id = %v, want UUID-shaped", a.Body["schedule_id"])
	}
	if did, ok := a.Body["dispatch_id"].(string); !ok || !brazeDispatchRe.MatchString(did) {
		t.Fatalf("schedule dispatch_id = %v, want 32-char hex", a.Body["dispatch_id"])
	}
	b := f.call("messages", "on_schedule_create", "POST", "/messages/schedule/create", nil, map[string]any{
		"schedule":          map[string]any{"time": sendB, "in_local_time": true},
		"external_user_ids": []any{"u1"},
		"messages":          map[string]any{"email": map[string]any{}},
	}, nil)
	if b.Status != 200 {
		t.Fatalf("schedule B -> %d: %v", b.Status, b.Body)
	}

	// ===== end_time is required =====
	// The real endpoint rejects a missing/unparseable end_time.
	if r := f.call("messages", "on_scheduled", "GET", "/messages/scheduled", nil, nil, nil); r.Status != 400 || r.Body["message"] != "Bad Request" {
		t.Fatalf("scheduled without end_time -> %d %v, want 400 Bad Request", r.Status, r.Body)
	}

	// ===== upcoming broadcasts list both schedules =====
	// Schedule types derive from the schedule object: UTC, or
	// local_time_zones when in_local_time is set.
	up := list(endAll)
	if len(up) != 2 {
		t.Fatalf("upcoming = %v, want both schedules", up)
	}
	byType := map[string]map[string]any{}
	for _, bc := range up {
		byType[bc["schedule_type"].(string)] = bc
	}
	utc, hasUTC := byType["UTC"]
	local, hasLocal := byType["local_time_zones"]
	if !hasUTC || !hasLocal {
		t.Fatalf("schedule types = %v, want UTC and local_time_zones", byType)
	}
	if utc["id"] != "cmp001" || utc["type"] != "Campaign" || utc["next_send_time"] != sendA {
		t.Fatalf("UTC broadcast = %v", utc)
	}
	if local["next_send_time"] != sendB {
		t.Fatalf("local broadcast = %v", local)
	}

	// ===== the send transition derives on read, exactly once =====
	// Advancing past sendA: the first listing marks it sent (persisted
	// before the webhook), drops it from upcoming, and emits message.sent
	// once — later reads stay quiet.
	f.vc.Advance(90 * time.Minute)
	after := list(endAll)
	if len(after) != 1 || after[0]["schedule_type"] != "local_time_zones" {
		t.Fatalf("after advance = %v, want only the later schedule", after)
	}
	envs := f.deliveries()
	if len(envs) != 1 || envs[0]["type"] != "message.sent" {
		t.Fatalf("scheduled deliveries = %+v, want exactly one message.sent", envs)
	}
	pl, _ := envs[0]["payload"].(map[string]any)
	if pl["schedule_id"] != a.Body["schedule_id"] || pl["dispatch_id"] != a.Body["dispatch_id"] {
		t.Fatalf("scheduled message.sent payload = %v, want schedule A ids", pl)
	}
	if again := list(endAll); len(again) != 1 {
		t.Fatalf("second read after transition = %v, want the later schedule only", again)
	}
	if len(f.deliveries()) != 1 {
		t.Fatalf("transition emitted more than once: %+v", f.deliveries())
	}
}

func TestBrazeSegmentsList(t *testing.T) {
	f := newBrazeFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== segments list seeds and pages by cursor =====
	// Three seeded segments; limit + the opaque cursor walk the pages.
	all := f.call("segments", "on_list_segments", "GET", "/segments/list", nil, nil, nil)
	if all.Status != 200 || all.Body["message"] != "success" {
		t.Fatalf("segments/list -> %d %v", all.Status, all.Body)
	}
	if segs := all.Body["segments"].([]any); len(segs) != 3 {
		t.Fatalf("seeded segments = %v, want 3", segs)
	}
	page1 := f.call("segments", "on_list_segments", "GET", "/segments/list",
		map[string]string{"limit": "2"}, nil, nil)
	if segs := page1.Body["segments"].([]any); len(segs) != 2 {
		t.Fatalf("page 1 = %v, want 2 segments", segs)
	}
	cursor, _ := page1.Body["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("page 1 next_cursor = %v, want a token", page1.Body["next_cursor"])
	}
	page2 := f.call("segments", "on_list_segments", "GET", "/segments/list",
		map[string]string{"limit": "2", "cursor": cursor}, nil, nil)
	if segs := page2.Body["segments"].([]any); len(segs) != 1 {
		t.Fatalf("page 2 = %v, want the last segment", segs)
	}
	if _, has := page2.Body["next_cursor"]; has {
		t.Fatalf("last page still advertises next_cursor = %v", page2.Body["next_cursor"])
	}
	// An invalid cursor is a 400 in the documented fatal envelope
	// ({message, errors}).
	bad := f.call("segments", "on_list_segments", "GET", "/segments/list",
		map[string]string{"limit": "2", "cursor": "bogus"}, nil, nil)
	if bad.Status != 400 || bad.Body["message"] != "Invalid cursor parameter." {
		t.Fatalf("invalid cursor -> %d %v, want 400 Invalid cursor parameter.", bad.Status, bad.Body)
	}
	if _, ok := bad.Body["errors"]; !ok {
		t.Fatalf("invalid cursor envelope has no errors array: %v", bad.Body)
	}
}

func TestBrazeUsersDelete(t *testing.T) {
	f := newBrazeFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	f.call("users", "on_track", "POST", "/users/track", nil, map[string]any{
		"attributes": []any{map[string]any{"external_id": "u9", "first_name": "Zoe"}},
		"events":     []any{map[string]any{"external_id": "u9", "name": "gone", "time": "2026-02-03T10:00:00Z"}},
	}, nil)

	// ===== one identifier type per delete =====
	// Mixing external_ids and user_aliases is a whole-request fatal.
	mixed := f.call("users", "on_delete", "POST", "/users/delete", nil, map[string]any{
		"external_ids": []any{"u9"},
		"user_aliases": []any{map[string]any{"alias_label": "l", "alias_name": "n"}},
	}, nil)
	if mixed.Status != 400 || mixed.Body["message"] != "Bad Request" {
		t.Fatalf("mixed delete -> %d %v, want 400 Bad Request", mixed.Status, mixed.Body)
	}

	// ===== delete removes the profile and its dependent records =====
	// The real envelope is {deleted: n} with no message key; the profile is
	// gone (and would export as invalid afterwards).
	del := f.call("users", "on_delete", "POST", "/users/delete", nil, map[string]any{
		"external_ids": []any{"u9"},
	}, nil)
	if del.Status != 200 {
		t.Fatalf("delete -> %d %v, want 200", del.Status, del.Body)
	}
	if n, ok := del.Body["deleted"].(int64); !ok || n != 1 {
		t.Fatalf("delete deleted = %v, want 1", del.Body["deleted"])
	}
	if _, has := del.Body["message"]; has {
		t.Fatalf("delete envelope carries a message key: %v", del.Body)
	}
	exp := f.call("users", "on_export_ids", "POST", "/users/export/ids", nil,
		map[string]any{"external_ids": []any{"u9"}}, nil)
	if users := exp.Body["users"].([]any); len(users) != 0 {
		t.Fatalf("deleted user still exports: %v", users)
	}
	if inv := exp.Body["invalid_user_ids"].([]any); len(inv) != 1 || inv[0] != "u9" {
		t.Fatalf("invalid_user_ids after delete = %v, want [u9]", inv)
	}
}
