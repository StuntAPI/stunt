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

// Drives the erc4337-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: the JSON-RPC 2.0 envelope
// (including batch bodies and the -32600/-32601/-32602 error codes),
// eth_estimateUserOperationGas' v0.7 field validation, the deterministic
// eth_sendUserOperation hash, the derive-on-read mempool -> bundled ->
// included lifecycle (no sleeps — the clock advances), the simulate_fail
// revert path, and the paymaster sponsorship endpoint.
const (
	ercEntryPoint = "0x0000000071727De22E5E9d8BAf0edAc6f37da032"
	ercSender     = "0x1234567890abcdef1234567890abcdef12345678"
)

type erc4337Fixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newErc4337Fixture(t *testing.T, start time.Time) *erc4337Fixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "erc4337-style")
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
	return &erc4337Fixture{t: t, vc: vc, host: "bundler.erc4337.test", vms: map[string]*starlark.VM{
		"rpc": load("rpc.star"), "paymaster": load("paymaster.star"),
	}}
}

// rpc posts one JSON-RPC request object to on_jsonrpc the way the engine
// dispatches POST /.
func (f *erc4337Fixture) rpc(method string, params []any, id any) map[string]any {
	f.t.Helper()
	r, err := f.vms["rpc"].Call("on_jsonrpc", starlark.Request{
		Method: "POST", Path: "/", Host: f.host, Headers: map[string]string{},
		Body:   map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": id},
		Params: map[string]string{}, Query: map[string]string{},
	})
	if err != nil {
		f.t.Fatalf("on_jsonrpc %s: %v", method, err)
	}
	if r.Status != 200 {
		f.t.Fatalf("on_jsonrpc %s -> HTTP %d: %v (JSON-RPC errors ride HTTP 200)", method, r.Status, r.Body)
	}
	return r.Body
}

// rpcBatch posts a JSON-RPC batch (the engine wraps array bodies in
// {"_batch": [...]}) and returns the array of envelopes.
func (f *erc4337Fixture) rpcBatch(calls []map[string]any) []any {
	f.t.Helper()
	// The engine's JSON path yields []any, not a typed slice.
	elems := make([]any, len(calls))
	for i, c := range calls {
		elems[i] = c
	}
	r, err := f.vms["rpc"].Call("on_jsonrpc", starlark.Request{
		Method: "POST", Path: "/", Host: f.host, Headers: map[string]string{},
		Body:   map[string]any{"_batch": elems},
		Params: map[string]string{}, Query: map[string]string{},
	})
	if err != nil {
		f.t.Fatalf("on_jsonrpc batch: %v", err)
	}
	if r.Status != 200 || r.BodyList == nil {
		f.t.Fatalf("batch -> HTTP %d body %v list %v, want 200 with an array body", r.Status, r.Body, r.BodyList)
	}
	return r.BodyList
}

// rpcRaw posts a prebuilt body verbatim (for invalid-request shapes).
func (f *erc4337Fixture) rpcRaw(body map[string]any) map[string]any {
	f.t.Helper()
	r, err := f.vms["rpc"].Call("on_jsonrpc", starlark.Request{
		Method: "POST", Path: "/", Host: f.host, Headers: map[string]string{},
		Body: body, Params: map[string]string{}, Query: map[string]string{},
	})
	if err != nil {
		f.t.Fatalf("on_jsonrpc raw: %v", err)
	}
	return r.Body
}

// ercUserOp builds a fully valid v0.7-shaped userOperation (the adapter's
// USEROP_FIELDS, which keeps the v0.6-style paymasterAndData field).
func ercUserOp(nonce string) map[string]any {
	return map[string]any{
		"sender":               ercSender,
		"nonce":                nonce,
		"initCode":             "0x",
		"callData":             "0xdeadbeef",
		"callGasLimit":         "0x7d00",
		"verificationGasLimit": "0x186a0",
		"preVerificationGas":   "0xc8",
		"maxFeePerGas":         "0x3b9aca00",
		"maxPriorityFeePerGas": "0x1",
		"paymasterAndData":     "0x",
		"signature":            "0x" + ercRepeat("aa", 65),
	}
}

func ercRepeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

// ercAssertEnvelope checks the jsonrpc/version + id echo on a response.
func ercAssertEnvelope(t *testing.T, resp map[string]any, id any) {
	t.Helper()
	if resp["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v, want \"2.0\"", resp["jsonrpc"])
	}
	if resp["id"] != id {
		t.Fatalf("id = %v (%T), want the request id %v echoed", resp["id"], resp["id"], id)
	}
}

// ercAssertErr asserts a JSON-RPC error envelope with the given code.
func ercAssertErr(t *testing.T, resp map[string]any, id any, code int64, messagePart string) {
	t.Helper()
	ercAssertEnvelope(t, resp, id)
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("want an error object, got %v", resp)
	}
	if got := ercNum(e["code"]); got != code {
		t.Fatalf("error code = %v, want %d (%v)", e["code"], code, e)
	}
	if msg, _ := e["message"].(string); !strings.Contains(msg, messagePart) {
		t.Fatalf("error message = %q, want it to contain %q", msg, messagePart)
	}
	if _, hasResult := resp["result"]; hasResult {
		t.Fatalf("error response also carries a result: %v", resp)
	}
}

// ercNum reads a response number whether the adapter produced a fresh
// Starlark int or a value that round-tripped the JSON document store.
func ercNum(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// TestErc4337BundlerRPC: the JSON-RPC bundler surface.
func TestErc4337BundlerRPC(t *testing.T) {
	f := newErc4337Fixture(t, time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC))

	// ===== supportedEntryPoints, chainId, and the JSON-RPC envelope =====
	eps := f.rpc("eth_supportedEntryPoints", []any{}, int64(1))
	ercAssertEnvelope(t, eps, int64(1))
	if list, ok := eps["result"].([]any); !ok || len(list) != 1 || list[0] != ercEntryPoint {
		t.Fatalf("supportedEntryPoints = %v, want [%s]", eps["result"], ercEntryPoint)
	}
	if r := f.rpc("eth_chainId", nil, "chain-1"); r["result"] != "0x1" {
		t.Fatalf("eth_chainId = %v, want 0x1", r["result"])
	}
	ercAssertErr(t, f.rpc("eth_bogusMethod", []any{}, int64(7)), int64(7), -32601, "does not exist/is not available")
	ercAssertErr(t, f.rpcRaw(map[string]any{"params": []any{}}), nil, -32600, "Invalid Request")
	// Batch bodies answer element-by-element in order.
	batch := f.rpcBatch([]map[string]any{
		{"jsonrpc": "2.0", "method": "eth_supportedEntryPoints", "params": []any{}, "id": int64(11)},
		{"jsonrpc": "2.0", "method": "eth_bogusMethod", "params": []any{}, "id": int64(12)},
	})
	if len(batch) != 2 {
		t.Fatalf("batch = %d envelopes, want 2", len(batch))
	}
	first, _ := batch[0].(map[string]any)
	second, _ := batch[1].(map[string]any)
	if list, _ := first["result"].([]any); len(list) != 1 || list[0] != ercEntryPoint {
		t.Fatalf("batch[0] = %v, want the entry points result", first)
	}
	if e, _ := second["error"].(map[string]any); ercNum(e["code"]) != -32601 {
		t.Fatalf("batch[1] = %v, want the -32601 method-not-found envelope", second)
	}

	// ===== estimateUserOperationGas validates the full v0.7 field set =====
	gas := f.rpc("eth_estimateUserOperationGas", []any{ercUserOp("0x0"), ercEntryPoint}, int64(2))
	ercAssertEnvelope(t, gas, int64(2))
	est, ok := gas["result"].(map[string]any)
	if !ok {
		t.Fatalf("estimateGas result = %v, want an object", gas["result"])
	}
	for field, want := range map[string]string{
		"preVerificationGas": "0xc8", "verificationGasLimit": "0x186a0", "callGasLimit": "0x7d00",
	} {
		if est[field] != want {
			t.Fatalf("estimateGas %s = %v, want the fixed %s", field, est[field], want)
		}
	}
	missing := ercUserOp("0x0")
	delete(missing, "callData")
	ercAssertErr(t, f.rpc("eth_estimateUserOperationGas", []any{missing, ercEntryPoint}, int64(3)),
		int64(3), -32602, "missing required field: callData")
	nullSig := ercUserOp("0x0")
	nullSig["signature"] = nil
	ercAssertErr(t, f.rpc("eth_estimateUserOperationGas", []any{nullSig, ercEntryPoint}, int64(4)),
		int64(4), -32602, "null value for required field: signature")
	ercAssertErr(t, f.rpc("eth_estimateUserOperationGas", []any{}, int64(5)),
		int64(5), -32602, "missing userOperation")

	// ===== sendUserOperation answers a deterministic hash and defaults the entry point =====
	sent := f.rpc("eth_sendUserOperation", []any{ercUserOp("0x0"), ercEntryPoint}, int64(6))
	ercAssertEnvelope(t, sent, int64(6))
	userOpHash, _ := sent["result"].(string)
	if len(userOpHash) != 66 || !strings.HasPrefix(userOpHash, "0x") {
		t.Fatalf("userOpHash = %q, want 0x + 64 hex chars", userOpHash)
	}
	// Omitting the entryPoint param defaults to v0.7 — same hash.
	defaulted := f.rpc("eth_sendUserOperation", []any{ercUserOp("0x0")}, int64(7))
	if defaulted["result"] != userOpHash {
		t.Fatalf("defaulted entry point hash = %v, want the same %v", defaulted["result"], userOpHash)
	}
	if again := f.rpc("eth_sendUserOperation", []any{ercUserOp("0x0"), ercEntryPoint}, int64(8)); again["result"] != userOpHash {
		t.Fatalf("resend hash = %v, want the deterministic %v", again["result"], userOpHash)
	}
	badOp := map[string]any{"sender": ercSender, "nonce": "0x0"}
	ercAssertErr(t, f.rpc("eth_sendUserOperation", []any{badOp, ercEntryPoint}, int64(9)),
		int64(9), -32602, "missing required field:")

	// ===== the op walks mempool -> bundled -> included on the virtual clock =====
	byHash := func(hash string) map[string]any {
		t.Helper()
		resp := f.rpc("eth_getUserOperationByHash", []any{hash}, int64(20))
		ercAssertEnvelope(t, resp, int64(20))
		return resp
	}
	receipt := func(hash string) map[string]any {
		t.Helper()
		resp := f.rpc("eth_getUserOperationReceipt", []any{hash}, int64(21))
		ercAssertEnvelope(t, resp, int64(21))
		return resp
	}
	// 0-1s: in the mempool, not yet addressable by hash.
	if r := byHash(userOpHash)["result"]; r != nil {
		t.Fatalf("byHash in mempool = %v, want null", r)
	}
	if r := receipt(userOpHash)["result"]; r != nil {
		t.Fatalf("receipt in mempool = %v, want null", r)
	}
	// 1-3s: bundled — visible, but no block placement and no receipt yet.
	f.vc.Advance(2 * time.Second)
	bundled, ok := byHash(userOpHash)["result"].(map[string]any)
	if !ok {
		t.Fatalf("byHash at +2s = %v, want the bundled shape", byHash(userOpHash)["result"])
	}
	op, ok := bundled["userOperation"].(map[string]any)
	if !ok || op["sender"] != ercSender || op["callData"] != "0xdeadbeef" {
		t.Fatalf("bundled userOperation = %v, want the stored op echoed", bundled["userOperation"])
	}
	if bundled["blockNumber"] != nil || bundled["blockHash"] != nil || bundled["transactionHash"] != nil {
		t.Fatalf("bundled placement = %v, want null block fields until inclusion", bundled)
	}
	if r := receipt(userOpHash)["result"]; r != nil {
		t.Fatalf("receipt while bundled = %v, want null until inclusion", r)
	}
	// >=3s: included — full on-chain shapes, and repeated polls agree.
	f.vc.Advance(2 * time.Second)
	inc, ok := receipt(userOpHash)["result"].(map[string]any)
	if !ok {
		t.Fatalf("receipt after inclusion = %v, want the full shape", receipt(userOpHash)["result"])
	}
	if inc["userOpHash"] != userOpHash || inc["sender"] != ercSender || inc["success"] != true {
		t.Fatalf("receipt = %v, want success:true echoing hash and sender", inc)
	}
	if inc["actualGasCost"] != "0x186a0" || inc["actualGasUsed"] != "0xc350" {
		t.Fatalf("receipt gas = %v/%v, want the fixed hex values", inc["actualGasCost"], inc["actualGasUsed"])
	}
	logs, ok := inc["logs"].([]any)
	if !ok || len(logs) != 1 {
		t.Fatalf("receipt logs = %v, want the single UserOperationEvent log", inc["logs"])
	}
	log0, _ := logs[0].(map[string]any)
	topics, _ := log0["topics"].([]any)
	if len(topics) != 1 || !strings.HasPrefix(topics[0].(string), "0x") {
		t.Fatalf("log topics = %v, want the event topic hash", log0["topics"])
	}
	if inc["reason"] != nil {
		t.Fatalf("successful receipt carries a reason: %v", inc)
	}
	mined, ok := byHash(userOpHash)["result"].(map[string]any)
	if !ok || mined["blockNumber"] != "0x1" {
		t.Fatalf("byHash after inclusion = %v, want blockNumber 0x1", byHash(userOpHash)["result"])
	}
	if txHash, _ := mined["transactionHash"].(string); len(txHash) != 66 {
		t.Fatalf("byHash transactionHash = %v, want 0x + 64 hex", mined["transactionHash"])
	}
	// Unknown hashes resolve to null results, not errors.
	if r := receipt("0x" + ercRepeat("ab", 32))["result"]; r != nil {
		t.Fatalf("unknown hash receipt = %v, want null", r)
	}

	// ===== simulate_fail reverts on inclusion with the AA95 reason =====
	failOp := ercUserOp("0x1")
	failOp["callData"] = "0xdeadbeed"
	failSend := f.rpc("eth_sendUserOperation", []any{failOp, ercEntryPoint, map[string]any{"simulate_fail": true}}, int64(30))
	failHash, _ := failSend["result"].(string)
	if failHash == "" || failHash == userOpHash {
		t.Fatalf("simulate_fail hash = %q, want a distinct non-empty hash", failHash)
	}
	f.vc.Advance(4 * time.Second)
	failReceipt, ok := receipt(failHash)["result"].(map[string]any)
	if !ok {
		t.Fatalf("simulate_fail receipt = %v, want the reverted shape", receipt(failHash)["result"])
	}
	if failReceipt["success"] != false {
		t.Fatalf("simulate_fail success = %v, want false", failReceipt["success"])
	}
	if failReceipt["reason"] != "AA95 user operation execution reverted" {
		t.Fatalf("simulate_fail reason = %v, want the AA95 revert reason", failReceipt["reason"])
	}
}

// TestErc4337PaymasterSign: the sponsorship-signing REST side endpoint.
func TestErc4337PaymasterSign(t *testing.T) {
	f := newErc4337Fixture(t, time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC))
	sign := func(body map[string]any) starlark.Response {
		t.Helper()
		r, err := f.vms["paymaster"].Call("on_sign", starlark.Request{
			Method: "POST", Path: "/paymaster/sign", Host: f.host, Headers: map[string]string{},
			Body: body, Params: map[string]string{}, Query: map[string]string{},
		})
		if err != nil {
			t.Fatalf("on_sign: %v", err)
		}
		return r
	}

	// ===== the paymaster signs the op into paymasterAndData =====
	ok := sign(map[string]any{"userOp": ercUserOp("0x0")})
	if ok.Status != 200 {
		t.Fatalf("paymaster/sign -> %d: %v", ok.Status, ok.Body)
	}
	pmData, _ := ok.Body["paymasterAndData"].(string)
	if !strings.HasPrefix(pmData, "0x0000000000000000000000000000000000000001") || len(pmData) < 42 {
		t.Fatalf("paymasterAndData = %q, want the mock paymaster address prefix", pmData)
	}
	if ok.Body["validUntil"] != "0x0" || ok.Body["validAfter"] != "0x0" {
		t.Fatalf("validity = %v/%v, want zero windows", ok.Body["validUntil"], ok.Body["validAfter"])
	}

	// ===== missing or invalid userOps are 400s =====
	if r := sign(map[string]any{}); r.Status != 400 || r.Body["error"] != "missing_userOp" {
		t.Fatalf("sign without userOp -> %d %v, want 400 missing_userOp", r.Status, r.Body)
	}
	partial := sign(map[string]any{"userOp": map[string]any{"sender": ercSender}})
	if partial.Status != 400 || partial.Body["error"] != "invalid_userOp" {
		t.Fatalf("sign with partial userOp -> %d %v, want 400 invalid_userOp", partial.Status, partial.Body)
	}
	if msg, _ := partial.Body["message"].(string); !strings.Contains(msg, "missing required field:") {
		t.Fatalf("invalid_userOp message = %q, want the field-validation detail", msg)
	}
}
