package adapters

import (
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

// Drives the twitter-style adapter scripts directly (lib.star preloaded)
// over a shared store and a virtual clock: the mock OAuth2 token endpoint
// and the never-enforced bearer gate, tweet create with the 280-char limit
// and reply-chain integrity, read-by-id with tweet.fields projection and
// author_id expansions, the user endpoints, the reverse-chronological
// timeline with its v2 filters, max_results/pagination_token paging via
// meta.next_token, and the X API v2 error envelopes (resource-not-found
// 404s, invalid-request 400s).
const twitterAuth = "Bearer mock-token-local-testing-only"

type twitterFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newTwitterFixture(t *testing.T, start time.Time) *twitterFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "twitter-style")
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

	// Seed exactly what the engine boots from adapter.yaml (Collection.Seed
	// is a no-op on a non-empty collection).
	for _, name := range []string{"tweets", "users"} {
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
	return &twitterFixture{t: t, vc: vc, host: "api.twitter.test", vms: map[string]*starlark.VM{
		"auth": load("auth.star"), "tweets": load("tweets.star"),
		"users": load("users.star"), "timeline": load("timeline.star"),
	}}
}

func (f *twitterFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
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

// createTweet POSTs a tweet and returns its id.
func (f *twitterFixture) createTweet(text, replyTo string) string {
	f.t.Helper()
	body := map[string]any{"text": text}
	if replyTo != "" {
		body["reply"] = map[string]any{"in_reply_to_tweet_id": replyTo}
	}
	resp := f.call("tweets", "on_create", "POST", "/2/tweets", nil, nil, body, twitterAuth)
	if resp.Status != 201 {
		f.t.Fatalf("create tweet -> %d: %v", resp.Status, resp.Body)
	}
	data, _ := resp.Body["data"].(map[string]any)
	id, _ := data["id"].(string)
	if id == "" {
		f.t.Fatalf("create tweet returned no id: %v", resp.Body)
	}
	return id
}

// getTweet runs GET /2/tweets/{id}.
func (f *twitterFixture) getTweet(id string, query map[string]string) starlark.Response {
	f.t.Helper()
	return f.call("tweets", "on_retrieve", "GET", "/2/tweets/"+id, map[string]string{"id": id}, query, nil, twitterAuth)
}

// timeline runs the reverse-chronological timeline read for usr_me.
func (f *twitterFixture) timeline(query map[string]string) starlark.Response {
	f.t.Helper()
	return f.call("timeline", "on_timeline", "GET", "/2/users/usr_me/timelines/reverse_chronological",
		map[string]string{"id": "usr_me"}, query, nil, twitterAuth)
}

// tweetIDs returns the id sequence of a {data:[...]} list response.
func twitterIDs(t *testing.T, r starlark.Response) []string {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("tweet list -> %d: %v", r.Status, r.Body)
	}
	list, ok := r.Body["data"].([]any)
	if !ok {
		t.Fatalf("data = %v, want array", r.Body["data"])
	}
	out := []string{}
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("list entry is %T, want object", e)
		}
		id, _ := m["id"].(string)
		out = append(out, id)
	}
	return out
}

// nextToken returns meta.next_token, "" when the page is the last.
func twitterNextToken(t *testing.T, r starlark.Response) string {
	t.Helper()
	meta, ok := r.Body["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta = %v, want object", r.Body["meta"])
	}
	tok, _ := meta["next_token"].(string)
	return tok
}

// --- assertion helpers ---

// twitterData returns the {data} object of a 200 response.
func twitterData(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("want 200, got %d: %v", r.Status, r.Body)
	}
	data, ok := r.Body["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v, want object", r.Body["data"])
	}
	return data
}

// twitterErrEntry asserts the body carries v2's errors array with exactly
// one problem object and returns it.
func twitterErrEntry(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	errs, ok := r.Body["errors"].([]any)
	if !ok || len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one entry (the X API v2 envelope)", r.Body["errors"])
	}
	e, ok := errs[0].(map[string]any)
	if !ok {
		t.Fatalf("errors[0] is %T, want object", errs[0])
	}
	return e
}

// twitterNotFound asserts v2's resource-not-found envelope: 404 with a
// single errors[] problem object naming the resource's type, parameter and
// id under the documented problem type.
func twitterNotFound(t *testing.T, r starlark.Response, resourceType, parameter, value string) {
	t.Helper()
	if r.Status != 404 {
		t.Fatalf("%s not found -> %d, want 404; body %v", resourceType, r.Status, r.Body)
	}
	e := twitterErrEntry(t, r)
	if e["title"] != "Not Found Error" {
		t.Fatalf("404 title = %v, want Not Found Error", e["title"])
	}
	if e["resource_type"] != resourceType || e["parameter"] != parameter {
		t.Fatalf("404 problem = resource_type %v / parameter %v, want %s / %s", e["resource_type"], e["parameter"], resourceType, parameter)
	}
	if e["value"] != value || e["resource_id"] != value {
		t.Fatalf("404 value/resource_id = %v / %v, want %s", e["value"], e["resource_id"], value)
	}
	if e["type"] != "https://api.twitter.com/2/problems/resource-not-found" {
		t.Fatalf("404 problem type = %v", e["type"])
	}
	if d, _ := e["detail"].(string); d == "" {
		t.Fatalf("404 detail is empty: %v", e)
	}
}

// twitterBadRequest asserts v2's invalid-request envelope: 400 whose
// errors[] entry echoes the offending parameter value and whose top level
// carries the RFC7807-ish fields.
func twitterBadRequest(t *testing.T, r starlark.Response, parameter, msgPart string) {
	t.Helper()
	if r.Status != 400 {
		t.Fatalf("bad request -> %d, want 400; body %v", r.Status, r.Body)
	}
	if r.Body["title"] != "Invalid Request" || r.Body["type"] != "about:blank" {
		t.Fatalf("400 envelope top level = %v", r.Body)
	}
	e := twitterErrEntry(t, r)
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, msgPart) {
		t.Fatalf("400 message = %q, want it to contain %q", msg, msgPart)
	}
	params, ok := e["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("400 parameters = %v, want object", e["parameters"])
	}
	if _, ok := params[parameter].([]any); !ok {
		t.Fatalf("400 parameters[%s] = %v, want the echoed value as a list", parameter, params[parameter])
	}
}

// twitterNum compares a JSON number regardless of int64/float64 width
// (metrics and counts round-trip through the collection store as floats).
func twitterNum(t *testing.T, v any, want float64, what string) {
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

// TestTwitterTweetLifecycle: the mock OAuth2 token endpoint and the
// never-enforced bearer gate, tweet create validation, read-by-id with
// tweet.fields/expansions, and delete.
func TestTwitterTweetLifecycle(t *testing.T) {
	f := newTwitterFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== the mock oauth2 endpoint mints a bearer and the bearer gate is never enforced =====
	// As-is: every endpoint accepts any (or no) Authorization header; the
	// real API answers 401 {"title":"Unauthorized",...} without one.
	// Documented simulator deviation — asserted here so a future gate flips
	// this loudly.
	tok := f.call("auth", "on_token", "POST", "/2/oauth2/token", nil, nil,
		map[string]any{"grant_type": "client_credentials"}, "")
	if tok.Status != 200 {
		t.Fatalf("oauth2 token -> %d: %v", tok.Status, tok.Body)
	}
	if tok.Body["token_type"] != "bearer" {
		t.Fatalf("token_type = %v, want bearer", tok.Body["token_type"])
	}
	twitterNum(t, tok.Body["expires_in"], 7200, "expires_in")
	if tok.Body["access_token"] != "mock-token-local-testing-only" {
		t.Fatalf("access_token = %v", tok.Body["access_token"])
	}
	if noAuth := f.call("users", "on_me", "GET", "/2/users/me", nil, nil, nil, ""); noAuth.Status != 200 {
		t.Fatalf("no Authorization header -> %d, want 200 (auth not enforced)", noAuth.Status)
	}
	if bogus := f.call("users", "on_me", "GET", "/2/users/me", nil, nil, nil, "Bearer not-a-real-token"); bogus.Status != 200 {
		t.Fatalf("bogus bearer -> %d, want 200 (auth not enforced)", bogus.Status)
	}

	// ===== create enforces the 280-char limit and reply-chain integrity with v2 400 envelopes =====
	twitterBadRequest(t, f.call("tweets", "on_create", "POST", "/2/tweets", nil, nil,
		map[string]any{"text": ""}, twitterAuth), "text", "text is required")
	twitterBadRequest(t, f.call("tweets", "on_create", "POST", "/2/tweets", nil, nil,
		map[string]any{"text": "   "}, twitterAuth), "text", "text is required")
	twitterBadRequest(t, f.call("tweets", "on_create", "POST", "/2/tweets", nil, nil,
		map[string]any{"text": strings.Repeat("a", 281)}, twitterAuth), "text", "max 280")
	if r := f.call("tweets", "on_create", "POST", "/2/tweets", nil, nil,
		map[string]any{"text": strings.Repeat("b", 280)}, twitterAuth); r.Status != 201 {
		t.Fatalf("280-char tweet (inclusive limit) -> %d: %v", r.Status, r.Body)
	}
	twitterBadRequest(t, f.call("tweets", "on_create", "POST", "/2/tweets", nil, nil, map[string]any{
		"text": "replying into the void", "reply": map[string]any{"in_reply_to_tweet_id": "twt_nope"},
	}, twitterAuth), "reply.in_reply_to_tweet_id", "not found")

	// ===== create answers 201 {data:{id,text}} with kv-sequence ids and clock-stamped created_at =====
	first := f.createTweet("Hello from the VM suite", "")
	second := f.createTweet("A second post", "")
	if !strings.HasPrefix(first, "twt_") || !strings.HasPrefix(second, "twt_") {
		t.Fatalf("tweet ids = %q / %q, want twt_ prefix", first, second)
	}
	if first == second {
		t.Fatalf("tweet ids collide: %q", first)
	}
	doc := twitterData(t, f.getTweet(first, nil))
	if doc["text"] != "Hello from the VM suite" || doc["author_id"] != "usr_me" {
		t.Fatalf("tweet doc = %v", doc)
	}
	if doc["created_at"] != "2026-02-03T12:00:00.000Z" {
		t.Fatalf("created_at = %v, want the engine clock at v2 millisecond precision", doc["created_at"])
	}
	// A reply threads onto its target and reads back.
	reply := f.createTweet("threading the needle", first)
	rd := twitterData(t, f.getTweet(reply, nil))
	if rd["in_reply_to_tweet_id"] != first {
		t.Fatalf("reply in_reply_to_tweet_id = %v, want %s", rd["in_reply_to_tweet_id"], first)
	}

	// ===== read by id projects tweet.fields and expands author_id into includes =====
	projected := twitterData(t, f.getTweet(reply, map[string]string{"tweet.fields": "id,text"}))
	if len(projected) != 2 || projected["text"] == nil {
		t.Fatalf("tweet.fields projection = %v, want exactly id+text", projected)
	}
	stamped := twitterData(t, f.getTweet(reply, map[string]string{"tweet.fields": "created_at"}))
	if len(stamped) != 2 || stamped["created_at"] == nil {
		t.Fatalf("tweet.fields=created_at = %v, want exactly id+created_at", stamped)
	}
	// As-is: with no tweet.fields the full stored doc comes back — v2's
	// bare default is id+text only (simulator superset).
	full := twitterData(t, f.getTweet(reply, nil))
	if full["author_id"] == nil || full["created_at"] == nil {
		t.Fatalf("bare tweet read = %v, want the full stored doc", full)
	}

	expanded := f.getTweet(reply, map[string]string{"expansions": "author_id"})
	includes, ok := expanded.Body["includes"].(map[string]any)
	if !ok {
		t.Fatalf("includes = %v, want an includes block", expanded.Body["includes"])
	}
	users, _ := includes["users"].([]any)
	if len(users) != 1 {
		t.Fatalf("includes.users = %v, want the author", includes["users"])
	}
	author, _ := users[0].(map[string]any)
	if author["id"] != "usr_me" || author["username"] != "local_test_user" || author["name"] != "Local Test User" {
		t.Fatalf("expanded author = %v", author)
	}
	if _, has := author["public_metrics"]; has {
		t.Fatal("default expansion carries public_metrics; v2's default user set is id/name/username")
	}
	rich := f.getTweet(reply, map[string]string{
		"expansions": "author_id", "user.fields": "public_metrics,username",
	})
	richUsers, _ := rich.Body["includes"].(map[string]any)["users"].([]any)
	richAuthor, _ := richUsers[0].(map[string]any)
	if _, has := richAuthor["public_metrics"]; !has {
		t.Fatalf("user.fields=public_metrics dropped it: %v", richAuthor)
	}
	metrics, _ := richAuthor["public_metrics"].(map[string]any)
	twitterNum(t, metrics["followers_count"], 42, "followers_count")
	if richAuthor["name"] != nil {
		t.Fatalf("user.fields projection kept name: %v", richAuthor)
	}
	if richAuthor["username"] != "local_test_user" {
		t.Fatalf("user.fields projection lost username: %v", richAuthor)
	}

	// ===== delete answers {data:{deleted:true}} and later reads are 404s =====
	del := f.call("tweets", "on_delete", "DELETE", "/2/tweets/"+first, map[string]string{"id": first}, nil, nil, twitterAuth)
	delData, ok := del.Body["data"].(map[string]any)
	if del.Status != 200 || !ok || delData["deleted"] != true {
		t.Fatalf("delete -> %d %v, want 200 {data:{deleted:true}}", del.Status, del.Body)
	}
	twitterNotFound(t, f.getTweet(first, nil), "tweet", "id", first)
	twitterNotFound(t, f.call("tweets", "on_delete", "DELETE", "/2/tweets/"+first,
		map[string]string{"id": first}, nil, nil, twitterAuth), "tweet", "id", first)
	twitterNotFound(t, f.getTweet("twt_nope", nil), "tweet", "id", "twt_nope")
	// Seeded tweets are deletable the same way.
	if r := f.call("tweets", "on_delete", "DELETE", "/2/tweets/seed-tweet-alpha",
		map[string]string{"id": "seed-tweet-alpha"}, nil, nil, twitterAuth); r.Status != 200 {
		t.Fatalf("delete seeded tweet -> %d: %v", r.Status, r.Body)
	}
}

// TestTwitterUsers: the session user, show-by-id and lookup-by-username —
// including the literal /2/users/me route winning over /2/users/{id} — with
// user.fields projection and v2 resource-not-found 404s.
func TestTwitterUsers(t *testing.T) {
	f := newTwitterFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== /me resolves the literal route before /2/users/{id} and returns v2's default field set =====
	// /2/users/me is declared before the parameterized route, so "me" is the
	// session user rather than a {id} miss.
	me := twitterData(t, f.call("users", "on_me", "GET", "/2/users/me", nil, nil, nil, twitterAuth))
	if me["id"] != "usr_me" || me["username"] != "local_test_user" || me["name"] != "Local Test User" {
		t.Fatalf("me = %v", me)
	}
	if _, has := me["public_metrics"]; has {
		t.Fatal("bare /me carries public_metrics; v2's default user fields are id/name/username")
	}
	full := twitterData(t, f.call("users", "on_me", "GET", "/2/users/me", nil,
		map[string]string{"user.fields": "public_metrics,created_at"}, nil, twitterAuth))
	metrics, ok := full["public_metrics"].(map[string]any)
	if !ok {
		t.Fatalf("public_metrics = %v, want object", full["public_metrics"])
	}
	twitterNum(t, metrics["followers_count"], 42, "followers_count")
	twitterNum(t, metrics["following_count"], 17, "following_count")
	twitterNum(t, metrics["tweet_count"], 3, "tweet_count")
	if full["created_at"] != "2024-01-10T08:00:00Z" {
		t.Fatalf("me created_at = %v", full["created_at"])
	}
	if full["name"] != nil {
		t.Fatalf("user.fields projection kept name: %v", full)
	}

	// ===== show by id and lookup by username round-trip the seeded users =====
	alpha := twitterData(t, f.call("users", "on_show", "GET", "/2/users/seed-user-alpha",
		map[string]string{"id": "seed-user-alpha"}, nil, nil, twitterAuth))
	if alpha["username"] != "alpha_local" || alpha["name"] != "Alpha Localuser" {
		t.Fatalf("seed-user-alpha = %v", alpha)
	}
	beta := twitterData(t, f.call("users", "on_lookup", "GET", "/2/users/by/username/beta_test",
		map[string]string{"username": "beta_test"}, nil, nil, twitterAuth))
	if beta["id"] != "seed-user-beta" {
		t.Fatalf("beta_test lookup = %v", beta)
	}
	// user.fields projects lookups too.
	proj := twitterData(t, f.call("users", "on_lookup", "GET", "/2/users/by/username/beta_test",
		map[string]string{"username": "beta_test"}, map[string]string{"user.fields": "username"}, nil, twitterAuth))
	if len(proj) != 2 || proj["username"] != "beta_test" {
		t.Fatalf("username-only projection = %v, want exactly id+username", proj)
	}

	// ===== unknown users are resource-not-found 404s in the v2 envelope =====
	twitterNotFound(t, f.call("users", "on_show", "GET", "/2/users/usr_ghost",
		map[string]string{"id": "usr_ghost"}, nil, nil, twitterAuth), "user", "id", "usr_ghost")
	twitterNotFound(t, f.call("users", "on_lookup", "GET", "/2/users/by/username/ghost",
		map[string]string{"username": "ghost"}, nil, nil, twitterAuth), "user", "username", "ghost")
}

// TestTwitterTimelineAndPagination: the reverse-chronological timeline with
// its v2 filters (start_time / end_time / exclude=replies), and
// max_results/pagination_token paging via meta.next_token on both the
// timeline and the tweet list.
func TestTwitterTimelineAndPagination(t *testing.T) {
	f := newTwitterFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// Three clock-stamped tweets: 12:00, 12:10, 12:20 (the last a reply).
	older := f.createTweet("first post of the session", "")
	f.vc.Advance(10 * time.Minute)
	mid := f.createTweet("second post", "")
	f.vc.Advance(10 * time.Minute)
	newest := f.createTweet("replying to the second post", mid)

	// ===== the timeline is newest-first and loses nothing by default =====
	got := twitterIDs(t, f.timeline(nil))
	if len(got) != 5 {
		t.Fatalf("timeline = %v, want all 5 tweets (3 created + 2 seeded)", got)
	}
	wantHead := []string{newest, mid, older}
	for i, w := range wantHead {
		if got[i] != w {
			t.Fatalf("timeline head = %v, want created tweets newest-first %v", got, wantHead)
		}
	}
	if got[3] != "seed-tweet-beta" || got[4] != "seed-tweet-alpha" {
		t.Fatalf("timeline tail = %v, want seeded beta then alpha", got[3:])
	}

	// ===== start_time/end_time bound created_at and exclude=replies drops threads =====
	if got := twitterIDs(t, f.timeline(map[string]string{"start_time": "2026-02-03T12:00:01Z"})); len(got) != 2 || got[0] != newest || got[1] != mid {
		t.Fatalf("start_time filter = %v, want only the 12:10 and 12:20 tweets", got)
	}
	if got := twitterIDs(t, f.timeline(map[string]string{"end_time": "2026-01-01T00:00:00Z"})); len(got) != 2 || got[0] != "seed-tweet-beta" {
		t.Fatalf("end_time filter = %v, want only the seeded tweets", got)
	}
	noReplies := twitterIDs(t, f.timeline(map[string]string{"exclude": "replies"}))
	if len(noReplies) != 4 {
		t.Fatalf("exclude=replies = %v, want the 4 non-reply tweets", noReplies)
	}
	for _, id := range noReplies {
		if id == newest {
			t.Fatalf("exclude=replies kept the reply %s: %v", newest, noReplies)
		}
	}

	// ===== max_results pages via meta.next_token; an invalid pagination_token is a 400 =====
	p1 := f.timeline(map[string]string{"max_results": "2"})
	if got := twitterIDs(t, p1); len(got) != 2 {
		t.Fatalf("max_results=2 page = %v", got)
	}
	meta, ok := p1.Body["meta"].(map[string]any)
	if !ok {
		t.Fatalf("timeline meta = %v, want object", p1.Body["meta"])
	}
	twitterNum(t, meta["result_count"], 2, "result_count")
	if twitterNextToken(t, p1) == "" {
		t.Fatal("non-final page has no meta.next_token")
	}

	seen := map[string]int{}
	cursor := ""
	for {
		query := map[string]string{"max_results": "2"}
		if cursor != "" {
			query["pagination_token"] = cursor
		}
		page := f.timeline(query)
		for _, id := range twitterIDs(t, page) {
			seen[id]++
		}
		next := twitterNextToken(t, page)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 5 {
		t.Fatalf("pagination walk covered %d distinct tweets, want all 5", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("tweet %s appeared %d times across pages, want exactly once", id, n)
		}
	}
	twitterBadRequest(t, f.timeline(map[string]string{"max_results": "2", "pagination_token": "abc"}),
		"pagination_token", "Invalid pagination_token")

	// ===== the tweet list shares the same v2 paging and stays unpaged without max_results =====
	list := f.call("tweets", "on_list", "GET", "/2/tweets", nil, map[string]string{"max_results": "3"}, nil, twitterAuth)
	if got := twitterIDs(t, list); len(got) != 3 {
		t.Fatalf("list max_results=3 page = %v", got)
	}
	twitterNum(t, list.Body["meta"].(map[string]any)["result_count"], 3, "list result_count")
	if twitterNextToken(t, list) == "" {
		t.Fatal("partial list page has no meta.next_token")
	}
	unpaged := f.call("tweets", "on_list", "GET", "/2/tweets", nil, nil, nil, twitterAuth)
	if got := twitterIDs(t, unpaged); len(got) != 5 {
		t.Fatalf("unpaged list = %v, want all 5 tweets", got)
	}
	if tok := twitterNextToken(t, unpaged); tok != "" {
		t.Fatalf("unpaged list advertises next_token %q; paging is off without max_results", tok)
	}
}
