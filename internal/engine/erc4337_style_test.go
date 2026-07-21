package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestErc4337StyleAdapter exercises the ERC-4337 bundler mock end-to-end
// through the full estimate → send → receipt flow:
//
//   - eth_supportedEntryPoints → ["0x0000000071727De22E5E9d8BAf0edAc6f37da032"]
//   - eth_estimateUserOperationGas → {preVerificationGas, ...}
//   - eth_sendUserOperation → userOp hash (validates fields)
//   - eth_sendUserOperation (missing field) → error -32602
//   - eth_getUserOperationReceipt(hash) → receipt with success:true
//   - eth_getUserOperationByHash(hash) → stored userOp
//   - POST /paymaster/sign → updated paymasterAndData
func TestErc4337StyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "erc4337-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"bundler": {Adapter: absAdapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	base := addrs["bundler"]

	// ===== eth_supportedEntryPoints → v0.7 EntryPoint address =====

	body, status := ercRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_supportedEntryPoints",
		"params":  []any{},
		"id":      1,
	})
	if status != 200 {
		t.Fatalf("supportedEntryPoints -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	json.Unmarshal([]byte(body), &resp)
	eps, ok := resp["result"].([]any)
	if !ok || len(eps) != 1 {
		t.Fatalf("supportedEntryPoints result = %v, want array of 1", resp["result"])
	}
	entryPoint, ok := eps[0].(string)
	if !ok {
		t.Fatalf("entryPoint = %v, want string", eps[0])
	}
	if entryPoint != "0x0000000071727De22E5E9d8BAf0edAc6f37da032" {
		t.Fatalf("entryPoint = %v, want 0x0000000071727De22E5E9d8BAf0edAc6f37da032", entryPoint)
	}

	// ===== eth_estimateUserOperationGas → gas estimate =====

	userOp := map[string]any{
		"sender":               "0x" + repeat("1234", 10),
		"nonce":                "0x0",
		"initCode":             "0x",
		"callData":             "0xdeadbeef",
		"callGasLimit":         "0x7d00",
		"verificationGasLimit": "0x186a0",
		"preVerificationGas":   "0xc8",
		"maxFeePerGas":         "0x3b9aca00",
		"maxPriorityFeePerGas": "0x1",
		"paymasterAndData":     "0x",
		"signature":            "0x" + repeat("aa", 65),
	}

	body, status = ercRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_estimateUserOperationGas",
		"params":  []any{userOp, entryPoint},
		"id":      2,
	})
	if status != 200 {
		t.Fatalf("estimateGas -> status %d, want 200; body %s", status, body)
	}
	var resp2 map[string]any
	json.Unmarshal([]byte(body), &resp2)
	gasResult, ok := resp2["result"].(map[string]any)
	if !ok {
		t.Fatalf("estimateGas result = %v, want object", resp2["result"])
	}
	for _, field := range []string{"preVerificationGas", "verificationGasLimit", "callGasLimit"} {
		v, ok := gasResult[field].(string)
		if !ok || v[:2] != "0x" {
			t.Fatalf("gasResult[%s] = %v, want 0x hex string", field, gasResult[field])
		}
	}

	// ===== eth_sendUserOperation → userOp hash =====

	body, status = ercRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_sendUserOperation",
		"params":  []any{userOp, entryPoint},
		"id":      3,
	})
	if status != 200 {
		t.Fatalf("sendUserOperation -> status %d, want 200; body %s", status, body)
	}
	var resp3 map[string]any
	json.Unmarshal([]byte(body), &resp3)
	userOpHash, ok := resp3["result"].(string)
	if !ok || len(userOpHash) != 66 || userOpHash[:2] != "0x" {
		t.Fatalf("userOpHash = %v, want 0x + 64 hex chars", resp3["result"])
	}

	// ===== eth_sendUserOperation (missing field) → error -32602 =====

	badUserOp := map[string]any{
		"sender":   "0x" + repeat("1234", 10),
		"nonce":    "0x0",
		"initCode": "0x",
		// Missing: callData, callGasLimit, etc.
	}
	body, status = ercRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_sendUserOperation",
		"params":  []any{badUserOp, entryPoint},
		"id":      4,
	})
	if status != 200 {
		t.Fatalf("sendUserOperation (bad) -> status %d, want 200; body %s", status, body)
	}
	var resp4 map[string]any
	json.Unmarshal([]byte(body), &resp4)
	errObj, ok := resp4["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %v", resp4)
	}
	if errObj["code"] != float64(-32602) {
		t.Fatalf("error code = %v, want -32602", errObj["code"])
	}

	// ===== eth_getUserOperationReceipt(hash) → receipt =====

	body, status = ercRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_getUserOperationReceipt",
		"params":  []any{userOpHash},
		"id":      5,
	})
	if status != 200 {
		t.Fatalf("getUserOperationReceipt -> status %d, want 200; body %s", status, body)
	}
	var resp5 map[string]any
	json.Unmarshal([]byte(body), &resp5)
	receipt, ok := resp5["result"].(map[string]any)
	if !ok {
		t.Fatalf("receipt = %v, want object", resp5["result"])
	}
	if receipt["userOpHash"] != userOpHash {
		t.Fatalf("receipt userOpHash = %v, want %v", receipt["userOpHash"], userOpHash)
	}
	if receipt["success"] != true {
		t.Fatalf("receipt success = %v, want true", receipt["success"])
	}
	if receipt["sender"] != userOp["sender"] {
		t.Fatalf("receipt sender = %v, want %v", receipt["sender"], userOp["sender"])
	}
	logs, ok := receipt["logs"].([]any)
	if !ok || len(logs) == 0 {
		t.Fatalf("receipt logs = %v, want non-empty", receipt["logs"])
	}
	innerReceipt, ok := receipt["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("receipt.receipt = %v, want object", receipt["receipt"])
	}
	txHash, ok := innerReceipt["transactionHash"].(string)
	if !ok || txHash[:2] != "0x" {
		t.Fatalf("receipt.receipt.transactionHash = %v, want 0x hex", innerReceipt["transactionHash"])
	}

	// ===== eth_getUserOperationByHash(hash) → stored userOp =====

	body, status = ercRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_getUserOperationByHash",
		"params":  []any{userOpHash},
		"id":      6,
	})
	if status != 200 {
		t.Fatalf("getUserOperationByHash -> status %d, want 200; body %s", status, body)
	}
	var resp6 map[string]any
	json.Unmarshal([]byte(body), &resp6)
	byHash, ok := resp6["result"].(map[string]any)
	if !ok {
		t.Fatalf("getUserOperationByHash result = %v, want object", resp6["result"])
	}
	storedOp, ok := byHash["userOperation"].(map[string]any)
	if !ok {
		t.Fatalf("userOperation = %v, want object", byHash["userOperation"])
	}
	if storedOp["sender"] != userOp["sender"] {
		t.Fatalf("storedOp sender = %v, want %v", storedOp["sender"], userOp["sender"])
	}
	if storedOp["callData"] != userOp["callData"] {
		t.Fatalf("storedOp callData = %v, want %v", storedOp["callData"], userOp["callData"])
	}

	// ===== POST /paymaster/sign → updated paymasterAndData =====

	body, status = ercPost(t, base, "/paymaster/sign", map[string]any{
		"userOp": userOp,
	})
	if status != 200 {
		t.Fatalf("paymaster/sign -> status %d, want 200; body %s", status, body)
	}
	var pmResp map[string]any
	json.Unmarshal([]byte(body), &pmResp)
	pmData, ok := pmResp["paymasterAndData"].(string)
	if !ok || pmData[:2] != "0x" {
		t.Fatalf("paymasterAndData = %v, want 0x hex", pmResp["paymasterAndData"])
	}
	// Should start with the paymaster address (20 bytes = 40 hex chars).
	if len(pmData) < 42 {
		t.Fatalf("paymasterAndData = %v, too short", pmData)
	}

	// ===== Determinism: same userOp → same hash =====

	body, _ = ercRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_sendUserOperation",
		"params":  []any{userOp, entryPoint},
		"id":      7,
	})
	var resp7 map[string]any
	json.Unmarshal([]byte(body), &resp7)
	userOpHash2, _ := resp7["result"].(string)
	if userOpHash2 != userOpHash {
		t.Fatalf("determinism failed: same userOp gave different hash %v != %v", userOpHash2, userOpHash)
	}
}

// === helpers ===

func ercRPC(t *testing.T, base string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", base+"/", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func ercPost(t *testing.T, base, path string, bodyObj map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(bodyObj)
	req, err := http.NewRequest("POST", base+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
