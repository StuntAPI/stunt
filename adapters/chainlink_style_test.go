package adapters

import (
	"math/big"
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

// Drives the chainlink-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: clock-derived Data Feed rounds
// (phase-1 string roundIds, heartbeat cadence, newest-first paged history),
// the Functions secrets envelope + queued -> running -> fulfilled request
// lifecycle (no sleeps — the clock advances), the Automation keepers-registry
// lifecycle (cadence performs, premium accounting, cancel/withdraw), CCIP
// lanes, and the Bearer gate including the seeded token's virtual expiry.

const (
	clHost       = "chainlink-style.test"
	clBearer     = "Bearer cl_mock_test_token"
	clFeedETH    = "0x01-ETH-USD"
	clPremium    = "250000000000000000" // 0.25 LINK premium in juels
	clBaseAnswer = "345012345678"       // ETH/USD base answer (8 decimals)
	clSeedRound  = 9000                 // aggregator round a seeded feed starts at
	clHeartbeat  = 60                   // seconds per derived round
	clJuelsOne   = "1000000000000000000"
)

type chainlinkFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newChainlinkFixture(t *testing.T, start time.Time) *chainlinkFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "chainlink-style")
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
	return &chainlinkFixture{t: t, vc: vc, host: clHost, vms: map[string]*starlark.VM{
		"feeds": load("feeds.star"), "functions": load("functions.star"),
		"automation": load("automation.star"), "ccip": load("ccip.star"),
	}}
}

func (f *chainlinkFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	if params == nil {
		params = map[string]string{}
	}
	if query == nil {
		query = map[string]string{}
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// clData unwraps the { data: ... } envelope's inner object.
func clData(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	d, ok := r.Body["data"].(map[string]any)
	if !ok {
		t.Fatalf("response has no data object: %v", r.Body)
	}
	return d
}

// clDataList unwraps the { data: [...] } envelope's inner array.
func clDataList(t *testing.T, r starlark.Response) []any {
	t.Helper()
	d, ok := r.Body["data"].([]any)
	if !ok {
		t.Fatalf("response has no data array: %v", r.Body)
	}
	return d
}

// clErr pulls the code and message out of the { error: { code, message } }
// envelope.
func clErr(t *testing.T, r starlark.Response) (code, message string) {
	t.Helper()
	e, ok := r.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error envelope: %v", r.Body)
	}
	code, _ = e["code"].(string)
	message, _ = e["message"].(string)
	return code, message
}

// clNum reads a response number as int64 whether the adapter produced a
// Starlark int (computed fresh in this call) or a float (read back through
// the JSON document store) — both marshal to the same JSON number on the wire.
func clNum(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// clPhase1 renders the phase-1 encoded roundId (2^64 + aggregatorRound) as
// the string the adapter serializes uint80s as.
func clPhase1(aggregatorRound int64) string {
	return new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(aggregatorRound)).String()
}

// clLink renders whole LINK as a juels string.
func clLink(links int64) string {
	one, _ := new(big.Int).SetString(clJuelsOne, 10)
	return new(big.Int).Mul(one, big.NewInt(links)).String()
}

// mustBig parses a decimal string (juels arithmetic helper).
func mustBig(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("mustBig: " + s)
	}
	return v
}

func clBase() time.Time { return time.Unix(1_750_000_000, 0).UTC() }

// TestChainlinkFeedsClockDerivedRounds: the feed registry seeds itself on
// first read, the feed summary and latestRoundData agree, rounds roll exactly
// one per virtual heartbeat with phase-1 string roundIds bounded to ±0.25% of
// the base answer, the history pages newest-first, getRoundData resolves ids,
// and unknown feeds/rounds answer the documented 404 envelopes.
func TestChainlinkFeedsClockDerivedRounds(t *testing.T) {
	f := newChainlinkFixture(t, clBase())

	// ===== the five default feeds seed on first read and narrow by network =====
	// Feeds are public: no Authorization header at all.
	list := f.call("feeds", "on_list_feeds", "GET", "/feeds", nil, nil, nil, "")
	if list.Status != 200 {
		t.Fatalf("list feeds -> %d: %v", list.Status, list.Body)
	}
	feeds := clDataList(t, list)
	if len(feeds) != 5 {
		t.Fatalf("seeded %d feeds, want the 5 defaults", len(feeds))
	}
	first := feeds[0].(map[string]any)
	if first["feedID"] != clFeedETH || first["title"] != "ETH / USD" {
		t.Fatalf("first feed = %v, want %s ETH / USD", first["feedID"], clFeedETH)
	}
	// Every entry carries the AggregatorV3 summary triple (answers and
	// roundIds are strings; decimals round-trips the store as a float).
	for _, e := range feeds {
		fd := e.(map[string]any)
		if fd["latestAnswer"] == "" || fd["latestRoundId"] == "" {
			t.Fatalf("feed %v missing latestAnswer/latestRoundId: %v", fd["feedID"], fd)
		}
		if fd["decimals"] != float64(8) {
			t.Fatalf("feed %v decimals = %v (%T), want 8", fd["feedID"], fd["decimals"], fd["decimals"])
		}
	}
	eth := f.call("feeds", "on_list_feeds", "GET", "/feeds", nil,
		map[string]string{"network": "ethereum"}, nil, "")
	if got := clDataList(t, eth); len(got) != 4 {
		t.Fatalf("network=ethereum -> %d feeds, want 4", len(got))
	}
	poly := f.call("feeds", "on_list_feeds", "GET", "/feeds", nil,
		map[string]string{"network": "polygon"}, nil, "")
	if got := clDataList(t, poly); len(got) != 1 || got[0].(map[string]any)["feedID"] != "0x05-ETH-USD" {
		t.Fatalf("network=polygon -> %v, want only 0x05-ETH-USD", got)
	}

	// ===== the feed summary and latestRoundData agree at the same instant =====
	feed := f.call("feeds", "on_get_feed", "GET", "/feeds/"+clFeedETH,
		map[string]string{"feedID": clFeedETH}, nil, nil, "")
	if feed.Status != 200 {
		t.Fatalf("get feed -> %d: %v", feed.Status, feed.Body)
	}
	summary := clData(t, feed)
	lrd := f.call("feeds", "on_latest_round_data", "GET", "/feeds/"+clFeedETH+"/latestRoundData",
		map[string]string{"feedID": clFeedETH}, nil, nil, "")
	if lrd.Status != 200 {
		t.Fatalf("latestRoundData -> %d: %v", lrd.Status, lrd.Body)
	}
	round := clData(t, lrd)
	if summary["latestAnswer"] != round["answer"] ||
		summary["latestRoundId"] != round["roundId"] ||
		summary["latestTimestamp"] != round["updatedAt"] {
		t.Fatalf("feed summary %v disagrees with latestRoundData %v", summary, round)
	}

	// ===== rounds roll one per virtual minute with phase-1 string roundIds =====
	// A fresh feed carries 120 backdated heartbeats, so k = 120 at seed time.
	if got := round["roundId"]; got != clPhase1(clSeedRound+120) {
		t.Fatalf("latest roundId = %v, want phase-1 %s", got, clPhase1(clSeedRound+120))
	}
	if round["answeredInRound"] != round["roundId"] || round["startedAt"] != round["updatedAt"] {
		t.Fatalf("round shape = %v (answeredInRound/startedAt must mirror roundId/updatedAt)", round)
	}
	// The answer drifts at most devBps (25 = ±0.25%) off the base answer.
	ans, ok := new(big.Int).SetString(round["answer"].(string), 10)
	if !ok {
		t.Fatalf("answer %v is not a decimal string", round["answer"])
	}
	baseAns, _ := new(big.Int).SetString(clBaseAnswer, 10)
	dev := new(big.Int).Sub(ans, baseAns)
	if dev.Sign() < 0 {
		dev.Neg(dev)
	}
	if new(big.Int).Mul(dev, big.NewInt(10000)).Cmp(new(big.Int).Mul(baseAns, big.NewInt(25))) > 0 {
		t.Fatalf("answer %v drifts more than ±0.25%% from base %s", ans, clBaseAnswer)
	}
	updated0 := round["updatedAt"].(int64)

	// A partial heartbeat does not roll the round; a full one rolls exactly
	// one round and one heartbeat of time.
	f.vc.Advance(30 * time.Second)
	same := clData(t, f.call("feeds", "on_latest_round_data", "GET", "/feeds/"+clFeedETH+"/latestRoundData",
		map[string]string{"feedID": clFeedETH}, nil, nil, ""))
	if same["roundId"] != round["roundId"] {
		t.Fatalf("round rolled after a partial heartbeat: %v -> %v", round["roundId"], same["roundId"])
	}
	f.vc.Advance(90 * time.Second)
	next := clData(t, f.call("feeds", "on_latest_round_data", "GET", "/feeds/"+clFeedETH+"/latestRoundData",
		map[string]string{"feedID": clFeedETH}, nil, nil, ""))
	if next["roundId"] != clPhase1(clSeedRound+122) {
		t.Fatalf("roundId after two heartbeats = %v, want %s", next["roundId"], clPhase1(clSeedRound+122))
	}
	if got := next["updatedAt"].(int64); got != updated0+2*clHeartbeat {
		t.Fatalf("updatedAt after two heartbeats = %d, want %d", got, updated0+2*clHeartbeat)
	}

	// ===== round history pages newest-first and getRoundData resolves ids =====
	page1 := f.call("feeds", "on_list_rounds", "GET", "/feeds/"+clFeedETH+"/rounds",
		map[string]string{"feedID": clFeedETH}, map[string]string{"limit": "3"}, nil, "")
	if page1.Status != 200 {
		t.Fatalf("rounds -> %d: %v", page1.Status, page1.Body)
	}
	rounds1 := clDataList(t, page1)
	if len(rounds1) != 3 {
		t.Fatalf("limit=3 -> %d rounds, want 3", len(rounds1))
	}
	if page1.Body["count"] != int64(123) {
		t.Fatalf("round count = %v, want 123 (121 seeded + 2 elapsed)", page1.Body["count"])
	}
	if rounds1[0].(map[string]any)["roundId"] != next["roundId"] {
		t.Fatalf("history head = %v, want the latest round %v",
			rounds1[0].(map[string]any)["roundId"], next["roundId"])
	}
	prevUpdated := updated0 + 2*clHeartbeat + 1
	for _, e := range rounds1 {
		u := e.(map[string]any)["updatedAt"].(int64)
		if u >= prevUpdated {
			t.Fatalf("history not newest-first: updatedAt %d after %d", u, prevUpdated)
		}
		prevUpdated = u
	}
	cursor, _ := page1.Body["nextCursor"].(string)
	if cursor == "" {
		t.Fatalf("paged rounds carry no nextCursor: %v", page1.Body)
	}
	page2 := f.call("feeds", "on_list_rounds", "GET", "/feeds/"+clFeedETH+"/rounds",
		map[string]string{"feedID": clFeedETH},
		map[string]string{"limit": "3", "cursor": cursor}, nil, "")
	rounds2 := clDataList(t, page2)
	if len(rounds2) != 3 {
		t.Fatalf("second page -> %d rounds, want 3", len(rounds2))
	}
	seen := map[string]bool{}
	for _, e := range append(rounds1, rounds2...) {
		id := e.(map[string]any)["roundId"].(string)
		if seen[id] {
			t.Fatalf("round %s repeated across pages", id)
		}
		seen[id] = true
	}
	if len(seen) != 6 {
		t.Fatalf("two pages covered %d distinct rounds, want 6", len(seen))
	}

	one := f.call("feeds", "on_get_round", "GET", "/feeds/"+clFeedETH+"/rounds/"+next["roundId"].(string),
		map[string]string{"feedID": clFeedETH, "roundId": next["roundId"].(string)}, nil, nil, "")
	if one.Status != 200 || clData(t, one)["roundId"] != next["roundId"] {
		t.Fatalf("getRoundData -> %d %v", one.Status, one.Body)
	}
	if got := clData(t, one)["updatedAt"]; got != next["updatedAt"] {
		t.Fatalf("getRoundData updatedAt = %v, want %v", got, next["updatedAt"])
	}

	// ===== unknown feeds and rounds answer the documented 404 envelopes =====
	gone := f.call("feeds", "on_get_feed", "GET", "/feeds/0x-nope",
		map[string]string{"feedID": "0x-nope"}, nil, nil, "")
	if gone.Status != 404 {
		t.Fatalf("unknown feed -> %d, want 404", gone.Status)
	}
	if code, _ := clErr(t, gone); code != "NOT_FOUND" {
		t.Fatalf("unknown feed code = %q, want NOT_FOUND", code)
	}
	for _, rid := range []string{"12345", clPhase1(clSeedRound + 300)} {
		r := f.call("feeds", "on_get_round", "GET", "/feeds/"+clFeedETH+"/rounds/"+rid,
			map[string]string{"feedID": clFeedETH, "roundId": rid}, nil, nil, "")
		if r.Status != 404 {
			t.Fatalf("round %s -> %d, want 404", rid, r.Status)
		}
		if code, msg := clErr(t, r); code != "ROUND_NOT_FOUND" || !strings.Contains(msg, rid) {
			t.Fatalf("round %s error = %q %q, want ROUND_NOT_FOUND naming the id", rid, code, msg)
		}
	}
	// A malformed cursor is the adapter's own 400 (code is lowercase
	// invalid_cursor, unlike the SCREAMING_SNAKE codes elsewhere — asserted
	// as-is; see the deviation report).
	badCursor := f.call("feeds", "on_list_rounds", "GET", "/feeds/"+clFeedETH+"/rounds",
		map[string]string{"feedID": clFeedETH}, map[string]string{"limit": "3", "cursor": "not-a-cursor"}, nil, "")
	if badCursor.Status != 400 {
		t.Fatalf("invalid cursor -> %d, want 400", badCursor.Status)
	}
	if code, _ := clErr(t, badCursor); code != "invalid_cursor" {
		t.Fatalf("invalid cursor code = %q, want invalid_cursor", code)
	}
}

// TestChainlinkFunctionsSecretsAndRequests: the Bearer gate, the
// deterministic version-tagged secrets envelope (canonical key order, real
// slot/version semantics), and the queued -> running -> fulfilled request
// lifecycle with the RequestFulfilled event shape and the simulate_fail
// fulfillment-code vocabulary — all on the virtual clock, no sleeps.
func TestChainlinkFunctionsSecretsAndRequests(t *testing.T) {
	base := clBase()
	f := newChainlinkFixture(t, base)

	// ===== v2 endpoints reject missing and wrong bearers =====
	payload := map[string]any{"secrets": map[string]any{"API_KEY": "secret123"}}
	noAuth := f.call("functions", "on_encrypt_secrets", "POST", "/v2/functions/encryptSecrets",
		nil, nil, payload, "")
	if noAuth.Status != 401 {
		t.Fatalf("no bearer -> %d, want 401", noAuth.Status)
	}
	if code, msg := clErr(t, noAuth); code != "UNAUTHORIZED" ||
		msg != "Missing or invalid Authorization Bearer token" {
		t.Fatalf("no bearer error = %q %q", code, msg)
	}
	badAuth := f.call("functions", "on_encrypt_secrets", "POST", "/v2/functions/encryptSecrets",
		nil, nil, payload, "Bearer not-the-token")
	if badAuth.Status != 401 {
		t.Fatalf("bad bearer -> %d, want 401", badAuth.Status)
	}
	if code, msg := clErr(t, badAuth); code != "UNAUTHORIZED" ||
		!strings.HasPrefix(msg, "Invalid or expired API token: ") {
		t.Fatalf("bad bearer error = %q %q", code, msg)
	}

	// ===== encryptSecrets is a deterministic version-0 canonical envelope =====
	enc := f.call("functions", "on_encrypt_secrets", "POST", "/v2/functions/encryptSecrets",
		nil, nil, payload, clBearer)
	if enc.Status != 200 {
		t.Fatalf("encryptSecrets -> %d: %v", enc.Status, enc.Body)
	}
	envelope, _ := enc.Body["encryptedSecrets"].(string)
	if !strings.HasPrefix(envelope, "0x01") || len(envelope) != 68 {
		t.Fatalf("envelope = %q, want 0x01-prefixed 32-byte hex (68 chars)", envelope)
	}
	again := f.call("functions", "on_encrypt_secrets", "POST", "/v2/functions/encryptSecrets",
		nil, nil, payload, clBearer)
	if again.Body["encryptedSecrets"] != envelope {
		t.Fatalf("envelope not deterministic: %v vs %v", again.Body["encryptedSecrets"], envelope)
	}
	// Key insertion order must not matter (canonical encoding sorts keys).
	ord1 := f.call("functions", "on_encrypt_secrets", "POST", "/v2/functions/encryptSecrets",
		nil, nil, map[string]any{"secrets": map[string]any{"ALPHA": "a", "BETA": "b"}}, clBearer)
	ord2 := f.call("functions", "on_encrypt_secrets", "POST", "/v2/functions/encryptSecrets",
		nil, nil, map[string]any{"secrets": map[string]any{"BETA": "b", "ALPHA": "a"}}, clBearer)
	if ord1.Status != 200 || ord2.Status != 200 {
		t.Fatalf("multi-key encrypt -> %d / %d", ord1.Status, ord2.Status)
	}
	if ord1.Body["encryptedSecrets"] != ord2.Body["encryptedSecrets"] {
		t.Fatalf("key order changed the envelope: %v vs %v",
			ord1.Body["encryptedSecrets"], ord2.Body["encryptedSecrets"])
	}
	other := f.call("functions", "on_encrypt_secrets", "POST", "/v2/functions/encryptSecrets",
		nil, nil, map[string]any{"secrets": map[string]any{"API_KEY": "different"}}, clBearer)
	if other.Body["encryptedSecrets"] == envelope {
		t.Fatal("different payload produced the same envelope")
	}
	noSecrets := f.call("functions", "on_encrypt_secrets", "POST", "/v2/functions/encryptSecrets",
		nil, nil, map[string]any{}, clBearer)
	if noSecrets.Status != 400 {
		t.Fatalf("missing secrets object -> %d, want 400", noSecrets.Status)
	}
	if code, _ := clErr(t, noSecrets); code != "BAD_REQUEST" {
		t.Fatalf("missing secrets code = %q, want BAD_REQUEST", code)
	}

	// ===== createSecrets bumps the slot version and never leaks plaintext =====
	slot := func(secrets map[string]any) starlark.Response {
		return f.call("functions", "on_create_secrets", "POST", "/v2/functions/createSecrets",
			nil, nil, map[string]any{"secrets": secrets, "slotIDs": []any{0}}, clBearer)
	}
	s1 := slot(map[string]any{"API_KEY": "secret456"})
	if s1.Status != 200 {
		t.Fatalf("createSecrets -> %d: %v", s1.Status, s1.Body)
	}
	if s1.Body["secretID"] != "8000000001" {
		t.Fatalf("first secretID = %v, want 8000000001", s1.Body["secretID"])
	}
	if v1, ok := s1.Body["versions"].([]any); !ok || len(v1) != 1 || v1[0] != int64(1) {
		t.Fatalf("first upload versions = %v, want [1]", s1.Body["versions"])
	}
	// Same slot, same payload: the per-(donId, slot) version bumps to 2 and
	// the envelope changes — a new upload gets its own secretID.
	s2 := slot(map[string]any{"API_KEY": "secret456"})
	if v2, ok := s2.Body["versions"].([]any); !ok || len(v2) != 1 || v2[0] != int64(2) {
		t.Fatalf("second upload versions = %v, want [2] (per-slot version bump)", s2.Body["versions"])
	}
	if s2.Body["secretID"] != "8000000002" {
		t.Fatalf("second secretID = %v, want 8000000002", s2.Body["secretID"])
	}
	if s2.Body["encryptedSecrets"] == s1.Body["encryptedSecrets"] {
		t.Fatal("a new slot version produced the same envelope; version must affect it")
	}
	// Each upload reads back with its own envelope/version (store
	// round-trips the version list as floats); the plaintext never appears.
	for i, up := range []starlark.Response{s1, s2} {
		sid := up.Body["secretID"].(string)
		stored := f.call("functions", "on_get_secrets", "GET", "/v2/functions/secrets/"+sid,
			map[string]string{"secretID": sid}, nil, nil, clBearer)
		if stored.Status != 200 {
			t.Fatalf("get secrets %s -> %d: %v", sid, stored.Status, stored.Body)
		}
		if stored.Body["encryptedSecrets"] != up.Body["encryptedSecrets"] {
			t.Fatalf("stored envelope for %s = %v, want its own upload's %v",
				sid, stored.Body["encryptedSecrets"], up.Body["encryptedSecrets"])
		}
		if v, ok := stored.Body["versions"].([]any); !ok || v[0] != float64(i+1) {
			t.Fatalf("stored versions for %s = %v, want [%d]", sid, stored.Body["versions"], i+1)
		}
		if _, leak := stored.Body["secrets"]; leak {
			t.Fatalf("secrets upload %s leaks the plaintext payload", sid)
		}
	}
	missing := f.call("functions", "on_get_secrets", "GET", "/v2/functions/secrets/8000000999",
		map[string]string{"secretID": "8000000999"}, nil, nil, clBearer)
	if missing.Status != 404 {
		t.Fatalf("unknown secretID -> %d, want 404", missing.Status)
	}

	// ===== a request walks queued -> running -> fulfilled on the virtual clock =====
	create := f.call("functions", "on_create_request", "POST", "/v2/functions/createRequest",
		nil, nil, map[string]any{
			"subscriptionId": float64(1234), "gasLimit": float64(400000), "network": "polygon",
		}, clBearer)
	if create.Status != 200 || create.Body["status"] != "queued" {
		t.Fatalf("createRequest -> %d %v, want queued", create.Status, create.Body)
	}
	rid, _ := create.Body["requestID"].(string)
	if rid != "6000000001" {
		t.Fatalf("first requestID = %q, want 6000000001", rid)
	}
	if create.Body["gasLimit"] != int64(400000) {
		t.Fatalf("create gasLimit = %v, want 400000", create.Body["gasLimit"])
	}

	f.vc.Advance(1 * time.Second)
	running := f.call("functions", "on_get_request", "GET", "/v2/functions/request/"+rid,
		map[string]string{"requestID": rid}, nil, nil, clBearer)
	if running.Status != 200 || running.Body["status"] != "running" {
		t.Fatalf("poll at +1s -> %d %v, want running", running.Status, running.Body)
	}
	if _, has := running.Body["result"]; has {
		t.Fatalf("running request already carries a result: %v", running.Body)
	}

	f.vc.Advance(2 * time.Second)
	done := f.call("functions", "on_get_request", "GET", "/v2/functions/request/"+rid,
		map[string]string{"requestID": rid}, nil, nil, clBearer)
	if done.Status != 200 || done.Body["status"] != "fulfilled" {
		t.Fatalf("poll at +3s -> %d %v, want fulfilled", done.Status, done.Body)
	}
	result, _ := done.Body["result"].(string)
	if !strings.HasPrefix(result, "0x") || len(result) != 66 {
		t.Fatalf("result = %q, want 0x-prefixed bytes32 (66 chars)", result)
	}
	if done.Body["completedAt"] != int64(base.Unix()+3) {
		t.Fatalf("completedAt = %v, want the virtual +3s instant", done.Body["completedAt"])
	}
	if done.Body["subscriptionId"] != int64(1234) {
		t.Fatalf("fulfilled subscriptionId = %v, want 1234", done.Body["subscriptionId"])
	}
	// Repeated polls agree (the derived terminal state is persisted; numeric
	// fields keep their types across the store round-trip).
	repoll := f.call("functions", "on_get_request", "GET", "/v2/functions/request/"+rid,
		map[string]string{"requestID": rid}, nil, nil, clBearer)
	if repoll.Body["status"] != "fulfilled" || repoll.Body["result"] != result ||
		repoll.Body["completedAt"] != done.Body["completedAt"] ||
		repoll.Body["subscriptionId"] != int64(1234) {
		t.Fatalf("repoll disagrees: %v vs %v", repoll.Body, done.Body)
	}

	// ===== the fulfill event packs chainId << 64 | gasUsed =====
	ev, ok := done.Body["fulfillEvent"].(map[string]any)
	if !ok {
		t.Fatalf("fulfilled request carries no fulfillEvent: %v", done.Body)
	}
	if ev["name"] != "RequestFulfilled" || ev["data"] != result {
		t.Fatalf("fulfillEvent = %v, want RequestFulfilled carrying the result", ev)
	}
	if ev["subscriptionId"] != int64(1234) {
		t.Fatalf("fulfillEvent subscriptionId = %v, want 1234", ev["subscriptionId"])
	}
	txReqID, _ := ev["requestId"].(string)
	if !strings.HasPrefix(txReqID, "0x") || len(txReqID) != 64+2 ||
		!strings.HasSuffix(txReqID, strconv.FormatInt(6000000001, 16)) {
		t.Fatalf("fulfillEvent requestId = %q, want the request id zero-padded to bytes32", txReqID)
	}
	gasUsed := clNum(ev["gasUsed"])
	if gasUsed <= 0 || gasUsed > 400000 {
		t.Fatalf("gasUsed = %d, want within the request's gas limit", gasUsed)
	}
	packed := new(big.Int).Lsh(big.NewInt(137), 64) // polygon chain id
	packed.Add(packed, big.NewInt(gasUsed))
	if ev["gasUsedAndChainIdCode"] != packed.String() {
		t.Fatalf("gasUsedAndChainIdCode = %v, want %s (chainId << 64 | gasUsed)",
			ev["gasUsedAndChainIdCode"], packed.String())
	}

	// ===== simulate_fail lands the real fulfillment-code vocabulary =====
	failKind := func(kind string) map[string]any {
		t.Helper()
		r := f.call("functions", "on_create_request", "POST", "/v2/functions/createRequest",
			nil, nil, map[string]any{"simulate_fail": kind}, clBearer)
		if r.Status != 200 {
			t.Fatalf("createRequest simulate_fail=%q -> %d: %v", kind, r.Status, r.Body)
		}
		f.vc.Advance(3 * time.Second)
		fid := r.Body["requestID"].(string)
		p := f.call("functions", "on_get_request", "GET", "/v2/functions/request/"+fid,
			map[string]string{"requestID": fid}, nil, nil, clBearer)
		if p.Status != 200 {
			t.Fatalf("poll simulate_fail=%q -> %d: %v", kind, p.Status, p.Body)
		}
		return p.Body
	}
	js := failKind("js_error")
	if js["status"] != "failed" || js["fulfillmentCode"] != int64(2) ||
		js["fulfillmentCodeName"] != "FULFILLMENT_CODE_COMPUTED_FAILED" ||
		js["errorMessage"] != "code 2: Uncaught exception inside Functions source" {
		t.Fatalf("js_error failure = %v", js)
	}
	bal := failKind("balance")
	if bal["fulfillmentCode"] != int64(3) ||
		bal["fulfillmentCodeName"] != "FULFILLMENT_CODE_COST_EXCEEDS_COMMITMENT" ||
		!strings.HasPrefix(bal["errorMessage"].(string), "code 3:") {
		t.Fatalf("balance failure = %v", bal)
	}
	dflt := failKind("not-a-known-kind")
	if dflt["fulfillmentCode"] != int64(2) ||
		dflt["errorMessage"] != "code 2: computation exceeded" {
		t.Fatalf("unknown failure kind = %v, want the computation default", dflt)
	}
	noReq := f.call("functions", "on_get_request", "GET", "/v2/functions/request/6000000999",
		map[string]string{"requestID": "6000000999"}, nil, nil, clBearer)
	if noReq.Status != 404 {
		t.Fatalf("unknown request -> %d, want 404", noReq.Status)
	}
}

// TestChainlinkAutomationRegistryLifecycle: the keepers-registry shape —
// registration validation and juels funding, cadence performs that drain the
// balance one premium at a time until the LINK runs out, checkUpkeep gating,
// manual performs (premium-charged, chronologically interleaved with cadence
// performs), the cancel/withdraw freeze, and the derived upkeep list.
func TestChainlinkAutomationRegistryLifecycle(t *testing.T) {
	base := clBase()
	f := newChainlinkFixture(t, base)
	register := func(body map[string]any) starlark.Response {
		return f.call("automation", "on_register_upkeep", "POST", "/v2/automation/registerUpkeep",
			nil, nil, body, clBearer)
	}

	// ===== registration validates the registry shape and funds in juels =====
	noName := register(map[string]any{})
	if noName.Status != 400 {
		t.Fatalf("register without name -> %d, want 400", noName.Status)
	}
	if code, _ := clErr(t, noName); code != "BAD_REQUEST" {
		t.Fatalf("no-name code = %q, want BAD_REQUEST", code)
	}
	badGas := register(map[string]any{"name": "x", "gasLimit": float64(6000000)})
	if badGas.Status != 400 {
		t.Fatalf("gasLimit above the registry max -> %d, want 400", badGas.Status)
	}
	// Week-long cadence: no automatic tick during this test.
	upk := register(map[string]any{
		"name": "price-guard", "triggerType": "cron", "amount": 5, "interval": 7 * 24 * 3600,
	})
	if upk.Status != 200 {
		t.Fatalf("registerUpkeep -> %d: %v", upk.Status, upk.Body)
	}
	if upk.Body["upkeepID"] != "9000000001" || upk.Body["status"] != "registered" {
		t.Fatalf("registration = %v, want id 9000000001 / registered", upk.Body)
	}
	if upk.Body["balance"] != clLink(5) {
		t.Fatalf("registered balance = %v, want 5 LINK in juels (%s)", upk.Body["balance"], clLink(5))
	}

	// ===== the keepers network performs on cadence until the LINK runs out =====
	// 60s cadence funded with exactly 1 LINK = four 0.25-LINK premiums; the
	// registry stops an upkeep whose balance cannot cover the premium.
	cad := register(map[string]any{"name": "cadence", "amount": 1, "interval": 60})
	cadID := cad.Body["upkeepID"].(string)
	f.vc.Advance(10 * time.Minute) // 10 cadence ticks elapse, only 4 are payable
	view := f.call("automation", "on_get_upkeep", "GET", "/v2/automation/"+cadID,
		map[string]string{"id": cadID}, nil, nil, clBearer)
	if view.Status != 200 {
		t.Fatalf("get upkeep -> %d: %v", view.Status, view.Body)
	}
	d := clData(t, view)
	if d["status"] != "active" || d["performedCount"] != int64(4) || d["balance"] != "0" {
		t.Fatalf("drained upkeep = %v, want active / 4 performs / 0 balance", d)
	}
	chk := f.call("automation", "on_check_upkeep", "GET", "/v2/automation/"+cadID+"/check",
		map[string]string{"id": cadID}, nil, nil, clBearer)
	if got := clData(t, chk)["upkeepNeeded"]; got != false {
		t.Fatalf("drained upkeep is still eligible: %v", chk.Body)
	}
	hist := f.call("automation", "on_list_performs", "GET", "/v2/automation/"+cadID+"/performs",
		map[string]string{"id": cadID}, nil, nil, clBearer)
	entries := clDataList(t, hist)
	if hist.Body["count"] != int64(4) || len(entries) != 4 {
		t.Fatalf("performed history = %d entries (count %v), want 4", len(entries), hist.Body["count"])
	}
	prev := int64(1) << 62
	for _, e := range entries {
		entry := e.(map[string]any)
		if entry["trigger"] != "auto" || entry["premiumJuels"] != clPremium {
			t.Fatalf("cadence entry = %v, want auto trigger + 0.25 LINK premium", entry)
		}
		if at := clNum(entry["performedAt"]); at >= prev {
			t.Fatalf("history not newest-first: %d after %d", at, prev)
		} else {
			prev = at
		}
		if tx, _ := entry["transactionHash"].(string); !strings.HasPrefix(tx, "0x") || len(tx) != 66 {
			t.Fatalf("perform tx = %q, want 0x-prefixed 32-byte hash", tx)
		}
		if g := clNum(entry["gasUsed"]); g <= 0 || g > 500000 {
			t.Fatalf("perform gasUsed = %d, want within the default gas limit", g)
		}
	}
	// Oldest entry is the first cadence tick (registration + 1 minute).
	if prev != base.Unix()+60 {
		t.Fatalf("oldest cadence perform at %d, want the first tick %d", prev, base.Unix()+60)
	}

	// ===== check gates eligibility and a manual perform charges the premium =====
	upkID := upk.Body["upkeepID"].(string)
	chk1 := clData(t, f.call("automation", "on_check_upkeep", "GET", "/v2/automation/"+upkID+"/check",
		map[string]string{"id": upkID}, nil, nil, clBearer))
	if chk1["upkeepNeeded"] != true {
		t.Fatalf("never-performed upkeep is not eligible: %v", chk1)
	}
	if pd, _ := chk1["performData"].(string); !strings.HasPrefix(pd, "0x") || len(pd) != 18 {
		t.Fatalf("performData = %q, want 0x-prefixed bytes", pd)
	}
	perf := f.call("automation", "on_perform_upkeep", "POST", "/v2/automation/"+upkID+"/perform",
		map[string]string{"id": upkID}, nil, map[string]any{}, clBearer)
	if perf.Status != 200 {
		t.Fatalf("perform -> %d: %v", perf.Status, perf.Body)
	}
	pr := clData(t, perf)
	if pr["performed"] != true {
		t.Fatalf("perform receipt = %v", pr)
	}
	wantBal := new(big.Int).Sub(mustBig(clLink(5)), mustBig(clPremium))
	if pr["balance"] != wantBal.String() {
		t.Fatalf("post-perform balance = %v, want %s (5 LINK - one premium)", pr["balance"], wantBal.String())
	}
	chk2 := clData(t, f.call("automation", "on_check_upkeep", "GET", "/v2/automation/"+upkID+"/check",
		map[string]string{"id": upkID}, nil, nil, clBearer))
	if chk2["upkeepNeeded"] != false {
		t.Fatalf("upkeep eligible immediately after a perform: %v", chk2)
	}
	again := f.call("automation", "on_perform_upkeep", "POST", "/v2/automation/"+upkID+"/perform",
		map[string]string{"id": upkID}, nil, map[string]any{}, clBearer)
	if code, _ := clErr(t, again); again.Status != 400 || code != "UPKEEP_NOT_NEEDED" {
		t.Fatalf("second perform = %d %q, want 400 UPKEEP_NOT_NEEDED", again.Status, code)
	}

	// ===== cadence performs interleave chronologically with manual ones =====
	// A manual perform before the first cadence tick must stay the OLDEST
	// entry once the network starts performing on cadence.
	mix := register(map[string]any{"name": "mixed", "amount": 10, "interval": 60})
	mixID := mix.Body["upkeepID"].(string)
	manual := f.call("automation", "on_perform_upkeep", "POST", "/v2/automation/"+mixID+"/perform",
		map[string]string{"id": mixID}, nil, map[string]any{}, clBearer)
	if manual.Status != 200 {
		t.Fatalf("manual perform before the first tick -> %d: %v", manual.Status, manual.Body)
	}
	f.vc.Advance(5 * time.Minute) // 5 cadence ticks
	mixView := clData(t, f.call("automation", "on_get_upkeep", "GET", "/v2/automation/"+mixID,
		map[string]string{"id": mixID}, nil, nil, clBearer))
	if mixView["performedCount"] != int64(6) {
		t.Fatalf("mixed upkeep performedCount = %v, want 6 (1 manual + 5 cadence)", mixView["performedCount"])
	}
	wantMixBal := new(big.Int).Sub(mustBig(clLink(10)),
		new(big.Int).Mul(big.NewInt(6), mustBig(clPremium)))
	if mixView["balance"] != wantMixBal.String() {
		t.Fatalf("mixed balance = %v, want %s (10 LINK - 6 premiums)", mixView["balance"], wantMixBal.String())
	}
	mixHist := clDataList(t, f.call("automation", "on_list_performs", "GET",
		"/v2/automation/"+mixID+"/performs", map[string]string{"id": mixID}, nil, nil, clBearer))
	if len(mixHist) != 6 {
		t.Fatalf("mixed history = %d entries, want 6", len(mixHist))
	}
	prev = int64(1) << 62
	for _, e := range mixHist {
		if at := clNum(e.(map[string]any)["performedAt"]); at >= prev {
			t.Fatalf("mixed history not newest-first: %d after %d", at, prev)
		} else {
			prev = at
		}
	}
	if mixHist[0].(map[string]any)["trigger"] != "auto" ||
		mixHist[len(mixHist)-1].(map[string]any)["trigger"] != "manual" {
		t.Fatalf("mixed history order = newest %v / oldest %v, want auto newest, manual oldest",
			mixHist[0], mixHist[len(mixHist)-1])
	}

	// ===== cancel freezes the upkeep and withdraw pays out exactly once =====
	fund0 := f.call("automation", "on_fund_upkeep", "POST", "/v2/automation/"+upkID+"/fund",
		map[string]string{"id": upkID}, nil, map[string]any{"amount": 0}, clBearer)
	if fund0.Status != 400 {
		t.Fatalf("fund zero -> %d, want 400", fund0.Status)
	}
	fund := f.call("automation", "on_fund_upkeep", "POST", "/v2/automation/"+upkID+"/fund",
		map[string]string{"id": upkID}, nil, map[string]any{"amount": 2}, clBearer)
	fd := clData(t, fund)
	wantBal = new(big.Int).Add(mustBig(clLink(5)), mustBig(clLink(2)))
	wantBal.Sub(wantBal, mustBig(clPremium))
	if fd["balance"] != wantBal.String() || fd["addedJuels"] != clLink(2) {
		t.Fatalf("fund receipt = %v, want balance %s + 2 LINK", fd, wantBal.String())
	}
	cancel := f.call("automation", "on_cancel_upkeep", "POST", "/v2/automation/"+upkID+"/cancel",
		map[string]string{"id": upkID}, nil, map[string]any{}, clBearer)
	cd := clData(t, cancel)
	if cd["status"] != "cancelled" || cd["balance"] != wantBal.String() {
		t.Fatalf("cancel receipt = %v, want cancelled with the balance retained", cd)
	}
	reCancel := f.call("automation", "on_cancel_upkeep", "POST", "/v2/automation/"+upkID+"/cancel",
		map[string]string{"id": upkID}, nil, map[string]any{}, clBearer)
	if code, _ := clErr(t, reCancel); reCancel.Status != 400 || code != "UPKEEP_ALREADY_CANCELLED" {
		t.Fatalf("re-cancel = %d %q, want 400 UPKEEP_ALREADY_CANCELLED", reCancel.Status, code)
	}
	for _, op := range []struct{ path, handler, code string }{
		{"/fund", "on_fund_upkeep", "UPKEEP_CANCELLED"},
		{"/perform", "on_perform_upkeep", "UPKEEP_CANCELLED"},
	} {
		r := f.call("automation", op.handler, "POST", "/v2/automation/"+upkID+op.path,
			map[string]string{"id": upkID}, nil, map[string]any{"amount": 1}, clBearer)
		if r.Status != 400 {
			t.Fatalf("cancelled upkeep %s -> %d, want 400", op.path, r.Status)
		}
		if code, _ := clErr(t, r); code != op.code {
			t.Fatalf("cancelled upkeep %s code = %q, want %s", op.path, code, op.code)
		}
	}
	wd := f.call("automation", "on_withdraw_upkeep", "POST", "/v2/automation/"+upkID+"/withdraw",
		map[string]string{"id": upkID}, nil, map[string]any{"to": "0xfeed00000000000000000000000000000000feed"}, clBearer)
	wdd := clData(t, wd)
	if wdd["amount"] != wantBal.String() || wdd["to"] != "0xfeed00000000000000000000000000000000feed" {
		t.Fatalf("withdraw receipt = %v, want %s juels to the target", wdd, wantBal.String())
	}
	wd2 := f.call("automation", "on_withdraw_upkeep", "POST", "/v2/automation/"+upkID+"/withdraw",
		map[string]string{"id": upkID}, nil, map[string]any{}, clBearer)
	if code, _ := clErr(t, wd2); wd2.Status != 400 || code != "NO_FUNDS" {
		t.Fatalf("second withdraw = %d %q, want 400 NO_FUNDS", wd2.Status, code)
	}
	wdActive := f.call("automation", "on_withdraw_upkeep", "POST", "/v2/automation/"+mixID+"/withdraw",
		map[string]string{"id": mixID}, nil, map[string]any{}, clBearer)
	if code, _ := clErr(t, wdActive); wdActive.Status != 400 || code != "UPKEEP_NOT_CANCELLED" {
		t.Fatalf("withdraw active upkeep = %d %q, want 400 UPKEEP_NOT_CANCELLED", wdActive.Status, code)
	}

	// ===== the upkeep list derives state for every entry and 404s unknown ids =====
	all := f.call("automation", "on_list_upkeeps", "GET", "/v2/automation/upkeeps",
		nil, nil, nil, clBearer)
	if all.Status != 200 {
		t.Fatalf("list upkeeps -> %d: %v", all.Status, all.Body)
	}
	listed := clDataList(t, all)
	if len(listed) != 3 {
		t.Fatalf("upkeep list has %d entries, want all 3", len(listed))
	}
	byID := map[string]map[string]any{}
	for _, e := range listed {
		m := e.(map[string]any)
		byID[m["upkeepID"].(string)] = m
	}
	if byID[upkID]["status"] != "cancelled" || byID[cadID]["status"] != "active" {
		t.Fatalf("listed statuses = %v", byID)
	}
	if byID[mixID]["performedCount"] != int64(6) {
		t.Fatalf("listed mixed performedCount = %v, want the derived 6", byID[mixID]["performedCount"])
	}
	unknown := f.call("automation", "on_get_upkeep", "GET", "/v2/automation/777",
		map[string]string{"id": "777"}, nil, nil, clBearer)
	if code, _ := clErr(t, unknown); unknown.Status != 404 || code != "NOT_FOUND" {
		t.Fatalf("unknown upkeep = %d %q, want 404 NOT_FOUND", unknown.Status, code)
	}
}

// TestChainlinkCCIPMessagesAndLanes: the Bearer-gated CCIP surface — the
// synthetic cross-chain message list and the lane status echoing the
// requested pair with its ramp addresses.
func TestChainlinkCCIPMessagesAndLanes(t *testing.T) {
	f := newChainlinkFixture(t, clBase())

	// ===== ccip messages require the bearer token =====
	noAuth := f.call("ccip", "on_list_messages", "GET", "/v2/ccip/messages", nil, nil, nil, "")
	if code, _ := clErr(t, noAuth); noAuth.Status != 401 || code != "UNAUTHORIZED" {
		t.Fatalf("ccip without bearer -> %d %q, want 401 UNAUTHORIZED", noAuth.Status, code)
	}
	msgs := f.call("ccip", "on_list_messages", "GET", "/v2/ccip/messages", nil, nil, nil, clBearer)
	if msgs.Status != 200 {
		t.Fatalf("ccip messages -> %d: %v", msgs.Status, msgs.Body)
	}
	list := clDataList(t, msgs)
	if len(list) != 2 {
		t.Fatalf("ccip messages = %d entries, want 2", len(list))
	}
	m0 := list[0].(map[string]any)
	if m0["messageID"] != "0xccip-001" || m0["srcChain"] != "ethereum" ||
		m0["dstChain"] != "arbitrum" || m0["status"] != "delivered" {
		t.Fatalf("first message = %v", m0)
	}
	toks := m0["tokenAmounts"].([]any)
	if len(toks) != 1 || toks[0].(map[string]any)["token"] != "LINK" ||
		toks[0].(map[string]any)["amount"] != "1000000000000000000" {
		t.Fatalf("first message tokenAmounts = %v, want 1 LINK (string juels)", toks)
	}
	if list[1].(map[string]any)["status"] != "in_flight" {
		t.Fatalf("second message = %v, want in_flight", list[1])
	}

	// ===== lane status echoes the requested pair with its ramp addresses =====
	lane := f.call("ccip", "on_lane", "GET", "/v2/ccip/lane/ethereum/arbitrum",
		map[string]string{"src": "ethereum", "dst": "arbitrum"}, nil, nil, clBearer)
	if lane.Status != 200 {
		t.Fatalf("ccip lane -> %d: %v", lane.Status, lane.Body)
	}
	ln := clData(t, lane)
	if ln["srcChain"] != "ethereum" || ln["dstChain"] != "arbitrum" || ln["status"] != "active" {
		t.Fatalf("lane = %v", ln)
	}
	if ln["onRamp"] != "0x"+strings.Repeat("a1", 20) || ln["offRamp"] != "0x"+strings.Repeat("b2", 20) {
		t.Fatalf("lane ramps = %v / %v, want 20-byte addresses", ln["onRamp"], ln["offRamp"])
	}
	if toks, _ := ln["supportedTokens"].([]any); len(toks) != 2 ||
		toks[0] != "LINK" || toks[1] != "USDC" {
		t.Fatalf("supportedTokens = %v", ln["supportedTokens"])
	}
	// Synthetic stance, asserted as-is: even an unknown chain pair reports an
	// active lane (no lane validation; see the deviation report).
	anyLane := f.call("ccip", "on_lane", "GET", "/v2/ccip/lane/nonexistent/sidechain",
		map[string]string{"src": "nonexistent", "dst": "sidechain"}, nil, nil, clBearer)
	if anyLane.Status != 200 || clData(t, anyLane)["status"] != "active" {
		t.Fatalf("unknown lane pair -> %d %v, want the synthetic active lane", anyLane.Status, anyLane.Body)
	}
}

// TestChainlinkTokenVirtualExpiry: the well-known test token is seeded with a
// ten-year window computed at first use — the virtual clock kills it without
// waiting a decade.
func TestChainlinkTokenVirtualExpiry(t *testing.T) {
	f := newChainlinkFixture(t, clBase())

	// ===== the seeded test token dies after its ten-year virtual window =====
	ok := f.call("ccip", "on_list_messages", "GET", "/v2/ccip/messages", nil, nil, nil, clBearer)
	if ok.Status != 200 {
		t.Fatalf("seeded token -> %d: %v", ok.Status, ok.Body)
	}
	f.vc.Advance((10*365 + 1) * 24 * time.Hour)
	expired := f.call("ccip", "on_list_messages", "GET", "/v2/ccip/messages", nil, nil, nil, clBearer)
	if code, msg := clErr(t, expired); expired.Status != 401 || code != "UNAUTHORIZED" ||
		!strings.HasPrefix(msg, "Invalid or expired API token: ") {
		t.Fatalf("token after 10 virtual years -> %d %q %q, want 401 expired", expired.Status, code, msg)
	}
}
