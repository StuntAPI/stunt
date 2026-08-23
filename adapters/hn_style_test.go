package adapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter"
	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// Drives the hn-style adapter scripts directly (lib.star preloaded) over the
// seeded items/users collections: the public Firebase read surface — item
// reads (the .json suffix convention, parent/kids comment trees, sparse
// field omission, the literal "null" for unknown ids), the six story-list
// endpoints (bare integer arrays with per-list ordering and partitioning),
// user reads (created/karma/about/submitted), the Firebase REST query params
// (accepted and ignored — not modeled), and the fact that the public API has
// no auth gate. The login/submit write flow is an intentional deviation
// (mirrors a reference HN client) and stays covered by the engine test.
const hnHost = "hacker-news.firebaseio.test"

// hnListPaths maps each story-list handler to its literal route.
var hnListPaths = map[string]string{
	"on_topstories":  "/v0/topstories.json",
	"on_newstories":  "/v0/newstories.json",
	"on_beststories": "/v0/beststories.json",
	"on_askstories":  "/v0/askstories.json",
	"on_showstories": "/v0/showstories.json",
	"on_jobstories":  "/v0/jobstories.json",
}

type hnFixture struct {
	t    *testing.T
	vms  map[string]*starlark.VM
	host string
}

func newHnFixture(t *testing.T) *hnFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "hn-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	tmp := t.TempDir()
	store, _ := primitives.Open(filepath.Join(tmp, "h.db"))
	t.Cleanup(func() { store.Close() })
	kvStore, _ := kv.Open(filepath.Join(tmp, "h.kv.db"))
	t.Cleanup(func() { kvStore.Close() })
	blobStore, _ := blob.Open(filepath.Join(tmp, "blobs"))
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(time.Unix(1_700_000_000, 0).UTC())
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test",
	})

	// Seed exactly what the engine boots from adapter.yaml (Collection.Seed
	// is a no-op on a non-empty collection).
	for _, name := range []string{"items", "users"} {
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
	return &hnFixture{t: t, host: hnHost, vms: map[string]*starlark.VM{
		"stories": load("stories.star"), "items": load("items.star"), "users": load("users.star"),
	}}
}

// call drives a read handler the way the engine dispatches it: the {id}
// route capture arrives in params, query params map straight through, and
// headers carry whatever the client sent (nil = the public anonymous read).
func (f *hnFixture) call(group, handler, path string, params, query map[string]string, headers map[string]string) starlark.Response {
	f.t.Helper()
	if headers == nil {
		headers = map[string]string{}
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: "GET", Path: path, Host: f.host, Headers: headers, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// hnItem GETs /v0/item/<id>.json (the canonical suffixed form).
func (f *hnFixture) hnItem(id string, query map[string]string, headers map[string]string) map[string]any {
	f.t.Helper()
	r := f.call("items", "on_get_item", "/v0/item/"+id+".json", map[string]string{"id": id + ".json"}, query, headers)
	if r.Status != 200 {
		f.t.Fatalf("item %s -> %d: %v %v", id, r.Status, r.Body, r.RawBody)
	}
	hnJSONType(f.t, r)
	return r.Body
}

// hnUser GETs /v0/user/<id>.json.
func (f *hnFixture) hnUser(id string) map[string]any {
	f.t.Helper()
	r := f.call("users", "on_get_user", "/v0/user/"+id+".json", map[string]string{"id": id + ".json"}, nil, nil)
	if r.Status != 200 {
		f.t.Fatalf("user %s -> %d: %v %v", id, r.Status, r.Body, r.RawBody)
	}
	hnJSONType(f.t, r)
	return r.Body
}

// hnList GETs one of the six literal story-list endpoints.
func (f *hnFixture) hnList(handler string, query map[string]string) starlark.Response {
	f.t.Helper()
	return f.call("stories", handler, hnListPaths[handler], nil, query, nil)
}

// --- assertion helpers ---

// hnNum compares a JSON number regardless of int64/float64 width.
func hnNum(t *testing.T, v any, want int64, what string) {
	t.Helper()
	switch n := v.(type) {
	case int64:
		if n != want {
			t.Fatalf("%s = %d, want %d", what, n, want)
		}
	case float64:
		if n != float64(want) {
			t.Fatalf("%s = %v, want %d", what, n, want)
		}
	default:
		t.Fatalf("%s = %T(%v), want number %d", what, v, v, want)
	}
}

// hnIDs decodes a story-list body — a bare JSON array of integers — into ids.
func hnIDs(t *testing.T, r starlark.Response) []int64 {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("story list -> %d: %v", r.Status, r.RawBody)
	}
	hnJSONType(t, r)
	if !strings.HasPrefix(r.RawBody, "[") {
		t.Fatalf("story list body %q is not a bare JSON array", r.RawBody)
	}
	var ids []int64
	if err := json.Unmarshal([]byte(r.RawBody), &ids); err != nil {
		t.Fatalf("story list body %q is not an integer array: %v", r.RawBody, err)
	}
	return ids
}

// hnIDsEqual asserts an exact id list.
func hnIDsEqual(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

// hnKids returns an item's kids ids, nil when the item has none.
func hnKids(t *testing.T, body map[string]any) []int64 {
	t.Helper()
	raw, ok := body["kids"].([]any)
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(raw))
	for _, k := range raw {
		n, _ := k.(int64)
		out = append(out, n)
	}
	return out
}

// hnJSONType asserts the Firebase JSON content type.
func hnJSONType(t *testing.T, r starlark.Response) {
	t.Helper()
	if ct := r.Headers["content-type"]; ct != "application/json; charset=utf-8" {
		t.Fatalf("content-type = %q, want application/json; charset=utf-8", ct)
	}
}

// hnSubmitted returns a user's submitted ids as numbers.
func hnSubmitted(t *testing.T, body map[string]any) []int64 {
	t.Helper()
	raw, ok := body["submitted"].([]any)
	if !ok {
		t.Fatalf("submitted = %v, want array", body["submitted"])
	}
	out := make([]int64, 0, len(raw))
	for _, s := range raw {
		n, _ := s.(int64)
		out = append(out, n)
	}
	return out
}

// TestHNItemReads: the Firebase item endpoint — the .json route-capture
// convention, the sparse per-type item shape, parent/kids comment trees with
// descendants cross-checked against the walked subtree, and the literal
// "null" for unknown ids.
func TestHNItemReads(t *testing.T) {
	f := newHnFixture(t)

	// ===== item reads resolve the .json route capture into the full Firebase story shape =====
	// The {id} capture arrives as "1001.json"; the answer is the story item.
	story := f.hnItem("1001", nil, nil)
	hnNum(t, story["id"], 1001, "id")
	if story["type"] != "story" || story["by"] != "alice" {
		t.Fatalf("story 1001 type/by = %v/%v, want story/alice", story["type"], story["by"])
	}
	if story["title"] != "Show HN: A synthetic demo for local testing" {
		t.Fatalf("story 1001 title = %v", story["title"])
	}
	if story["url"] != "https://example.test/show-hn" {
		t.Fatalf("story 1001 url = %v", story["url"])
	}
	hnNum(t, story["score"], 42, "score")
	hnNum(t, story["time"], 1700000100, "time")
	hnIDsEqual(t, hnKids(t, story), 1002, 1003)

	// ===== comments link up via parent and kids, omit story-only fields, and count their subtree in descendants =====
	// Firebase items carry only the fields their type uses.
	c2 := f.hnItem("1002", nil, nil)
	if c2["type"] != "comment" || c2["by"] != "bob" {
		t.Fatalf("comment 1002 type/by = %v/%v", c2["type"], c2["by"])
	}
	hnNum(t, c2["parent"], 1001, "parent")
	hnIDsEqual(t, hnKids(t, c2), 1004)
	if txt, _ := c2["text"].(string); txt == "" {
		t.Fatal("comment 1002 has no text")
	}
	for _, k := range []string{"title", "url", "score", "descendants"} {
		if _, ok := c2[k]; ok {
			t.Fatalf("comment 1002 carries story-only field %q = %v", k, c2[k])
		}
	}
	// The nested leaf has a parent but no kids of its own.
	c4 := f.hnItem("1004", nil, nil)
	hnNum(t, c4["parent"], 1002, "parent")
	if kids := hnKids(t, c4); len(kids) != 0 {
		t.Fatalf("leaf comment 1004 kids = %v, want none", kids)
	}
	// Ask stories are text stories: no url, no kids.
	ask := f.hnItem("1005", nil, nil)
	if _, ok := ask["url"]; ok {
		t.Fatalf("ask story 1005 carries url = %v", ask["url"])
	}
	if kids := hnKids(t, ask); len(kids) != 0 {
		t.Fatalf("ask story 1005 kids = %v, want none", kids)
	}
	// descendants must equal the comment subtree reachable through kids.
	for _, id := range []int64{1001, 1005} {
		body := f.hnItem(strconv.FormatInt(id, 10), nil, nil)
		size := int64(0)
		stack := hnKids(t, body)
		for len(stack) > 0 {
			kid := stack[0]
			stack = stack[1:]
			size++
			stack = append(stack, hnKids(t, f.hnItem(strconv.FormatInt(kid, 10), nil, nil))...)
		}
		hnNum(t, body["descendants"], size, "descendants of item "+strconv.FormatInt(id, 10))
	}

	// ===== unknown items and users answer 200 with the literal null, not a 404 =====
	// Firebase's missing-resource convention is the four-byte body "null".
	missing := f.call("items", "on_get_item", "/v0/item/99999.json", map[string]string{"id": "99999.json"}, nil, nil)
	if missing.Status != 200 || missing.RawBody != "null" {
		t.Fatalf("unknown item -> %d %q, want 200 \"null\"", missing.Status, missing.RawBody)
	}
	hnJSONType(t, missing)
	missingUser := f.call("users", "on_get_user", "/v0/user/nobody.json", map[string]string{"id": "nobody.json"}, nil, nil)
	if missingUser.Status != 200 || missingUser.RawBody != "null" {
		t.Fatalf("unknown user -> %d %q, want 200 \"null\"", missingUser.Status, missingUser.RawBody)
	}
}

// TestHNStoryLists: the six literal list endpoints — bare integer JSON
// arrays, per-list ordering (top/best by score, new newest-first), the
// ask/show title-prefix partition, jobs kept off the story lists, and the
// Firebase REST query params being accepted and ignored.
func TestHNStoryLists(t *testing.T) {
	f := newHnFixture(t)

	// ===== story lists are bare integer arrays: top and best rank by score, new is newest-first =====
	// Firebase items carry no rank; score is the derivable stand-in, so top
	// (1001 score 42 ahead of 1005 score 15) differs from new (id-descending).
	hnIDsEqual(t, hnIDs(t, f.hnList("on_topstories", nil)), 1001, 1005)
	hnIDsEqual(t, hnIDs(t, f.hnList("on_beststories", nil)), 1001, 1005)
	hnIDsEqual(t, hnIDs(t, f.hnList("on_newstories", nil)), 1005, 1001)

	// ===== ask and show partition the story set by title prefix; jobs stay type-only and off the story lists =====
	// "Ask HN"/"Show HN" prefixes are the only marker the item shape has.
	hnIDsEqual(t, hnIDs(t, f.hnList("on_askstories", nil)), 1005)
	hnIDsEqual(t, hnIDs(t, f.hnList("on_showstories", nil)), 1001)
	// Job items (type=job) get their own list and never leak into story lists.
	hnIDsEqual(t, hnIDs(t, f.hnList("on_jobstories", nil)), 1006)
	for handler, path := range hnListPaths {
		if handler == "on_jobstories" {
			continue
		}
		for _, id := range hnIDs(t, f.hnList(handler, nil)) {
			if id == 1006 {
				t.Fatalf("%s leaks the job item 1006", path)
			}
		}
	}

	// ===== Firebase REST query params (print=pretty, orderBy, limitToFirst) are accepted and ignored =====
	// As-is: none of the Firebase REST query params are modeled — reads are
	// unaffected by them (a documented gap, not silent filtering).
	plain := f.hnList("on_topstories", nil)
	pretty := f.hnList("on_topstories", map[string]string{"print": "pretty"})
	if pretty.Status != 200 || pretty.RawBody != plain.RawBody {
		t.Fatalf("topstories?print=pretty -> %d %q, want the identical body %q", pretty.Status, pretty.RawBody, plain.RawBody)
	}
	filtered := f.hnItem("1001", map[string]string{"orderBy": "\"score\"", "limitToFirst": "1"}, nil)
	hnNum(t, filtered["id"], 1001, "id under orderBy/limitToFirst")
	if len(filtered) != len(f.hnItem("1001", nil, nil)) {
		t.Fatal("orderBy/limitToFirst changed the item shape; they must be ignored until modeled")
	}
}

// TestHNUserReads: the Firebase user endpoint — the .json route capture, the
// created/karma/about/submitted shape with by/submitted agreeing both ways,
// and sparse omission of an empty about.
func TestHNUserReads(t *testing.T) {
	f := newHnFixture(t)

	// ===== user reads resolve .json ids into created/karma/about/submitted with by/submitted agreeing =====
	alice := f.hnUser("alice")
	if alice["id"] != "alice" {
		t.Fatalf("user id = %v, want alice", alice["id"])
	}
	hnNum(t, alice["created"], 1690000000, "created")
	hnNum(t, alice["karma"], 5123, "karma")
	if alice["about"] != "Maker and tester" {
		t.Fatalf("alice about = %v", alice["about"])
	}
	hnIDsEqual(t, hnSubmitted(t, alice), 1001)
	// The authorship is bidirectional: the item's by is the user, and the
	// user's submitted carries the item.
	if by := f.hnItem("1001", nil, nil)["by"]; by != "alice" {
		t.Fatalf("item 1001 by = %v, want alice", by)
	}

	// ===== users without a bio omit about, matching the sparse Firebase shape =====
	// carol is seeded with an empty about; the field must be absent.
	carol := f.hnUser("carol")
	if _, ok := carol["about"]; ok {
		t.Fatalf("carol about = %v, want the field omitted", carol["about"])
	}
	hnNum(t, carol["karma"], 99, "karma")
	hnIDsEqual(t, hnSubmitted(t, carol), 1003)
}

// TestHNPublicReadSurface: the honest shape of the surface — every read is
// anonymous (no credential gates anything), and the read surface is exactly
// the six lists plus item and user: /v0/maxitem.json and /v0/updates.json
// are not modeled (the documented conformance gap).
func TestHNPublicReadSurface(t *testing.T) {
	f := newHnFixture(t)

	// ===== reads are public: no credential is required and a stray bearer gates nothing =====
	// The HN Firebase API has no auth; a stray Authorization and a foreign
	// session cookie must neither gate nor alter an item read.
	stray := f.hnItem("1001", nil, map[string]string{
		"Authorization": "Bearer not-a-token", "Cookie": "user=hn_session_9",
	})
	hnNum(t, stray["id"], 1001, "id under stray credentials")
	if len(stray) != len(f.hnItem("1001", nil, nil)) {
		t.Fatal("stray credentials changed the item shape; reads must ignore them")
	}

	// ===== maxitem and updates are not modeled: the read surface is the six lists plus item and user =====
	// As-is: the manifest declares exactly these routes; extending it (e.g.
	// adding maxitem) means extending this suite too.
	a, err := adapter.Load(filepath.Join(repoAdaptersDir(t), "hn-style"))
	if err != nil {
		t.Fatalf("load hn-style manifest: %v", err)
	}
	have := map[string]bool{}
	for _, e := range a.Endpoints {
		have[e.Method+" "+e.Route] = true
	}
	for _, want := range []string{
		"GET /v0/topstories.json", "GET /v0/newstories.json", "GET /v0/beststories.json",
		"GET /v0/askstories.json", "GET /v0/showstories.json", "GET /v0/jobstories.json",
		"GET /v0/item/{id}", "GET /v0/user/{id}",
		// The write flow is the documented deviation (reference-client login
		// and submit over the read-only Firebase API).
		"POST /login", "GET /logout", "POST /submit",
	} {
		if !have[want] {
			t.Errorf("manifest is missing %s", want)
		}
		delete(have, want)
	}
	for extra := range have {
		t.Errorf("manifest declares unexpected route %s (update this suite)", extra)
	}
	for _, e := range a.Endpoints {
		if strings.Contains(e.Route, "maxitem") || strings.Contains(e.Route, "updates") {
			t.Errorf("route %s exists but maxitem/updates were unmodeled when this suite was written", e.Route)
		}
	}
}
