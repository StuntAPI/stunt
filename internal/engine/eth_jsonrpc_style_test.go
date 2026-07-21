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

// TestEthJsonrpcStyleAdapter exercises the Ethereum JSON-RPC provider mock
// end-to-end through the full send → receipt → logs flow:
//
//   - eth_chainId → "0x1"
//   - eth_blockNumber → hex block number (starts at 0)
//   - eth_getBalance → hex wei
//   - eth_sendRawTransaction → tx hash; block number advances
//   - eth_getTransactionReceipt(hash) → status "0x1" + logs (STATEFUL)
//   - eth_getLogs → returns the logs from sent txs
//   - BATCH request → array response
//   - eth_call → hex
//   - error for unknown method
//   - eth_getBlockByNumber → block with transactions
func TestEthJsonrpcStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "eth-jsonrpc-style")
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
			"eth": {Adapter: absAdapterDir},
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

	base := addrs["eth"]

	// ===== eth_chainId → "0x1" =====

	body, status := ethRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_chainId",
		"params":  []any{},
		"id":      1,
	})
	if status != 200 {
		t.Fatalf("eth_chainId -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	json.Unmarshal([]byte(body), &resp)
	if resp["result"] != "0x1" {
		t.Fatalf("chainId = %v, want 0x1", resp["result"])
	}
	if resp["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v, want 2.0", resp["jsonrpc"])
	}
	if resp["id"] != float64(1) {
		t.Fatalf("id = %v, want 1", resp["id"])
	}

	// ===== eth_blockNumber → starts at "0x0" =====

	body, _ = ethRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_blockNumber",
		"params":  []any{},
		"id":      2,
	})
	var resp2 map[string]any
	json.Unmarshal([]byte(body), &resp2)
	initialBlockStr, ok := resp2["result"].(string)
	if !ok {
		t.Fatalf("blockNumber result = %v, want string", resp["result"])
	}
	initialBlock := parseHex(initialBlockStr)
	if initialBlock != 0 {
		t.Fatalf("initial blockNumber = %d, want 0", initialBlock)
	}

	// ===== eth_getBalance → hex wei =====

	body, _ = ethRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_getBalance",
		"params":  []any{"0x0000000000000000000000000000000000000000", "latest"},
		"id":      3,
	})
	var resp3 map[string]any
	json.Unmarshal([]byte(body), &resp3)
	balStr, ok := resp3["result"].(string)
	if !ok || balStr == "0x0" || balStr == "" {
		t.Fatalf("balance = %v, want non-zero hex wei", resp["result"])
	}

	// ===== eth_getTransactionCount → nonce =====

	body, _ = ethRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_getTransactionCount",
		"params":  []any{"0x0000000000000000000000000000000000000000", "latest"},
		"id":      4,
	})
	var resp4 map[string]any
	json.Unmarshal([]byte(body), &resp4)
	if resp4["result"] != "0x0" {
		t.Fatalf("initial nonce = %v, want 0x0", resp["result"])
	}

	// ===== eth_sendRawTransaction → tx hash + block advances =====

	rawTx := "0xf86c098504a817c800825208943535353535353535353535353535353535353535880de0b6b3a76400008025a028ef61340bd939bc2195fe537567866003e1a15d3c71ff63e1590620aa636276a067cbe9d8997f761aecb703304b3800ccf555c9f3dc64214b297fb1966a3b6d83"
	body, _ = ethRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_sendRawTransaction",
		"params":  []any{rawTx},
		"id":      5,
	})
	var resp5 map[string]any
	json.Unmarshal([]byte(body), &resp5)
	txHash, ok := resp5["result"].(string)
	if !ok || len(txHash) != 66 || txHash[:2] != "0x" {
		t.Fatalf("txHash = %v, want 0x + 64 hex chars", resp["result"])
	}

	// Block number should have advanced by 1.
	body, _ = ethRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_blockNumber",
		"params":  []any{},
		"id":      6,
	})
	var resp6 map[string]any
	json.Unmarshal([]byte(body), &resp6)
	newBlockStr, _ := resp6["result"].(string)
	newBlock := parseHex(newBlockStr)
	if newBlock != initialBlock+1 {
		t.Fatalf("blockNumber after send = %d, want %d", newBlock, initialBlock+1)
	}

	// ===== eth_getTransactionReceipt(hash) → status 0x1 + logs =====

	body, _ = ethRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_getTransactionReceipt",
		"params":  []any{txHash},
		"id":      7,
	})
	var resp7 map[string]any
	json.Unmarshal([]byte(body), &resp7)
	receipt, ok := resp7["result"].(map[string]any)
	if !ok {
		t.Fatalf("receipt = %v, want object", resp["result"])
	}
	if receipt["status"] != "0x1" {
		t.Fatalf("receipt status = %v, want 0x1", receipt["status"])
	}
	if receipt["transactionHash"] != txHash {
		t.Fatalf("receipt txHash = %v, want %v", receipt["transactionHash"], txHash)
	}
	logs, ok := receipt["logs"].([]any)
	if !ok || len(logs) == 0 {
		t.Fatalf("receipt logs = %v, want non-empty array", receipt["logs"])
	}
	firstLog, ok := logs[0].(map[string]any)
	if !ok {
		t.Fatalf("first log = %v, want object", logs[0])
	}
	if firstLog["transactionHash"] != txHash {
		t.Fatalf("log txHash = %v, want %v", firstLog["transactionHash"], txHash)
	}
	topics, ok := firstLog["topics"].([]any)
	if !ok || len(topics) == 0 {
		t.Fatalf("log topics = %v, want non-empty", firstLog["topics"])
	}
	if firstLog["blockNumber"] != newBlockStr {
		t.Fatalf("log blockNumber = %v, want %v", firstLog["blockNumber"], newBlockStr)
	}

	// ===== eth_getLogs → returns the logs =====

	body, _ = ethRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_getLogs",
		"params":  []any{map[string]any{"fromBlock": "0x0", "toBlock": "latest"}},
		"id":      8,
	})
	var resp8 map[string]any
	json.Unmarshal([]byte(body), &resp8)
	getLogs, ok := resp8["result"].([]any)
	if !ok {
		t.Fatalf("getLogs result = %v, want array", resp["result"])
	}
	if len(getLogs) == 0 {
		t.Fatalf("getLogs returned empty array, expected at least 1 log from sent tx")
	}

	// ===== eth_call → deterministic hex =====

	body, _ = ethRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_call",
		"params": []any{
			map[string]any{
				"to":   "0x0000000000000000000000000000000000000001",
				"data": "0x70a082310000000000000000000000003535353535353535353535353535353535353535",
			},
			"latest",
		},
		"id": 9,
	})
	var resp9 map[string]any
	json.Unmarshal([]byte(body), &resp9)
	callResult, ok := resp9["result"].(string)
	if !ok || callResult[:2] != "0x" {
		t.Fatalf("eth_call result = %v, want hex string", resp["result"])
	}

	// ===== BATCH request → array response =====

	batchBody := `[
		{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":10},
		{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":11},
		{"jsonrpc":"2.0","method":"net_version","params":[],"id":12}
	]`
	body, status = ethRPCCustom(t, base, batchBody)
	if status != 200 {
		t.Fatalf("batch -> status %d, want 200; body %s", status, body)
	}
	var batchResp []any
	if err := json.Unmarshal([]byte(body), &batchResp); err != nil {
		t.Fatalf("unmarshal batch response: %v (body %s)", err, body)
	}
	if len(batchResp) != 3 {
		t.Fatalf("batch response length = %d, want 3", len(batchResp))
	}
	// Verify each element has the right result.
	first := batchResp[0].(map[string]any)
	if first["result"] != "0x1" {
		t.Fatalf("batch[0] result = %v, want 0x1", first["result"])
	}
	second := batchResp[1].(map[string]any)
	if second["result"] != newBlockStr {
		t.Fatalf("batch[1] result = %v, want %v", second["result"], newBlockStr)
	}
	third := batchResp[2].(map[string]any)
	if third["result"] != "1" {
		t.Fatalf("batch[2] result = %v, want '1'", third["result"])
	}

	// ===== Unknown method → error =====

	body, _ = ethRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_nonexistent",
		"params":  []any{},
		"id":      13,
	})
	var resp13 map[string]any
	json.Unmarshal([]byte(body), &resp13)
	errObj, ok := resp13["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %v", resp)
	}
	if errObj["code"] != float64(-32601) {
		t.Fatalf("error code = %v, want -32601", errObj["code"])
	}

	// ===== eth_getBlockByNumber → block with transactions =====

	body, _ = ethRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_getBlockByNumber",
		"params":  []any{newBlockStr, false},
		"id":      14,
	})
	var resp14 map[string]any
	json.Unmarshal([]byte(body), &resp14)
	blkObj, ok := resp14["result"].(map[string]any)
	if !ok {
		t.Fatalf("getBlockByNumber result = %v, want object", resp["result"])
	}
	blkTxs, ok := blkObj["transactions"].([]any)
	if !ok || len(blkTxs) == 0 {
		t.Fatalf("block transactions = %v, want non-empty", blkObj["transactions"])
	}
	if blkTxs[0] != txHash {
		t.Fatalf("block tx[0] = %v, want %v", blkTxs[0], txHash)
	}

	// ===== web3_clientVersion =====

	body, _ = ethRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "web3_clientVersion",
		"params":  []any{},
		"id":      15,
	})
	var resp15 map[string]any
	json.Unmarshal([]byte(body), &resp15)
	if resp15["result"] != "Stunt/v1.0.0/mock" {
		t.Fatalf("web3_clientVersion = %v, want Stunt/v1.0.0/mock", resp["result"])
	}

	// ===== Determinism: same raw tx → same hash =====

	body, _ = ethRPC(t, base, map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_sendRawTransaction",
		"params":  []any{rawTx},
		"id":      16,
	})
	var resp16 map[string]any
	json.Unmarshal([]byte(body), &resp16)
	txHash2, _ := resp16["result"].(string)
	if txHash2 != txHash {
		t.Fatalf("determinism failed: same raw tx gave different hash %v != %v", txHash2, txHash)
	}
}

// === helpers ===

func ethRPC(t *testing.T, base string, body map[string]any) (string, int) {
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

func ethRPCCustom(t *testing.T, base, rawBody string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", base+"/", bytes.NewReader([]byte(rawBody)))
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

func parseHex(s string) int {
	// Simple hex parser for "0xNN" strings.
	n := 0
	for i := 2; i < len(s); i++ {
		c := s[i]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return n
		}
		n = n*16 + d
	}
	return n
}
