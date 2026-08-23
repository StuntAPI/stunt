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

// Drives the walletconnect-style adapter script directly (lib.star preloaded)
// over a shared store and virtual clock: the pairing -> propose -> approve ->
// JSON-RPC request -> extend -> disconnect relay lifecycle, wc: URI parsing,
// the auto-paired symKey, the derived eip155 namespaces, the bare-array
// session list with limit paging, the {error, message} envelopes, and the
// unenforced projectId gate.

const (
	wcHost          = "relay.walletconnect.test"
	wcWallet        = "0x1234567890abcdef1234567890abcdef12345678"
	wcPairingExpiry = 2592000 // 30-day pairing TTL, seconds
	wcSessionExpiry = 604800  // 7-day session TTL, seconds
)

// 64-hex fixtures for the wc: URI round-trip (topics/symKeys carry no 0x).
var (
	wcTopicURI  = strings.Repeat("a1b2c3d4", 8)
	wcSymKeyURI = strings.Repeat("9f8e7d6c", 8)
)

func wcBase() time.Time { return time.Unix(1_750_000_000, 0).UTC() }

type walletconnectFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vm   *starlark.VM
	host string
}

func newWalletconnectFixture(t *testing.T, start time.Time) *walletconnectFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "walletconnect-style")
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
	src, err := os.ReadFile(filepath.Join(root, "scripts", "relay.star"))
	if err != nil {
		t.Fatalf("read relay.star: %v", err)
	}
	vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
	if err != nil {
		t.Fatalf("LoadWithLib relay.star: %v", err)
	}
	return &walletconnectFixture{t: t, vc: vc, vm: vm, host: wcHost}
}

func (f *walletconnectFixture) call(handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
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
	resp, err := f.vm.Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// sessions lists the bare-array session index keyed by topic.
func (f *walletconnectFixture) sessions(query map[string]string) map[string]map[string]any {
	f.t.Helper()
	r := f.call("on_list_sessions", "GET", "/v1/sessions", nil, query, nil, "")
	if r.Status != 200 {
		f.t.Fatalf("list sessions -> %d: %v", r.Status, r.Body)
	}
	byTopic := map[string]map[string]any{}
	for _, e := range r.BodyList {
		m, ok := e.(map[string]any)
		if !ok {
			f.t.Fatalf("session entry = %T, want an object", e)
		}
		topic, _ := m["topic"].(string)
		if topic == "" {
			f.t.Fatalf("session entry has no topic: %v", m)
		}
		byTopic[topic] = m
	}
	return byTopic
}

// wcNum reads a response number as int64 whether the adapter produced a
// Starlark int (computed fresh in this call) or a float (read back through
// the JSON document store) — both marshal to the same JSON number on the wire.
func wcNum(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// wcHex reports whether s is exactly n lowercase hex characters.
func wcHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// wcHash reports whether s is a 0x-prefixed 64-hex synthetic hash.
func wcHash(s string) bool {
	return strings.HasPrefix(s, "0x") && wcHex(strings.TrimPrefix(s, "0x"), 64)
}

// TestWalletconnectPairingAndSessionLifecycle: the pairing surface (URI
// round-trip, malformed-URI rejects, auto-generated pairings), session
// proposal + approval with derived eip155 namespaces, and the bare-array
// session list with limit paging.
func TestWalletconnectPairingAndSessionLifecycle(t *testing.T) {
	f := newWalletconnectFixture(t, wcBase())

	// ===== every route answers without a projectId (the gate is not wired) =====
	// The manifest declares identity.token_scheme: bearer and lib.star ships
	// _require_project_id, but no handler calls the helper — the relay answers
	// with no credential at all. Asserted as-is; see the deviation report.
	if r := f.call("on_create_pairing", "POST", "/v1/pairings", nil, nil, map[string]any{}, ""); r.Status != 200 {
		t.Fatalf("pairing with no projectId -> %d, want 200 (gate unenforced): %v", r.Status, r.Body)
	}
	if r := f.call("on_create_pairing", "POST", "/v1/pairings", nil, nil, map[string]any{}, "Bearer not-a-project-id"); r.Status != 200 {
		t.Fatalf("pairing with a bogus bearer -> %d, want 200 (gate unenforced): %v", r.Status, r.Body)
	}
	if r := f.call("on_list_sessions", "GET", "/v1/sessions", nil, nil, nil, ""); r.Status != 200 {
		t.Fatalf("list with no projectId -> %d, want 200 (gate unenforced)", r.Status)
	}

	// ===== a wc: URI pairing round-trips its topic, relay protocol, and symKey =====
	paired := f.call("on_create_pairing", "POST", "/v1/pairings", nil, nil, map[string]any{
		"uri": "wc:" + wcTopicURI + "@2?relay-protocol=irn&symKey=" + wcSymKeyURI,
	}, "")
	if paired.Status != 200 {
		t.Fatalf("pairing from URI -> %d: %v", paired.Status, paired.Body)
	}
	if paired.Body["topic"] != wcTopicURI {
		t.Fatalf("pairing topic = %v, want the URI topic %s", paired.Body["topic"], wcTopicURI)
	}
	relay, _ := paired.Body["relay"].(map[string]any)
	if relay["protocol"] != "irn" {
		t.Fatalf("relay protocol = %v, want irn", relay["protocol"])
	}
	if got := wcNum(paired.Body["expiry"]); got != wcPairingExpiry {
		t.Fatalf("pairing expiry = %v, want the 30-day TTL %d", paired.Body["expiry"], wcPairingExpiry)
	}
	state, _ := paired.Body["state"].(map[string]any)
	if state["symKey"] != wcSymKeyURI {
		t.Fatalf("pairing symKey = %v, want the URI symKey", state["symKey"])
	}
	// A URI naming a different relay protocol is echoed, not forced to irn.
	other := f.call("on_create_pairing", "POST", "/v1/pairings", nil, nil, map[string]any{
		"uri": "wc:" + strings.Repeat("0f1e2d3c", 8) + "@2?relay-protocol=custom&symKey=" + strings.Repeat("55667788", 8),
	}, "")
	otherRelay, _ := other.Body["relay"].(map[string]any)
	if other.Status != 200 || otherRelay["protocol"] != "custom" {
		t.Fatalf("custom relay-protocol URI -> %d %v, want the echoed protocol", other.Status, other.Body)
	}
	// Malformed URIs (wrong scheme, missing @version, empty topic) are 400s.
	for _, bad := range []string{
		"https://wallet.example.test/uri", // not a wc: URI
		"wc:topic-without-a-version",      // no @2
		"wc:@2?symKey=abc",                // empty topic before @2
	} {
		r := f.call("on_create_pairing", "POST", "/v1/pairings", nil, nil, map[string]any{"uri": bad}, "")
		if r.Status != 400 {
			t.Fatalf("uri %q -> %d, want 400", bad, r.Status)
		}
		if r.Body["error"] != "invalid_uri" {
			t.Fatalf("uri %q error = %v, want invalid_uri", bad, r.Body["error"])
		}
		if msg, _ := r.Body["message"].(string); msg == "" {
			t.Fatalf("uri %q carries no message: %v", bad, r.Body)
		}
	}

	// ===== an auto pairing mints a fresh topic and a 64-hex symKey =====
	auto1 := f.call("on_create_pairing", "POST", "/v1/pairings", nil, nil, map[string]any{}, "")
	if auto1.Status != 200 {
		t.Fatalf("auto pairing -> %d: %v", auto1.Status, auto1.Body)
	}
	autoTopic1, _ := auto1.Body["topic"].(string)
	autoState1, _ := auto1.Body["state"].(map[string]any)
	autoSym1, _ := autoState1["symKey"].(string)
	if !wcHex(autoTopic1, 64) || autoTopic1 == wcTopicURI {
		t.Fatalf("auto topic = %q, want a fresh 64-hex topic", autoTopic1)
	}
	if !wcHex(autoSym1, 64) {
		t.Fatalf("auto symKey = %q, want a minted 64-hex key (never empty)", autoSym1)
	}
	auto2 := f.call("on_create_pairing", "POST", "/v1/pairings", nil, nil, map[string]any{}, "")
	autoTopic2, _ := auto2.Body["topic"].(string)
	autoState2, _ := auto2.Body["state"].(map[string]any)
	if autoTopic2 == autoTopic1 || autoState2["symKey"] == autoSym1 {
		t.Fatalf("second auto pairing reuses topic/symKey: %q vs %q", autoTopic2, autoTopic1)
	}

	// ===== proposing requires pairingTopic — and accepts one never paired =====
	noTopic := f.call("on_propose_session", "POST", "/v1/sessions", nil, nil, map[string]any{}, "")
	if noTopic.Status != 400 {
		t.Fatalf("propose without pairingTopic -> %d, want 400", noTopic.Status)
	}
	if noTopic.Body["error"] != "missing_pairingTopic" {
		t.Fatalf("propose error = %v, want missing_pairingTopic", noTopic.Body["error"])
	}
	// The simulated wallet never consults the pairings store: an unknown
	// pairingTopic still proposes. Asserted as-is; see the deviation report.
	ghost := f.call("on_propose_session", "POST", "/v1/sessions", nil, nil,
		map[string]any{"pairingTopic": strings.Repeat("ee", 32)}, "")
	if ghost.Status != 200 {
		t.Fatalf("propose on an unknown pairingTopic -> %d, want 200 (no pairing check): %v", ghost.Status, ghost.Body)
	}
	ghostTopic, _ := ghost.Body["topic"].(string)
	if !wcHex(ghostTopic, 64) {
		t.Fatalf("proposed topic = %q, want 64-hex", ghostTopic)
	}
	if ghost.Body["acknowledged"] != false || ghost.Body["pairingTopic"] != strings.Repeat("ee", 32) {
		t.Fatalf("proposal shape = %v, want acknowledged:false + echoed pairingTopic", ghost.Body)
	}
	if got := wcNum(ghost.Body["expiry"]); got != wcSessionExpiry {
		t.Fatalf("proposal expiry = %v, want the 7-day TTL %d", ghost.Body["expiry"], wcSessionExpiry)
	}
	if ns, _ := ghost.Body["namespaces"].(map[string]any); len(ns) != 0 {
		t.Fatalf("proposal namespaces = %v, want empty until approved", ghost.Body["namespaces"])
	}
	proposed := f.call("on_propose_session", "POST", "/v1/sessions", nil, nil, map[string]any{
		"pairingTopic": wcTopicURI,
		"requiredNamespaces": map[string]any{
			"eip155": map[string]any{
				"chains":  []any{"eip155:137", "eip155:10"},
				"methods": []any{"eth_signTypedData_v4"},
				"events":  []any{"chainChanged"},
			},
		},
	}, "")
	if proposed.Status != 200 {
		t.Fatalf("propose -> %d: %v", proposed.Status, proposed.Body)
	}
	topicA, _ := proposed.Body["topic"].(string)
	if topicA == "" || topicA == wcTopicURI {
		t.Fatalf("session topic = %q, want its own topic distinct from the pairing", topicA)
	}

	// ===== approve acknowledges the session and derives eip155 namespaces =====
	approved := f.call("on_approve_session", "POST", "/v1/sessions/"+topicA+"/approve",
		map[string]string{"topic": topicA}, nil, map[string]any{}, "")
	if approved.Status != 200 {
		t.Fatalf("approve -> %d: %v", approved.Status, approved.Body)
	}
	if approved.Body["topic"] != topicA || approved.Body["acknowledged"] != true {
		t.Fatalf("approval = %v, want acknowledged:true on its topic", approved.Body)
	}
	ns, _ := approved.Body["namespaces"].(map[string]any)
	eip, _ := ns["eip155"].(map[string]any)
	accounts, _ := eip["accounts"].([]any)
	if len(accounts) != 2 || accounts[0] != "eip155:137:"+wcWallet || accounts[1] != "eip155:10:"+wcWallet {
		t.Fatalf("derived accounts = %v, want one per required chain suffixed with the wallet", accounts)
	}
	if m, _ := eip["methods"].([]any); len(m) != 1 || m[0] != "eth_signTypedData_v4" {
		t.Fatalf("derived methods = %v, want the required set", eip["methods"])
	}
	if ev, _ := eip["events"].([]any); len(ev) != 1 || ev[0] != "chainChanged" {
		t.Fatalf("derived events = %v, want the required set", eip["events"])
	}
	// Without requiredNamespaces the wallet falls back to its defaults.
	dflt := f.call("on_propose_session", "POST", "/v1/sessions", nil, nil,
		map[string]any{"pairingTopic": autoTopic1}, "")
	if dflt.Status != 200 {
		t.Fatalf("propose defaults -> %d: %v", dflt.Status, dflt.Body)
	}
	dfltTopic, _ := dflt.Body["topic"].(string)
	def := f.call("on_approve_session", "POST", "/v1/sessions/"+dfltTopic+"/approve",
		map[string]string{"topic": dfltTopic}, nil, map[string]any{}, "")
	if def.Status != 200 {
		t.Fatalf("approve defaults -> %d: %v", def.Status, def.Body)
	}
	defNS, _ := def.Body["namespaces"].(map[string]any)
	defEip, _ := defNS["eip155"].(map[string]any)
	if a, _ := defEip["accounts"].([]any); len(a) != 1 || a[0] != "eip155:1:"+wcWallet {
		t.Fatalf("default accounts = %v, want eip155:1:<wallet>", defEip["accounts"])
	}
	if m, _ := defEip["methods"].([]any); len(m) != 2 {
		t.Fatalf("default methods = %v, want the 2 wallet defaults", defEip["methods"])
	}
	// Unknown topics answer the documented 404 envelope.
	unknown := f.call("on_approve_session", "POST", "/v1/sessions/dead00beef/approve",
		map[string]string{"topic": "dead00beef"}, nil, map[string]any{}, "")
	if unknown.Status != 404 || unknown.Body["error"] != "session_not_found" {
		t.Fatalf("approve unknown topic -> %d %v, want 404 session_not_found", unknown.Status, unknown.Body)
	}
	if msg, _ := unknown.Body["message"].(string); !strings.Contains(msg, "dead00beef") {
		t.Fatalf("approve unknown message = %q, want it to name the topic", msg)
	}

	// ===== the session list is a bare array capped by limit =====
	all := f.sessions(nil)
	if len(all) != 3 {
		t.Fatalf("session list has %d entries, want all three proposals", len(all))
	}
	a := all[topicA]
	if a["acknowledged"] != true {
		t.Fatalf("listed approved session = %v, want acknowledged:true", a)
	}
	if ns, _ := a["namespaces"].(map[string]any); len(ns) == 0 {
		t.Fatalf("listed approved session has empty namespaces: %v", a)
	}
	if got := wcNum(a["expiry"]); got != wcSessionExpiry {
		t.Fatalf("listed expiry = %v, want %d", a["expiry"], wcSessionExpiry)
	}
	if g := all[ghostTopic]; g["acknowledged"] != false {
		t.Fatalf("listed unapproved session = %v, want acknowledged:false", g)
	}
	page1 := f.call("on_list_sessions", "GET", "/v1/sessions", nil, map[string]string{"limit": "1"}, nil, "")
	if page1.Status != 200 || len(page1.BodyList) != 1 {
		t.Fatalf("limit=1 -> %d (%d entries), want a 1-entry page", page1.Status, len(page1.BodyList))
	}
	// The page is a bare array: no envelope object, and no cursor token is
	// surfaced anywhere (the offset token must be guessed). Asserted as-is;
	// see the deviation report.
	if page1.Body != nil {
		t.Fatalf("paged list carries an envelope object: %v", page1.Body)
	}
	t1, _ := page1.BodyList[0].(map[string]any)["topic"].(string)
	page2 := f.call("on_list_sessions", "GET", "/v1/sessions", nil,
		map[string]string{"limit": "1", "cursor": "1"}, nil, "")
	if page2.Status != 200 || len(page2.BodyList) != 1 {
		t.Fatalf("cursor=1 -> %d (%d entries), want the other 1-entry page", page2.Status, len(page2.BodyList))
	}
	t2, _ := page2.BodyList[0].(map[string]any)["topic"].(string)
	known := map[string]bool{topicA: true, ghostTopic: true, dfltTopic: true}
	if !known[t1] || !known[t2] || t1 == t2 {
		t.Fatalf("pages = %q / %q, want two distinct listed sessions", t1, t2)
	}
	badCursor := f.call("on_list_sessions", "GET", "/v1/sessions", nil,
		map[string]string{"cursor": "not-a-cursor"}, nil, "")
	if badCursor.Status != 400 || badCursor.Body["error"] != "invalid_cursor" {
		t.Fatalf("invalid cursor -> %d %v, want 400 invalid_cursor", badCursor.Status, badCursor.Body)
	}
}

// TestWalletconnectSessionRequestLifecycle: the auto-approving wallet's
// JSON-RPC surface — the 2.0 envelope with globally monotonic ids, synthetic
// per-method hashes, the missing approval gate, the fixed-TTL extend, and the
// disconnect that retires the topic.
func TestWalletconnectSessionRequestLifecycle(t *testing.T) {
	f := newWalletconnectFixture(t, wcBase())
	propose := func(pairingTopic string) string {
		r := f.call("on_propose_session", "POST", "/v1/sessions", nil, nil,
			map[string]any{"pairingTopic": pairingTopic}, "")
		if r.Status != 200 {
			t.Fatalf("propose -> %d: %v", r.Status, r.Body)
		}
		topic, _ := r.Body["topic"].(string)
		if topic == "" {
			t.Fatalf("proposal carries no topic: %v", r.Body)
		}
		return topic
	}
	request := func(topic, method string, params []any) starlark.Response {
		return f.call("on_session_request", "POST", "/v1/sessions/"+topic+"/request",
			map[string]string{"topic": topic}, nil,
			map[string]any{"request": map[string]any{"method": method, "params": params}}, "")
	}
	params := []any{"0x48656c6c6f", wcWallet}
	topicA := propose(strings.Repeat("aa", 32)) // paired and approved below
	topicB := propose(strings.Repeat("bb", 32)) // never approved
	if r := f.call("on_approve_session", "POST", "/v1/sessions/"+topicA+"/approve",
		map[string]string{"topic": topicA}, nil, map[string]any{}, ""); r.Status != 200 {
		t.Fatalf("approve -> %d: %v", r.Status, r.Body)
	}

	// ===== the approval gate is missing: an unacknowledged session still answers =====
	// topicB was never approved; the auto-wallet serves it anyway. Asserted
	// as-is; see the deviation report.
	if r := request(topicB, "eth_requestAccounts", []any{}); r.Status != 200 {
		t.Fatalf("request on an unapproved session -> %d, want 200 (no approval gate): %v", r.Status, r.Body)
	}

	// ===== wallet requests answer in a JSON-RPC 2.0 envelope with monotonic ids =====
	// Batch JSON-RPC arrays are not modeled: one request object per call.
	id0 := wcNum(request(topicB, "eth_requestAccounts", []any{}).Body["id"])
	acc := request(topicA, "eth_requestAccounts", []any{})
	if acc.Status != 200 {
		t.Fatalf("eth_requestAccounts -> %d: %v", acc.Status, acc.Body)
	}
	if acc.Body["jsonrpc"] != "2.0" || acc.Body["topic"] != topicA {
		t.Fatalf("envelope = %v, want jsonrpc 2.0 on its topic", acc.Body)
	}
	if res, _ := acc.Body["result"].([]any); len(res) != 1 || res[0] != wcWallet {
		t.Fatalf("eth_requestAccounts result = %v, want [<wallet>]", acc.Body["result"])
	}
	if wcNum(acc.Body["id"]) != id0+1 {
		t.Fatalf("eth_requestAccounts id = %v after %d, want the next global id", acc.Body["id"], id0)
	}
	// The id sequence is global (kv-backed) and increments per call — across
	// sessions, not per topic.
	accts := request(topicA, "eth_accounts", []any{})
	if wcNum(accts.Body["id"]) != id0+2 || accts.Body["jsonrpc"] != "2.0" {
		t.Fatalf("eth_accounts id = %v after %d, want the next global id", accts.Body["id"], id0+2)
	}
	if res, _ := accts.Body["result"].([]any); len(res) != 1 || res[0] != wcWallet {
		t.Fatalf("eth_accounts result = %v, want [<wallet>]", accts.Body["result"])
	}

	// ===== signing and transaction methods return synthetic 0x-hex hashes =====
	for _, method := range []string{"personal_sign", "eth_sendTransaction", "eth_sign"} {
		r := request(topicA, method, params)
		if r.Status != 200 {
			t.Fatalf("%s -> %d: %v", method, r.Status, r.Body)
		}
		sig, ok := r.Body["result"].(string)
		if !ok || !wcHash(sig) {
			t.Fatalf("%s result = %v, want a 0x-prefixed 64-hex hash", method, r.Body["result"])
		}
	}
	// Unknown methods fall through to the same synthetic hash shape.
	if r := request(topicA, "eth_signTypedData_v4", params); r.Status != 200 || !wcHash(r.Body["result"].(string)) {
		t.Fatalf("unknown method -> %d %v, want the default synthetic hash", r.Status, r.Body)
	}
	// Hashes are seeded by the request id, so the same method differs per call.
	s1, _ := request(topicA, "personal_sign", params).Body["result"].(string)
	s2, _ := request(topicA, "personal_sign", params).Body["result"].(string)
	if s1 == s2 || !wcHash(s1) || !wcHash(s2) {
		t.Fatalf("repeated personal_sign = %q / %q, want distinct id-seeded hashes", s1, s2)
	}
	// Unknown topics answer the same 404 envelope as the other actions.
	if r := request("dead00beef", "eth_requestAccounts", []any{}); r.Status != 404 || r.Body["error"] != "session_not_found" {
		t.Fatalf("request unknown topic -> %d %v, want 404 session_not_found", r.Status, r.Body)
	}

	// ===== extend echoes the fixed session TTL without persisting anything =====
	ext := f.call("on_extend_session", "POST", "/v1/sessions/"+topicA+"/extend",
		map[string]string{"topic": topicA}, nil, map[string]any{}, "")
	if ext.Status != 200 || ext.Body["topic"] != topicA {
		t.Fatalf("extend -> %d %v, want the echoed topic", ext.Status, ext.Body)
	}
	if got := wcNum(ext.Body["expiry"]); got != wcSessionExpiry {
		t.Fatalf("extend expiry = %v, want the 7-day TTL %d", ext.Body["expiry"], wcSessionExpiry)
	}
	// The stored doc is untouched — and the value is a TTL constant, not the
	// absolute unix timestamp real WC expiry uses. Asserted as-is; see the
	// deviation report.
	if got := wcNum(f.sessions(nil)[topicA]["expiry"]); got != wcSessionExpiry {
		t.Fatalf("stored expiry after extend = %v, want the unchanged %d", got, wcSessionExpiry)
	}
	if r := f.call("on_extend_session", "POST", "/v1/sessions/dead00beef/extend",
		map[string]string{"topic": "dead00beef"}, nil, map[string]any{}, ""); r.Status != 404 {
		t.Fatalf("extend unknown topic -> %d, want 404", r.Status)
	}

	// ===== disconnect retires the topic and every later call 404s =====
	del := f.call("on_disconnect_session", "DELETE", "/v1/sessions/"+topicA,
		map[string]string{"topic": topicA}, nil, map[string]any{}, "")
	if del.Status != 200 || del.Body["acknowledged"] != false {
		t.Fatalf("disconnect -> %d %v, want acknowledged:false", del.Status, del.Body)
	}
	if msg, _ := del.Body["message"].(string); msg != "session disconnected" {
		t.Fatalf("disconnect message = %q, want session disconnected", msg)
	}
	remaining := f.sessions(nil)
	if _, gone := remaining[topicA]; gone {
		t.Fatalf("session %s still listed after disconnect", topicA)
	}
	if _, kept := remaining[topicB]; !kept {
		t.Fatalf("disconnect also removed the unrelated session %s", topicB)
	}
	for _, c := range []struct{ handler, method, suffix string }{
		{"on_approve_session", "POST", "/approve"},
		{"on_session_request", "POST", "/request"},
		{"on_extend_session", "POST", "/extend"},
		{"on_disconnect_session", "DELETE", ""},
	} {
		r := f.call(c.handler, c.method, "/v1/sessions/"+topicA+c.suffix,
			map[string]string{"topic": topicA}, nil, map[string]any{}, "")
		if r.Status != 404 || r.Body["error"] != "session_not_found" {
			t.Fatalf("%s after disconnect -> %d %v, want 404 session_not_found", c.handler, r.Status, r.Body)
		}
		if msg, _ := r.Body["message"].(string); !strings.Contains(msg, topicA) {
			t.Fatalf("%s after disconnect message = %q, want it to name the topic", c.handler, msg)
		}
	}
}
