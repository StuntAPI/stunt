package adapters

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// Drives the helius-style adapter scripts directly (lib.star preloaded) over
// a shared store, a VIRTUAL clock and a real local webhook sink: the api-key
// query gate, the Solana JSON-RPC surface, the derive-on-read sendTransaction
// confirmation lifecycle (null -> processed -> confirmed -> finalized) with
// its exactly-once enhanced webhook, the Enhanced Transactions API (seeding,
// Helius parsed shapes, type/source filters, before/until/limit cursors), the
// parse-transactions contract, and the webhooks API.
const (
	heliusKey     = "helius-test-key"
	heliusSecret  = "helius-shared-secret"
	heliusAddrA   = "He1iusTestWa11et111111111111111111111111111"
	heliusAddrB   = "SecondTestWa11et77777777777777777777777777"
	heliusBase58  = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	heliusHalfSOL = float64(5000 * 1000 * 100) // 0.5 SOL in lamports
)

// heliusDelivery is one captured outbound webhook delivery.
type heliusDelivery struct {
	body    string
	headers http.Header
}

type heliusFixture struct {
	t       *testing.T
	vc      *clock.Clock
	vms     map[string]*starlark.VM
	mu      sync.Mutex
	sink    []heliusDelivery
	sinkURL string
}

func newHeliusFixture(t *testing.T, start time.Time) *heliusFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "helius-style")
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
	f := &heliusFixture{t: t, vc: vc}
	// Real (local) sink so the unsigned enhanced deliveries can be captured —
	// the same emitter the engine hands handlers.
	em := events.NewEmitter()
	t.Cleanup(em.Close)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.sink = append(f.sink, heliusDelivery{body: string(b), headers: r.Header.Clone()})
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sink.Close)
	f.sinkURL = sink.URL

	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: em,
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
		"rpc": load("rpc.star"), "enh": load("enhanced.star"), "hooks": load("webhooks.star"),
	}
	return f
}

func (f *heliusFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any) starlark.Response {
	f.t.Helper()
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: "mainnet.helius-rpc.test", Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// heliusQ builds a query map carrying the api-key plus optional extras.
func heliusQ(extra map[string]string) map[string]string {
	q := map[string]string{"api-key": heliusKey}
	for k, v := range extra {
		q[k] = v
	}
	return q
}

// rpc POSTs one JSON-RPC request body to the on_rpc handler.
func (f *heliusFixture) rpc(method string, params []any, id int64) starlark.Response {
	f.t.Helper()
	return f.call("rpc", "on_rpc", "POST", "/", nil, heliusQ(nil), map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
}

// sendTx submits sendTransaction with the simulator config object and
// returns the landed signature.
func (f *heliusFixture) sendTx(cfg map[string]any) string {
	f.t.Helper()
	params := []any{"base64-wire-transaction"}
	if cfg != nil {
		params = append(params, cfg)
	}
	resp := f.rpc("sendTransaction", params, 1)
	if resp.Status != 200 {
		f.t.Fatalf("sendTransaction -> %d: %v", resp.Status, resp.Body)
	}
	sig, _ := resp.Body["result"].(string)
	if sig == "" {
		f.t.Fatalf("sendTransaction returned no signature: %v", resp.Body)
	}
	return sig
}

// sigStatus polls getSignatureStatuses for one signature and returns the
// value item (nil when the RPC answers null for it).
func (f *heliusFixture) sigStatus(sig string) map[string]any {
	f.t.Helper()
	resp := f.rpc("getSignatureStatuses", []any{[]any{sig}}, 1)
	if resp.Status != 200 {
		f.t.Fatalf("getSignatureStatuses -> %d: %v", resp.Status, resp.Body)
	}
	vals := heliusList(f.t, heliusMap(f.t, resp.Body["result"], "result")["value"], "value")
	if len(vals) != 1 {
		f.t.Fatalf("getSignatureStatuses value = %v, want one item", vals)
	}
	if vals[0] == nil {
		return nil
	}
	return heliusMap(f.t, vals[0], "value[0]")
}

// addressTxs fetches the enhanced parsed history for an address.
func (f *heliusFixture) addressTxs(addr string, query map[string]string) []map[string]any {
	f.t.Helper()
	resp := f.call("enh", "on_get_address_transactions", "GET",
		"/v0/addresses/"+addr+"/transactions", map[string]string{"address": addr}, heliusQ(query), nil)
	if resp.Status != 200 {
		f.t.Fatalf("address transactions -> %d: %v", resp.Status, resp.Body)
	}
	out := []map[string]any{}
	for i, e := range resp.BodyList {
		out = append(out, heliusMap(f.t, e, "history["+strconv.Itoa(i)+"]"))
	}
	return out
}

// hookCall drives one webhook API endpoint.
func (f *heliusFixture) hookCall(handler, method, path string, params map[string]string, body map[string]any) starlark.Response {
	f.t.Helper()
	return f.call("hooks", handler, method, path, params, heliusQ(nil), body)
}

func (f *heliusFixture) deliveries() []heliusDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]heliusDelivery, len(f.sink))
	copy(out, f.sink)
	return out
}

// --- assertion helpers ---

// heliusNum compares a JSON number regardless of int64/float64 width (docs
// round-trip through the collection store, where ints come back floats).
func heliusNum(t *testing.T, v any, want float64, what string) {
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
		t.Fatalf("%s is %T(%v), want number %v", what, v, v, want)
	}
}

func heliusMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T(%v), want object", what, v, v)
	}
	return m
}

func heliusList(t *testing.T, v any, what string) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("%s is %T(%v), want array", what, v, v)
	}
	return l
}

// heliusIsBase58 checks the Solana base58 alphabet (and length).
func heliusIsBase58(t *testing.T, s, what string, wantLen int) {
	t.Helper()
	if len(s) != wantLen {
		t.Fatalf("%s length = %d, want %d", what, len(s), wantLen)
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(heliusBase58, rune(s[i])) {
			t.Fatalf("%s has non-base58 char %q at %d", what, s[i], i)
		}
	}
}

// heliusBalance mirrors lib.star's _balance_for_address so RPC and meta
// assertions can check the deterministic lamport figure. The Starlark hash
// is arbitrary-precision, so reduce mod 10^6 per character (composition
// holds); the address hash is positive, so Go's % matches Starlark's.
func heliusBalance(addr string) float64 {
	const m = 1000 * 1000
	h := int64(0)
	for _, c := range addr {
		h = (h*31 + int64(c)) % m
	}
	return float64(h * m)
}

// decodeDelivery unpacks the {type, payload} envelope of one delivery.
func decodeDelivery(t *testing.T, d heliusDelivery) (string, map[string]any) {
	t.Helper()
	var env struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(d.body), &env); err != nil {
		t.Fatalf("delivery body is not JSON: %v (body %s)", err, d.body)
	}
	return env.Type, env.Payload
}

// TestHeliusJSONRPCSurface: the api-key query gate on both API styles, the
// read methods, and the JSON-RPC error envelopes.
func TestHeliusJSONRPCSurface(t *testing.T) {
	f := newHeliusFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== the api-key query gate rejects both surfaces =====
	// RPC without a key: a JSON-RPC error envelope (code -32000), id null.
	r := f.call("rpc", "on_rpc", "POST", "/", nil, nil, map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "getBalance", "params": []any{heliusAddrA},
	})
	if r.Status != 401 {
		t.Fatalf("rpc without api-key -> %d, want 401", r.Status)
	}
	if r.Body["jsonrpc"] != "2.0" || r.Body["id"] != nil {
		t.Fatalf("rpc 401 envelope = %v", r.Body)
	}
	rpcErr := heliusMap(t, r.Body["error"], "rpc 401 error")
	heliusNum(t, rpcErr["code"], -32000, "rpc 401 error.code")
	if rpcErr["message"] != "Missing api-key" {
		t.Fatalf("rpc 401 message = %v", rpcErr["message"])
	}
	// Enhanced and webhook surfaces: bare {"error": ...} objects.
	if r := f.call("enh", "on_get_balances", "GET", "/v0/addresses/"+heliusAddrA+"/balances",
		map[string]string{"address": heliusAddrA}, nil, nil); r.Status != 401 || r.Body["error"] != "Missing api-key" {
		t.Fatalf("balances without api-key -> %d %v, want 401 {error: Missing api-key}", r.Status, r.Body)
	}
	if r := f.call("hooks", "on_create_webhook", "POST", "/v0/webhooks", nil, nil, map[string]any{
		"webhookURL": f.sinkURL,
	}); r.Status != 401 || r.Body["error"] != "Missing api-key" {
		t.Fatalf("create webhook without api-key -> %d %v, want 401 {error: Missing api-key}", r.Status, r.Body)
	}

	// ===== the JSON-RPC reads answer deterministic values; an unknown method is a -32601 envelope =====
	bal := f.rpc("getBalance", []any{heliusAddrA}, 7)
	if bal.Status != 200 || bal.Body["jsonrpc"] != "2.0" {
		t.Fatalf("getBalance envelope -> %d %v", bal.Status, bal.Body)
	}
	heliusNum(t, bal.Body["id"], 7, "getBalance id echo")
	balResult := heliusMap(t, bal.Body["result"], "getBalance result")
	heliusNum(t, balResult["value"], heliusBalance(heliusAddrA), "getBalance value (lamports)")
	balCtx := heliusMap(t, balResult["context"], "getBalance context")
	heliusNum(t, balCtx["slot"], 25000000, "getBalance context.slot")
	if balCtx["apiVersion"] != "1.18.0" {
		t.Fatalf("getBalance apiVersion = %v", balCtx["apiVersion"])
	}

	// Two blockhashes: fresh slot and blockhash each call.
	bh1 := heliusMap(t, heliusMap(t, f.rpc("getLatestBlockhash", nil, 8).Body["result"], "blockhash result"), "r1")
	bh2 := heliusMap(t, heliusMap(t, f.rpc("getLatestBlockhash", nil, 8).Body["result"], "blockhash result"), "r2")
	bh1v, bh2v := heliusMap(t, bh1["value"], "bh1.value"), heliusMap(t, bh2["value"], "bh2.value")
	heliusIsBase58(t, bh1v["blockhash"].(string), "blockhash 1", 44)
	heliusIsBase58(t, bh2v["blockhash"].(string), "blockhash 2", 44)
	if bh1v["blockhash"] == bh2v["blockhash"] {
		t.Fatalf("getLatestBlockhash repeated blockhash %v", bh1v["blockhash"])
	}
	heliusNum(t, bh1v["lastValidBlockHeight"], 2000*1000*100+1, "bh1 lastValidBlockHeight")
	heliusNum(t, bh2v["lastValidBlockHeight"], 2000*1000*100+2, "bh2 lastValidBlockHeight")
	heliusNum(t, heliusMap(t, bh2["context"], "bh2 context")["slot"], 25000002, "bh2 context.slot")

	// Token accounts by owner: two seeded accounts, filterable by mint.
	tok := f.rpc("getTokenAccountsByOwner", []any{heliusAddrA}, 9)
	accounts := heliusList(t, heliusMap(t, tok.Body["result"], "token result")["value"], "token value")
	if len(accounts) != 2 {
		t.Fatalf("getTokenAccountsByOwner = %d accounts, want the 2 seeded", len(accounts))
	}
	first := heliusMap(t, accounts[0], "token account 0")
	mint0, _ := first["mint"].(string)
	heliusIsBase58(t, mint0, "mint 0", 44)
	if first["owner"] != heliusAddrA || first["lamports"] == nil {
		t.Fatalf("token account 0 = %v", first)
	}
	info := heliusMap(t, heliusMap(t, heliusMap(t, heliusMap(t, first["data"], "data")["parsed"], "parsed")["info"], "info"), "info")
	ta := heliusMap(t, info["tokenAmount"], "tokenAmount")
	heliusNum(t, ta["decimals"], 6, "USDC decimals")
	if info["state"] != "initialized" {
		t.Fatalf("token account state = %v", info["state"])
	}
	filtered := heliusList(t, heliusMap(t, f.rpc("getTokenAccountsByOwner",
		[]any{heliusAddrA, map[string]any{"mint": mint0}}, 9).Body["result"], "filtered result")["value"], "filtered value")
	if len(filtered) != 1 || filtered[0].(map[string]any)["mint"] != mint0 {
		t.Fatalf("mint-filtered token accounts = %v, want only mint %s", filtered, mint0)
	}

	// Unknown method: HTTP 200 with the JSON-RPC -32601 error, id echoed.
	bad := f.rpc("getSlot", nil, 42)
	if bad.Status != 200 {
		t.Fatalf("unknown method -> HTTP %d, want 200 (JSON-RPC errors ride 200)", bad.Status)
	}
	if bad.Body["result"] != nil {
		t.Fatalf("unknown method result = %v, want null", bad.Body["result"])
	}
	heliusNum(t, bad.Body["id"], 42, "unknown method id echo")
	badErr := heliusMap(t, bad.Body["error"], "unknown method error")
	heliusNum(t, badErr["code"], -32601, "unknown method code")
	if badErr["message"] != "Method not found: getSlot" {
		t.Fatalf("unknown method message = %v", badErr["message"])
	}
}

// TestHeliusTransactionLifecycle: the derive-on-read confirmation walk driven
// by the virtual clock, the exactly-once enhanced webhook, getTransaction's
// full Solana shape, and failure injection.
func TestHeliusTransactionLifecycle(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newHeliusFixture(t, base)

	// A hook subscribing to everything, with the authHeader shared secret.
	if r := f.call("hooks", "on_create_webhook", "POST", "/v0/webhooks", nil, heliusQ(nil), map[string]any{
		"webhookURL": f.sinkURL, "authHeader": heliusSecret,
	}); r.Status != 201 {
		t.Fatalf("create webhook -> %d: %v", r.Status, r.Body)
	}

	sig := f.sendTx(map[string]any{"simulate_address": heliusAddrA})

	// ===== sendTransaction walks null → processed → confirmed → finalized, firing the enhanced webhook exactly once at first confirmation =====
	// t=0: a just-submitted signature has no status yet (real RPC answers null).
	if st := f.sigStatus(sig); st != nil {
		t.Fatalf("status at t=0 = %v, want null", st)
	}
	if got := len(f.deliveries()); got != 0 {
		t.Fatalf("deliveries before confirmation = %d, want 0", got)
	}

	f.vc.Advance(1500 * time.Millisecond) // 1.5s: processed
	st := f.sigStatus(sig)
	if st["confirmationStatus"] != "processed" {
		t.Fatalf("status at 1.5s = %v, want processed", st["confirmationStatus"])
	}
	heliusNum(t, st["confirmations"], 0, "processed confirmations")
	heliusNum(t, st["slot"], 25000001, "processed slot")
	if _, has := heliusMap(t, st["status"], "status")["Err"]; has {
		t.Fatalf("processed status = %v, want Ok", st["status"])
	}
	if got := len(f.deliveries()); got != 0 {
		t.Fatalf("deliveries at processed = %d, want 0 (webhook fires on confirmation only)", got)
	}

	f.vc.Advance(1 * time.Second) // 2.5s: confirmed
	st = f.sigStatus(sig)
	if st["confirmationStatus"] != "confirmed" {
		t.Fatalf("status at 2.5s = %v, want confirmed", st["confirmationStatus"])
	}
	heliusNum(t, st["confirmations"], 31, "confirmed confirmations")
	// The delivery fired exactly once, on first confirmation.
	dv := f.deliveries()
	if len(dv) != 1 {
		t.Fatalf("deliveries at confirmed = %d, want exactly one", len(dv))
	}
	kind, payload := decodeDelivery(t, dv[0])
	if kind != "TRANSFER" || payload["signature"] != sig {
		t.Fatalf("delivery = %s %v, want the TRANSFER for %s", kind, payload["signature"], sig)
	}
	if payload["source"] != "SYSTEM_PROGRAM" || payload["description"] != "Transfer 0.5 SOL" || payload["feePayer"] != heliusAddrA {
		t.Fatalf("delivery parsed tx = %v", payload)
	}
	nt := heliusMap(t, heliusList(t, payload["nativeTransfers"], "nativeTransfers")[0], "nativeTransfers[0]")
	if nt["fromUserAccount"] != heliusAddrA {
		t.Fatalf("delivery fromUserAccount = %v, want the fee payer", nt["fromUserAccount"])
	}
	heliusNum(t, nt["amount"], heliusHalfSOL, "delivery amount (0.5 SOL)")
	heliusNum(t, payload["timestamp"], float64(base.Unix()+2), "delivery timestamp (stamped at confirmation)")
	if got := dv[0].headers.Get("Authorization"); got != heliusSecret {
		t.Fatalf("delivery Authorization = %q, want the hook's authHeader", got)
	}
	if got := dv[0].headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("delivery Content-Type = %q", got)
	}
	for k := range dv[0].headers {
		if strings.Contains(strings.ToLower(k), "sign") {
			t.Fatalf("delivery carries signature header %q; Helius webhooks are unsigned by design", k)
		}
	}
	// Re-polling the same state does not re-emit.
	f.sigStatus(sig)
	if got := len(f.deliveries()); got != 1 {
		t.Fatalf("re-poll deliveries = %d, want still one", got)
	}

	f.vc.Advance(1500 * time.Millisecond) // 4s: finalized
	st = f.sigStatus(sig)
	if st["confirmationStatus"] != "finalized" {
		t.Fatalf("status at 4s = %v, want finalized", st["confirmationStatus"])
	}
	if st["confirmations"] != nil {
		t.Fatalf("finalized confirmations = %v, want null", st["confirmations"])
	}
	if got := len(f.deliveries()); got != 1 {
		t.Fatalf("deliveries after finalization = %d, want still one", got)
	}

	// getTransaction reports the landed signature in the full Solana shape:
	// deterministic balances moved by the parsed transfer, fee on the payer.
	gt := heliusMap(t, f.rpc("getTransaction", []any{sig}, 9).Body["result"], "getTransaction result")
	heliusNum(t, gt["blockTime"], float64(base.Unix()), "blockTime (send time)")
	heliusNum(t, gt["slot"], 25000001, "getTransaction slot")
	txn := heliusMap(t, gt["transaction"], "transaction")
	if sigs := heliusList(t, txn["signatures"], "signatures"); len(sigs) != 1 || sigs[0] != sig {
		t.Fatalf("transaction.signatures = %v", sigs)
	}
	msg := heliusMap(t, txn["message"], "message")
	keys := heliusList(t, msg["accountKeys"], "accountKeys")
	key0 := heliusMap(t, keys[0], "accountKeys[0]")
	if key0["pubkey"] != heliusAddrA || key0["signer"] != true || key0["writable"] != true {
		t.Fatalf("accountKeys[0] = %v, want the paying signer", key0)
	}
	instr := heliusMap(t, heliusList(t, msg["instructions"], "instructions")[0], "instructions[0]")
	if instr["program"] != "system" || instr["programId"] != "11111111111111111111111111111111" {
		t.Fatalf("system instruction = %v", instr)
	}
	heliusNum(t, heliusMap(t, instr["args"], "args")["lamports"], heliusHalfSOL, "instruction lamports")
	meta := heliusMap(t, gt["meta"], "meta")
	heliusNum(t, meta["fee"], 5000, "meta.fee")
	if meta["err"] != nil {
		t.Fatalf("meta.err = %v, want null", meta["err"])
	}
	if _, has := heliusMap(t, meta["status"], "meta.status")["Ok"]; !has {
		t.Fatalf("meta.status = %v, want Ok", meta["status"])
	}
	pre := heliusList(t, meta["preBalances"], "preBalances")
	post := heliusList(t, meta["postBalances"], "postBalances")
	if len(pre) != 2 || len(post) != 2 {
		t.Fatalf("balances: pre %v post %v, want the payer and the recipient", pre, post)
	}
	heliusNum(t, pre[0], heliusBalance(heliusAddrA), "payer preBalance")
	heliusNum(t, post[0], heliusBalance(heliusAddrA)-heliusHalfSOL-5000, "payer postBalance (amount + fee)")
	heliusNum(t, post[1], toFloat(t, pre[1])+heliusHalfSOL, "recipient postBalance")
	// An unknown signature is a null result, not an error.
	if got := f.rpc("getTransaction", []any{"unknown-signature"}, 9).Body["result"]; got != nil {
		t.Fatalf("getTransaction unknown sig = %v, want null", got)
	}

	// ===== simulate_fail lands an on-chain InstructionError while confirmation still proceeds =====
	failSig := f.sendTx(map[string]any{"simulate_fail": true})
	f.vc.Advance(2500 * time.Millisecond) // 6.5s: the failed tx reaches confirmed
	st = f.sigStatus(failSig)
	if st["confirmationStatus"] != "confirmed" {
		t.Fatalf("failed tx status = %v, want confirmed (failure does not stall confirmation)", st["confirmationStatus"])
	}
	heliusNum(t, st["confirmations"], 31, "failed tx confirmations")
	errObj := heliusMap(t, st["err"], "failed tx err")
	ie := heliusList(t, errObj["InstructionError"], "InstructionError")
	heliusNum(t, ie[0], 0, "InstructionError index")
	heliusNum(t, heliusMap(t, ie[1], "InstructionError custom")["Custom"], 600, "InstructionError custom code")
	if _, has := heliusMap(t, st["status"], "failed tx status")["Err"]; !has {
		t.Fatalf("failed tx status = %v, want Err", st["status"])
	}
	// The ANY hook delivers failed transactions too, like real Helius.
	if got := len(f.deliveries()); got != 2 {
		t.Fatalf("deliveries after failed tx confirmed = %d, want 2", got)
	}
	if _, fp := decodeDelivery(t, f.deliveries()[1]); fp["signature"] != failSig {
		t.Fatalf("second delivery signature = %v, want the failed tx", fp["signature"])
	}
	f.vc.Advance(1500 * time.Millisecond) // 8s: finalized, error persists
	st = f.sigStatus(failSig)
	if st["confirmationStatus"] != "finalized" || st["err"] == nil {
		t.Fatalf("failed tx finalized = %v, want finalized with a persistent err", st)
	}
}

// TestHeliusEnhancedTransactionsAPI: the flagship Enhanced Transactions API —
// one-shot seeding with the Helius parsed vocabulary, type/source filters and
// signature cursors, cross-surface visibility of sent transactions, the
// parse-transactions contract, and the deterministic lookups.
func TestHeliusEnhancedTransactionsAPI(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newHeliusFixture(t, base)

	// ===== the enhanced history seeds once per address, newest first, in the Helius parsed shape =====
	hist := f.addressTxs(heliusAddrA, nil)
	if len(hist) != 8 {
		t.Fatalf("seeded history = %d items, want 8", len(hist))
	}
	heliusIsBase58(t, hist[0]["signature"].(string), "history signature", 88)
	for i, tx := range hist {
		m := i % 4
		wantType, wantSource := "TRANSFER", "SYSTEM_PROGRAM"
		if m == 1 {
			wantType, wantSource = "SWAP", "JUPITER"
		} else if m == 3 {
			wantSource = "SPL_TOKEN"
		}
		if tx["type"] != wantType || tx["source"] != wantSource {
			t.Fatalf("history[%d] = %s/%s, want %s/%s", i, tx["type"], tx["source"], wantType, wantSource)
		}
		heliusNum(t, tx["fee"], 5000, "history fee")
		heliusNum(t, tx["slot"], 25000000+float64(i+1), "history slot")
		if tx["feePayer"] != heliusAddrA || tx["description"] == "" {
			t.Fatalf("history[%d] payer/description = %v / %v", i, tx["feePayer"], tx["description"])
		}
		if i > 0 {
			if toFloat(t, hist[i-1]["timestamp"]) <= toFloat(t, tx["timestamp"]) {
				t.Fatalf("history not newest-first at %d: %v then %v", i, hist[i-1]["timestamp"], tx["timestamp"])
			}
		}
	}
	heliusNum(t, hist[0]["timestamp"], float64(base.Unix()-600), "newest seeded timestamp (10min spacing)")
	nt := heliusMap(t, heliusList(t, hist[0]["nativeTransfers"], "nativeTransfers")[0], "nativeTransfers[0]")
	heliusNum(t, nt["amount"], heliusHalfSOL, "seeded native transfer amount")
	swap := hist[1]
	ev := heliusMap(t, swap["events"], "events")
	swapEv := heliusMap(t, ev["swap"], "events.swap")
	tokIn := heliusMap(t, swapEv["tokenInput"], "swap tokenInput")
	if tokIn["userAccount"] != heliusAddrA {
		t.Fatalf("swap tokenInput user = %v, want the address", tokIn["userAccount"])
	}
	inAmt := heliusMap(t, tokIn["tokenAmount"], "swap tokenInput tokenAmount")
	if inAmt["amount"] != "500000000" {
		t.Fatalf("swap tokenInput amount = %v, want the 0.5 SOL string", inAmt["amount"])
	}
	heliusNum(t, inAmt["decimals"], 9, "WSOL decimals")
	heliusNum(t, inAmt["uiAmount"], 0.5, "swap input uiAmount")
	outAmt := heliusMap(t, heliusMap(t, swapEv["tokenOutput"], "swap tokenOutput")["tokenAmount"], "swap tokenOutput tokenAmount")
	heliusNum(t, outAmt["uiAmount"], 87.5, "swap output uiAmount")
	spl := heliusMap(t, heliusList(t, hist[3]["tokenTransfers"], "tokenTransfers")[0], "tokenTransfers[0]")
	splAmt := heliusMap(t, spl["tokenAmount"], "SPL tokenAmount")
	if splAmt["amount"] != "100000000" {
		t.Fatalf("SPL amount = %v, want the 100 USDC string", splAmt["amount"])
	}
	heliusNum(t, splAmt["uiAmount"], 100, "SPL uiAmount")
	heliusNum(t, splAmt["decimals"], 6, "USDC decimals")
	// Seed-once: a second read does not duplicate the history.
	again := f.addressTxs(heliusAddrA, nil)
	if len(again) != 8 || again[0]["signature"] != hist[0]["signature"] {
		t.Fatalf("second read = %d items, want the same 8 (seed once)", len(again))
	}

	// ===== type/source filters and before/until/limit paging narrow the history =====
	if swaps := f.addressTxs(heliusAddrA, map[string]string{"type": "SWAP"}); len(swaps) != 2 {
		t.Fatalf("type=SWAP = %d items, want the 2 seeded swaps", len(swaps))
	}
	if transfers := f.addressTxs(heliusAddrA, map[string]string{"type": "TRANSFER,SWAP"}); len(transfers) != 8 {
		t.Fatalf("type=TRANSFER,SWAP = %d items, want all 8 (comma list)", len(transfers))
	}
	if sys := f.addressTxs(heliusAddrA, map[string]string{"source": "SYSTEM_PROGRAM"}); len(sys) != 4 {
		t.Fatalf("source=SYSTEM_PROGRAM = %d items, want 4", len(sys))
	}
	if splOnly := f.addressTxs(heliusAddrA, map[string]string{"source": "SPL_TOKEN"}); len(splOnly) != 2 {
		t.Fatalf("source=SPL_TOKEN = %d items, want 2", len(splOnly))
	}
	if lim := f.addressTxs(heliusAddrA, map[string]string{"limit": "3"}); len(lim) != 3 || lim[0]["signature"] != hist[0]["signature"] || lim[2]["signature"] != hist[2]["signature"] {
		t.Fatalf("limit=3 = %d items, want the first three newest", len(lim))
	}
	if got := len(f.addressTxs(heliusAddrA, map[string]string{"limit": "1000"})); got != 8 {
		t.Fatalf("limit=1000 = %d items, want 8 (limit clamps to the max page of 100)", got)
	}
	// before: page backwards starting AFTER the cursor signature.
	before := f.addressTxs(heliusAddrA, map[string]string{"before": hist[1]["signature"].(string)})
	if len(before) != 6 || before[0]["signature"] != hist[2]["signature"] {
		t.Fatalf("before=hist[1] = %d items, want the 6 older ones", len(before))
	}
	// until: stop BEFORE the cursor signature.
	until := f.addressTxs(heliusAddrA, map[string]string{"until": hist[6]["signature"].(string)})
	if len(until) != 6 || until[5]["signature"] != hist[5]["signature"] {
		t.Fatalf("until=hist[6] = %d items, want the 6 newer ones", len(until))
	}
	// Crossed cursors (until older-listed than before): until is only
	// searched at-or-after the before cursor, so it is silently ignored and
	// the page is just the items after before (asserted as-is; deviation
	// candidate vs the real API's empty window).
	crossed := f.addressTxs(heliusAddrA, map[string]string{
		"before": hist[5]["signature"].(string), "until": hist[2]["signature"].(string),
	})
	if len(crossed) != 2 || crossed[0]["signature"] != hist[6]["signature"] || crossed[1]["signature"] != hist[7]["signature"] {
		t.Fatalf("crossed cursors = %d items, want the as-is page after before (hist[6], hist[7])", len(crossed))
	}

	// ===== a sent transaction joins its address's history; other addresses do not see it =====
	sig := f.sendTx(map[string]any{"simulate_address": heliusAddrA})
	f.vc.Advance(4 * time.Second) // land and finalize
	after := f.addressTxs(heliusAddrA, nil)
	if len(after) != 9 {
		t.Fatalf("history after send = %d items, want 9", len(after))
	}
	if after[0]["signature"] != sig || after[0]["type"] != "TRANSFER" || after[0]["feePayer"] != heliusAddrA {
		t.Fatalf("newest history item = %v, want the just-sent TRANSFER", after[0]["signature"])
	}
	other := f.addressTxs(heliusAddrB, nil)
	if len(other) != 8 {
		t.Fatalf("other address history = %d items, want only its own 8", len(other))
	}
	for _, tx := range other {
		if tx["signature"] == sig {
			t.Fatalf("unrelated address sees signature %s", sig)
		}
	}

	// ===== the parse-transactions contract holds and the balances/nfts/names lookups are deterministic =====
	raw1, raw2 := strings.Repeat("A", 70), strings.Repeat("B", 65)
	pr := f.call("enh", "on_parse_transactions", "POST", "/v0/transactions", nil, heliusQ(nil), map[string]any{
		"transactions": []any{raw1, raw2},
	})
	if pr.Status != 200 {
		t.Fatalf("parse transactions -> %d: %v", pr.Status, pr.Body)
	}
	if len(pr.BodyList) != 2 {
		t.Fatalf("parsed = %d items, want 2", len(pr.BodyList))
	}
	p0 := heliusMap(t, pr.BodyList[0], "parsed[0]")
	if p0["signature"] != raw1[:64] || p0["type"] != "TRANSFER" || p0["source"] != "SYSTEM_PROGRAM" {
		t.Fatalf("parsed[0] = %v", p0["signature"])
	}
	for _, bad := range []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"transactions": []any{}}, "non-empty array"},
		{map[string]any{}, "non-empty array"},
		{map[string]any{"transactions": []any{raw1, 42}}, "transactions[1]"},
	} {
		r := f.call("enh", "on_parse_transactions", "POST", "/v0/transactions", nil, heliusQ(nil), bad.body)
		errMsg, _ := r.Body["error"].(string)
		if r.Status != 400 || !strings.Contains(errMsg, bad.want) {
			t.Fatalf("parse %v -> %d %v, want 400 mentioning %q", bad.body, r.Status, r.Body["error"], bad.want)
		}
	}
	tooMany := []any{}
	for i := 0; i < 101; i++ {
		tooMany = append(tooMany, raw1)
	}
	if r := f.call("enh", "on_parse_transactions", "POST", "/v0/transactions", nil, heliusQ(nil),
		map[string]any{"transactions": tooMany}); r.Status != 400 || !strings.Contains(r.Body["error"].(string), "100") {
		t.Fatalf("parse 101 -> %d %v, want the max-100 400", r.Status, r.Body["error"])
	}

	// Balances: the two seeded tokens with Helius tokenAmount shapes.
	bal := f.call("enh", "on_get_balances", "GET", "/v0/addresses/"+heliusAddrA+"/balances",
		map[string]string{"address": heliusAddrA}, heliusQ(nil), nil)
	if bal.Status != 200 {
		t.Fatalf("balances -> %d: %v", bal.Status, bal.Body)
	}
	tokens := heliusList(t, bal.Body["tokens"], "tokens")
	if len(tokens) != 2 || heliusMap(t, tokens[0], "token 0")["symbol"] != "USDC" || heliusMap(t, tokens[1], "token 1")["symbol"] != "SOL" {
		t.Fatalf("balances tokens = %v", bal.Body["tokens"])
	}
	heliusNum(t, heliusMap(t, tokens[0], "token 0")["decimals"], 6, "USDC decimals")
	heliusNum(t, bal.Body["totalPrice"], 2*100.50, "totalPrice")
	// NFTs: deterministic holdings owned by the address.
	nfts := f.call("enh", "on_get_nfts", "GET", "/v0/addresses/"+heliusAddrA+"/nfts",
		map[string]string{"address": heliusAddrA}, heliusQ(nil), nil)
	nftList := heliusList(t, nfts.Body["nfts"], "nfts")
	heliusNum(t, nfts.Body["total"], 2, "nft total")
	n0 := heliusMap(t, nftList[0], "nft 0")
	if heliusMap(t, n0["ownership"], "ownership")["owner"] != heliusAddrA || heliusMap(t, n0["collection"], "collection")["verified"] != true {
		t.Fatalf("nft 0 = %v", n0)
	}
	// Names: every address maps to a .sol name, stably across calls.
	names := func() map[string]string {
		r := f.call("enh", "on_get_names", "POST", "/v0/names", nil, heliusQ(nil), map[string]any{
			"addresses": []any{heliusAddrA, heliusAddrB},
		})
		if r.Status != 200 {
			t.Fatalf("names -> %d: %v", r.Status, r.Body)
		}
		out := map[string]string{}
		for a, n := range heliusMap(t, r.Body["names"], "names") {
			s, _ := n.(string)
			out[a] = s
		}
		return out
	}
	n1, n2 := names(), names()
	for _, addr := range []string{heliusAddrA, heliusAddrB} {
		if !strings.HasSuffix(n1[addr], ".sol") || n1[addr] == "" {
			t.Fatalf("name for %s = %q, want a .sol domain", addr, n1[addr])
		}
		if n1[addr] != n2[addr] {
			t.Fatalf("name for %s is not deterministic: %q vs %q", addr, n1[addr], n2[addr])
		}
	}
}

// TestHeliusWebhooks: the webhooks API — config CRUD with the Helius shape,
// and delivery semantics (unsigned, authHeader as Authorization,
// transactionTypes filtering, exactly-once on confirmation).
func TestHeliusWebhooks(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newHeliusFixture(t, base)

	// ===== webhook registration round-trips the Helius config shape with 404s for unknown ids =====
	created := f.hookCall("on_create_webhook", "POST", "/v0/webhooks", nil, map[string]any{
		"webhookURL": f.sinkURL,
	})
	if created.Status != 201 {
		t.Fatalf("create webhook -> %d: %v", created.Status, created.Body)
	}
	wid, _ := created.Body["webhookID"].(string)
	if wid != "wh_1" {
		t.Fatalf("webhookID = %q, want wh_1", wid)
	}
	if types := heliusList(t, created.Body["transactionTypes"], "transactionTypes"); len(types) != 1 || types[0] != "ANY" {
		t.Fatalf("default transactionTypes = %v, want [ANY]", created.Body["transactionTypes"])
	}
	if created.Body["webhook_type"] != "enhanced" || created.Body["authHeader"] != "" {
		t.Fatalf("created webhook = %v", created.Body)
	}
	if listed := heliusList(t, f.hookCall("on_list_webhooks", "GET", "/v0/webhooks", nil, nil).BodyList, "webhook list"); len(listed) != 1 {
		t.Fatalf("list webhooks = %d, want 1", len(listed))
	}
	hookParams := map[string]string{"webhookId": wid}
	if got := f.hookCall("on_get_webhook", "GET", "/v0/webhooks/"+wid, hookParams, nil); got.Status != 200 || got.Body["webhookID"] != wid {
		t.Fatalf("get webhook -> %d %v", got.Status, got.Body)
	}
	edited := f.hookCall("on_edit_webhook", "PUT", "/v0/webhooks/"+wid, hookParams, map[string]any{
		"transactionTypes": []any{"SWAP", "TRANSFER"}, "authHeader": "edited-secret",
	})
	if edited.Status != 200 {
		t.Fatalf("edit webhook -> %d: %v", edited.Status, edited.Body)
	}
	if types := heliusList(t, edited.Body["transactionTypes"], "edited types"); len(types) != 2 || types[0] != "SWAP" {
		t.Fatalf("edited transactionTypes = %v", edited.Body["transactionTypes"])
	}
	if edited.Body["authHeader"] != "edited-secret" || edited.Body["webhookURL"] != f.sinkURL {
		t.Fatalf("edited webhook = %v", edited.Body)
	}
	for _, c := range []struct {
		handler, method string
		body            map[string]any
	}{
		{"on_get_webhook", "GET", nil},
		{"on_edit_webhook", "PUT", map[string]any{"authHeader": "x"}},
		{"on_delete_webhook", "DELETE", nil},
	} {
		if r := f.hookCall(c.handler, c.method, "/v0/webhooks/wh_nope", map[string]string{"webhookId": "wh_nope"}, c.body); r.Status != 404 || r.Body["error"] != "webhook not found" {
			t.Fatalf("%s unknown id -> %d %v, want 404 webhook not found", c.handler, r.Status, r.Body)
		}
	}
	if r := f.hookCall("on_delete_webhook", "DELETE", "/v0/webhooks/"+wid, hookParams, nil); r.Status != 200 || r.Body["webhookID"] != wid {
		t.Fatalf("delete webhook -> %d %v", r.Status, r.Body)
	}
	if listed := heliusList(t, f.hookCall("on_list_webhooks", "GET", "/v0/webhooks", nil, nil).BodyList, "webhook list"); len(listed) != 0 {
		t.Fatalf("list after delete = %d, want 0", len(listed))
	}

	// ===== deliveries are unsigned, carry authHeader as Authorization, honor transactionTypes and fire exactly once =====
	if r := f.hookCall("on_create_webhook", "POST", "/v0/webhooks", nil, map[string]any{
		"webhookURL": f.sinkURL, "transactionTypes": []any{"SWAP"}, "authHeader": heliusSecret,
	}); r.Status != 201 {
		t.Fatalf("create SWAP hook -> %d: %v", r.Status, r.Body)
	}

	swapSig := f.sendTx(map[string]any{"simulate_type": "SWAP", "simulate_address": heliusAddrA})
	f.vc.Advance(2500 * time.Millisecond) // first confirmation window
	if st := f.sigStatus(swapSig); st["confirmationStatus"] != "confirmed" {
		t.Fatalf("swap tx status = %v, want confirmed", st["confirmationStatus"])
	}
	dv := f.deliveries()
	if len(dv) != 1 {
		t.Fatalf("deliveries after swap confirmed = %d, want 1", len(dv))
	}
	if got := dv[0].headers.Get("Authorization"); got != heliusSecret {
		t.Fatalf("delivery Authorization = %q, want the authHeader shared secret", got)
	}
	for k := range dv[0].headers {
		if strings.Contains(strings.ToLower(k), "sign") {
			t.Fatalf("delivery carries signature header %q; Helius webhooks are unsigned by design", k)
		}
	}
	kind, payload := decodeDelivery(t, dv[0])
	if kind != "SWAP" || payload["signature"] != swapSig || payload["source"] != "JUPITER" {
		t.Fatalf("delivery = %s %v, want the JUPITER swap", kind, payload["signature"])
	}
	if payload["description"] != "Swap 0.5 SOL for 87.5 USDC" {
		t.Fatalf("swap delivery description = %v", payload["description"])
	}
	if heliusMap(t, payload["events"], "payload events")["swap"] == nil {
		t.Fatalf("swap delivery carries no events.swap: %v", payload["events"])
	}
	// Finalizing does not re-deliver.
	f.vc.Advance(2 * time.Second)
	f.sigStatus(swapSig)
	if got := len(f.deliveries()); got != 1 {
		t.Fatalf("deliveries after finalization = %d, want still 1", got)
	}

	// A TRANSFER confirmation is not delivered: the hook subscribes only to SWAP.
	trSig := f.sendTx(map[string]any{"simulate_address": heliusAddrA})
	f.vc.Advance(2 * time.Second) // milestones are whole seconds: land inside the confirmed window
	if st := f.sigStatus(trSig); st["confirmationStatus"] != "confirmed" {
		t.Fatalf("transfer tx status = %v, want confirmed", st["confirmationStatus"])
	}
	if got := len(f.deliveries()); got != 1 {
		t.Fatalf("TRANSFER delivered to a SWAP-only hook: %d deliveries", got)
	}

	// An ANY hook registered alongside picks up what the typed hook skips,
	// and without an authHeader the delivery carries no Authorization.
	if r := f.hookCall("on_create_webhook", "POST", "/v0/webhooks", nil, map[string]any{
		"webhookURL": f.sinkURL,
	}); r.Status != 201 {
		t.Fatalf("create ANY hook -> %d: %v", r.Status, r.Body)
	}
	anySig := f.sendTx(map[string]any{"simulate_address": heliusAddrA})
	f.vc.Advance(2 * time.Second)
	if st := f.sigStatus(anySig); st["confirmationStatus"] != "confirmed" {
		t.Fatalf("any-hook tx status = %v, want confirmed", st["confirmationStatus"])
	}
	dv = f.deliveries()
	if len(dv) != 2 {
		t.Fatalf("deliveries with the ANY hook = %d, want 2", len(dv))
	}
	if got := dv[1].headers.Get("Authorization"); got != "" {
		t.Fatalf("delivery without authHeader carries Authorization %q", got)
	}
	if kind, _ := decodeDelivery(t, dv[1]); kind != "TRANSFER" {
		t.Fatalf("ANY-hook delivery type = %s, want TRANSFER", kind)
	}
}

// toFloat widens a JSON number to float64 for arithmetic in assertions.
func toFloat(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	default:
		t.Fatalf("value is %T(%v), want number", v, v)
		return 0
	}
}
