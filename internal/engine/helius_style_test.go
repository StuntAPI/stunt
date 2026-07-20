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

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestHeliusStyleAdapter exercises the Helius-style adapter end-to-end:
//
//   - JSON-RPC getBalance → lamports
//   - JSON-RPC getLatestBlockhash → blockhash + lastValidBlockHeight
//   - JSON-RPC sendTransaction → signature
//   - GET balances → token list
//   - GET NFTs → NFT holdings
//   - 401 without api-key
func TestHeliusStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "helius-style")
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
			"helius": {Adapter: absAdapterDir},
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

	base := addrs["helius"]
	const apiKey = "test-key-helius"
	const testAddr = "7xKXtg2CW87d97TXJSDpbD5jBkheTqA2DU9uTh4F6tJ9"
	rpcURL := base + "/?api-key=" + apiKey

	// ===== JSON-RPC getBalance =====

	body, status := heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "getBalance",
		"params":  []string{testAddr},
	})
	if status != 200 {
		t.Fatalf("getBalance -> status %d, want 200; body %s", status, body)
	}
	var balResp map[string]any
	if err := json.Unmarshal([]byte(body), &balResp); err != nil {
		t.Fatalf("unmarshal getBalance: %v (body %s)", err, body)
	}
	if balResp["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v, want 2.0", balResp["jsonrpc"])
	}
	result, ok := balResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want object", balResp["result"])
	}
	val, ok := result["value"].(float64)
	if !ok {
		t.Fatalf("value = %v, want number", result["value"])
	}
	if val < 0 {
		t.Fatalf("balance value = %v, should be >= 0", val)
	}

	// ===== JSON-RPC getLatestBlockhash =====

	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "getLatestBlockhash",
		"params":  []any{},
	})
	if status != 200 {
		t.Fatalf("getLatestBlockhash -> status %d, want 200; body %s", status, body)
	}
	var bhResp map[string]any
	if err := json.Unmarshal([]byte(body), &bhResp); err != nil {
		t.Fatalf("unmarshal blockhash: %v (body %s)", err, body)
	}
	bhResult := bhResp["result"].(map[string]any)
	bhVal := bhResult["value"].(map[string]any)
	if bhVal["blockhash"] == nil || bhVal["blockhash"] == "" {
		t.Fatalf("blockhash = %v, want non-empty", bhVal["blockhash"])
	}
	if bhVal["lastValidBlockHeight"] == nil {
		t.Fatalf("lastValidBlockHeight = %v, want non-nil", bhVal["lastValidBlockHeight"])
	}

	// ===== JSON-RPC sendTransaction =====

	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "sendTransaction",
		"params":  []string{"base64encodedtxdata"},
	})
	if status != 200 {
		t.Fatalf("sendTransaction -> status %d, want 200; body %s", status, body)
	}
	var sendResp map[string]any
	if err := json.Unmarshal([]byte(body), &sendResp); err != nil {
		t.Fatalf("unmarshal sendTransaction: %v (body %s)", err, body)
	}
	sig, ok := sendResp["result"].(string)
	if !ok || sig == "" {
		t.Fatalf("result (signature) = %v, want non-empty string", sendResp["result"])
	}

	// ===== 401 without api-key =====

	_, status = heliusPost(t, base+"/", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "getBalance",
		"params":  []string{testAddr},
	})
	if status != 401 {
		t.Fatalf("no api-key -> status %d, want 401", status)
	}

	// ===== GET balances =====

	body, status = heliusGet(t, base+"/v0/addresses/"+testAddr+"/balances?api-key="+apiKey)
	if status != 200 {
		t.Fatalf("get balances -> status %d, want 200; body %s", status, body)
	}
	var balGetResp map[string]any
	if err := json.Unmarshal([]byte(body), &balGetResp); err != nil {
		t.Fatalf("unmarshal balances: %v (body %s)", err, body)
	}
	tokens, ok := balGetResp["tokens"].([]any)
	if !ok || len(tokens) < 1 {
		t.Fatalf("tokens = %v, want non-empty array", balGetResp["tokens"])
	}

	// ===== GET NFTs =====

	body, status = heliusGet(t, base+"/v0/addresses/"+testAddr+"/nfts?api-key="+apiKey)
	if status != 200 {
		t.Fatalf("get nfts -> status %d, want 200; body %s", status, body)
	}
	var nftResp map[string]any
	if err := json.Unmarshal([]byte(body), &nftResp); err != nil {
		t.Fatalf("unmarshal nfts: %v (body %s)", err, body)
	}
	nfts, ok := nftResp["nfts"].([]any)
	if !ok || len(nfts) < 1 {
		t.Fatalf("nfts = %v, want non-empty array", nftResp["nfts"])
	}
	firstNFT := nfts[0].(map[string]any)
	if firstNFT["name"] == nil || firstNFT["name"] == "" {
		t.Fatalf("nft name = %v, want non-empty", firstNFT["name"])
	}

	// ===== POST names =====

	body, status = heliusPost(t, base+"/v0/names?api-key="+apiKey, map[string]any{
		"addresses": []string{testAddr},
	})
	if status != 200 {
		t.Fatalf("get names -> status %d, want 200; body %s", status, body)
	}
	var namesResp map[string]any
	if err := json.Unmarshal([]byte(body), &namesResp); err != nil {
		t.Fatalf("unmarshal names: %v (body %s)", err, body)
	}
	names, ok := namesResp["names"].(map[string]any)
	if !ok {
		t.Fatalf("names = %v, want object", namesResp["names"])
	}
	name, exists := names[testAddr]
	if !exists {
		t.Fatalf("names missing address %q: %v", testAddr, names)
	}
	if name == nil || name == "" {
		t.Fatalf("name for address = %v, want non-empty", name)
	}

	// ===== Unknown method → JSON-RPC error =====

	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "nonexistentMethod",
		"params":  []any{},
	})
	if status != 200 {
		t.Fatalf("unknown method -> status %d, want 200; body %s", status, body)
	}
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal error: %v (body %s)", err, body)
	}
	if errResp["error"] == nil {
		t.Fatalf("error = %v, want non-nil for unknown method", errResp["error"])
	}
}

// === Helius test helpers ===

func heliusGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func heliusPost(t *testing.T, rawurl string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
