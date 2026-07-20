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

// TestWalletConnectStyleAdapter exercises the WalletConnect v2 relay mock
// end-to-end through the full pairing → session → request → disconnect flow:
//
//   - POST /v1/pairings → {topic, relay:{protocol:"irn"}, expiry, state}
//   - POST /v1/sessions → propose session → {topic, acknowledged:false, ...}
//   - POST /v1/sessions/{topic}/approve → {acknowledged:true, namespaces}
//   - POST /v1/sessions/{topic}/request (eth_requestAccounts) → [address]
//   - POST /v1/sessions/{topic}/request (personal_sign) → signature hash
//   - GET /v1/sessions → list (stateful)
//   - DELETE /v1/sessions/{topic} → disconnect
func TestWalletConnectStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "walletconnect-style")
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
			"wc": {Adapter: absAdapterDir},
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

	base := addrs["wc"]

	// ===== POST /v1/pairings → establish pairing =====

	body, status := wcPost(t, base, "/v1/pairings", map[string]any{
		"uri": "wc:7e2d0a3f6b4c1e8d9a0f5b3c2e1f4a7d@2?relay-protocol=irn&symKey=abc123def456",
	})
	if status != 200 {
		t.Fatalf("pairings -> status %d, want 200; body %s", status, body)
	}
	var pairing map[string]any
	json.Unmarshal([]byte(body), &pairing)
	pairingTopic, ok := pairing["topic"].(string)
	if !ok || pairingTopic == "" {
		t.Fatalf("pairing topic = %v, want non-empty string", pairing["topic"])
	}
	if pairing["relay"] == nil {
		t.Fatal("pairing: missing relay")
	}
	relayObj := pairing["relay"].(map[string]any)
	if relayObj["protocol"] != "irn" {
		t.Fatalf("relay protocol = %v, want 'irn'", relayObj["protocol"])
	}

	// ===== POST /v1/sessions → propose session =====

	body, status = wcPost(t, base, "/v1/sessions", map[string]any{
		"pairingTopic": pairingTopic,
		"requiredNamespaces": map[string]any{
			"eip155": map[string]any{
				"chains":  []string{"eip155:1"},
				"methods": []string{"eth_sendTransaction", "personal_sign"},
				"events":  []string{"chainChanged", "accountsChanged"},
			},
		},
	})
	if status != 200 {
		t.Fatalf("propose session -> status %d, want 200; body %s", status, body)
	}
	var sessionProp map[string]any
	json.Unmarshal([]byte(body), &sessionProp)
	topic, ok := sessionProp["topic"].(string)
	if !ok || topic == "" {
		t.Fatalf("session topic = %v, want non-empty string", sessionProp["topic"])
	}
	if sessionProp["acknowledged"] != false {
		t.Fatalf("session acknowledged = %v, want false", sessionProp["acknowledged"])
	}

	// ===== POST /v1/sessions/{topic}/approve → acknowledge =====

	body, status = wcPost(t, base, "/v1/sessions/"+topic+"/approve", map[string]any{})
	if status != 200 {
		t.Fatalf("approve session -> status %d, want 200; body %s", status, body)
	}
	var approved map[string]any
	json.Unmarshal([]byte(body), &approved)
	if approved["acknowledged"] != true {
		t.Fatalf("acknowledged = %v, want true", approved["acknowledged"])
	}
	ns := approved["namespaces"].(map[string]any)
	eip155 := ns["eip155"].(map[string]any)
	accounts, ok := eip155["accounts"].([]any)
	if !ok || len(accounts) == 0 {
		t.Fatalf("accounts = %v, want non-empty array", eip155["accounts"])
	}
	accountStr, ok := accounts[0].(string)
	if !ok || accountStr == "" {
		t.Fatalf("account[0] = %v, want non-empty string", accounts[0])
	}
	// Account should be in the format eip155:chainId:address
	if accountStr[:8] != "eip155:1" {
		t.Fatalf("account[0] = %v, want eip155:1:0x...", accountStr)
	}
	methods, ok := eip155["methods"].([]any)
	if !ok || len(methods) < 2 {
		t.Fatalf("methods = %v, want at least 2", eip155["methods"])
	}

	// ===== POST /v1/sessions/{topic}/request (eth_requestAccounts) =====

	body, status = wcPost(t, base, "/v1/sessions/"+topic+"/request", map[string]any{
		"request": map[string]any{
			"method": "eth_requestAccounts",
			"params": []any{},
		},
	})
	if status != 200 {
		t.Fatalf("request eth_requestAccounts -> status %d, want 200; body %s", status, body)
	}
	var reqResp1 map[string]any
	json.Unmarshal([]byte(body), &reqResp1)
	if reqResp1["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v, want '2.0'", reqResp1["jsonrpc"])
	}
	if reqResp1["topic"] != topic {
		t.Fatalf("topic = %v, want %v", reqResp1["topic"], topic)
	}
	accountsResult, ok := reqResp1["result"].([]any)
	if !ok || len(accountsResult) == 0 {
		t.Fatalf("result = %v, want array of addresses", reqResp1["result"])
	}
	addrStr, ok := accountsResult[0].(string)
	if !ok || addrStr[:2] != "0x" {
		t.Fatalf("result[0] = %v, want 0x... address", accountsResult[0])
	}

	// ===== POST /v1/sessions/{topic}/request (personal_sign) =====

	body, status = wcPost(t, base, "/v1/sessions/"+topic+"/request", map[string]any{
		"request": map[string]any{
			"method": "personal_sign",
			"params": []any{"0x48656c6c6f", "0x1234567890abcdef1234567890abcdef12345678"},
		},
	})
	if status != 200 {
		t.Fatalf("request personal_sign -> status %d, want 200; body %s", status, body)
	}
	var reqResp2 map[string]any
	json.Unmarshal([]byte(body), &reqResp2)
	sig, ok := reqResp2["result"].(string)
	if !ok || len(sig) != 66 || sig[:2] != "0x" {
		t.Fatalf("personal_sign result = %v, want 0x + 64 hex chars", reqResp2["result"])
	}

	// ===== POST /v1/sessions/{topic}/request (eth_sendTransaction) =====

	body, status = wcPost(t, base, "/v1/sessions/"+topic+"/request", map[string]any{
		"request": map[string]any{
			"method": "eth_sendTransaction",
			"params": []any{
				map[string]any{
					"from":  "0x1234567890abcdef1234567890abcdef12345678",
					"to":    "0x3535353535353535353535353535353535353535",
					"value": "0xde0b6b3a7640000",
					"gas":   "0x5208",
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("request eth_sendTransaction -> status %d, want 200; body %s", status, body)
	}
	var reqResp3 map[string]any
	json.Unmarshal([]byte(body), &reqResp3)
	txHash, ok := reqResp3["result"].(string)
	if !ok || len(txHash) != 66 || txHash[:2] != "0x" {
		t.Fatalf("eth_sendTransaction result = %v, want 0x + 64 hex chars", reqResp3["result"])
	}

	// ===== GET /v1/sessions → list (stateful) =====

	body, status = wcGet(t, base, "/v1/sessions")
	if status != 200 {
		t.Fatalf("list sessions -> status %d, want 200; body %s", status, body)
	}
	var sessionsList []any
	json.Unmarshal([]byte(body), &sessionsList)
	if len(sessionsList) == 0 {
		t.Fatalf("sessions list = %v, want at least 1", sessionsList)
	}
	firstSession := sessionsList[0].(map[string]any)
	if firstSession["topic"] != topic {
		t.Fatalf("sessions[0] topic = %v, want %v", firstSession["topic"], topic)
	}

	// ===== POST /v1/sessions/{topic}/extend → refresh expiry =====

	body, status = wcPost(t, base, "/v1/sessions/"+topic+"/extend", map[string]any{})
	if status != 200 {
		t.Fatalf("extend session -> status %d, want 200; body %s", status, body)
	}
	var extendResp map[string]any
	json.Unmarshal([]byte(body), &extendResp)
	if extendResp["topic"] != topic {
		t.Fatalf("extend topic = %v, want %v", extendResp["topic"], topic)
	}
	if extendResp["expiry"] == nil {
		t.Fatal("extend: missing expiry")
	}

	// ===== DELETE /v1/sessions/{topic} → disconnect =====

	body, status = wcDelete(t, base, "/v1/sessions/"+topic)
	if status != 200 {
		t.Fatalf("disconnect -> status %d, want 200; body %s", status, body)
	}
	var discResp map[string]any
	json.Unmarshal([]byte(body), &discResp)
	if discResp["acknowledged"] != false {
		t.Fatalf("disconnect acknowledged = %v, want false", discResp["acknowledged"])
	}

	// Verify session is gone from list.
	body, _ = wcGet(t, base, "/v1/sessions")
	json.Unmarshal([]byte(body), &sessionsList)
	for _, s := range sessionsList {
		sm := s.(map[string]any)
		if sm["topic"] == topic {
			t.Fatalf("session %v still in list after disconnect", topic)
		}
	}

	// ===== Error: approve nonexistent session =====

	body, status = wcPost(t, base, "/v1/sessions/nonexistent/approve", map[string]any{})
	if status != 404 {
		t.Fatalf("approve nonexistent -> status %d, want 404; body %s", status, body)
	}
}

// === helpers ===

func wcPost(t *testing.T, base, path string, bodyObj map[string]any) (string, int) {
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

func wcGet(t *testing.T, base, path string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func wcDelete(t *testing.T, base, path string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
