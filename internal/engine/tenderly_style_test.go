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

// TestTenderlyStyleAdapter exercises the Tenderly-style adapter end-to-end:
//
//   - simulate transaction → status true, gas_used, call trace
//   - simulate bundle → multiple results
//   - list networks
//   - 401 without auth
func TestTenderlyStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "tenderly-style")
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
			"tenderly": {Adapter: absAdapterDir},
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

	base := addrs["tenderly"]
	const token = "test-token-tenderly"
	simURL := base + "/api/v1/account/myacct/project/myproject/simulate"

	// ===== Simulate transaction =====

	body, status := tenderlyPost(t, simURL, token, map[string]any{
		"network_id":        "1",
		"block_number":      19000000,
		"transaction_index": 0,
		"accounts":          map[string]any{},
		"transaction": map[string]any{
			"from":      "0x742d35Cc6634C0532925a3b844Bc454e4438f44e",
			"to":        "0xdAC17F958D2ee523a2206206994597C13D831ec7",
			"gas":       100000,
			"gas_price": "1000000000",
			"value":     "0",
			"input":     "0xa9059cbb0000000000000000000000001234567890abcdef1234567890abcdef1234567800000000000000000000000000000000000000000000000000000000000f4240",
		},
	})
	if status != 200 {
		t.Fatalf("simulate -> status %d, want 200; body %s", status, body)
	}
	var simResp map[string]any
	if err := json.Unmarshal([]byte(body), &simResp); err != nil {
		t.Fatalf("unmarshal simulate: %v (body %s)", err, body)
	}
	tx, ok := simResp["transaction"].(map[string]any)
	if !ok {
		t.Fatalf("transaction = %v, want object", simResp["transaction"])
	}
	if tx["status"] != true {
		t.Fatalf("transaction.status = %v, want true", tx["status"])
	}
	if tx["gas_used"] == nil {
		t.Fatalf("transaction.gas_used = %v, want non-nil", tx["gas_used"])
	}
	trace, ok := simResp["sim_call_trace"].(map[string]any)
	if !ok {
		t.Fatalf("sim_call_trace = %v, want object", simResp["sim_call_trace"])
	}
	if trace["status"] != true {
		t.Fatalf("trace.status = %v, want true", trace["status"])
	}

	// ===== 401 without auth =====

	_, status = tenderlyNoAuth(t, simURL)
	if status != 401 {
		t.Fatalf("no auth -> status %d, want 401", status)
	}

	// ===== Simulate bundle =====

	bundleURL := base + "/api/v1/account/myacct/project/myproject/simulate-bundle"
	body, status = tenderlyPost(t, bundleURL, token, map[string]any{
		"simulations": []map[string]any{
			{
				"network_id": "1",
				"transaction": map[string]any{
					"from":  "0x742d35Cc6634C0532925a3b844Bc454e4438f44e",
					"to":    "0xdAC17F958D2ee523a2206206994597C13D831ec7",
					"gas":   100000,
					"value": "0",
					"input": "0x",
				},
			},
			{
				"network_id": "1",
				"transaction": map[string]any{
					"from":  "0xdAC17F958D2ee523a2206206994597C13D831ec7",
					"to":    "0x742d35Cc6634C0532925a3b844Bc454e4438f44e",
					"gas":   21000,
					"value": "1000000",
					"input": "0x",
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("simulate bundle -> status %d, want 200; body %s", status, body)
	}
	var bundleResp map[string]any
	if err := json.Unmarshal([]byte(body), &bundleResp); err != nil {
		t.Fatalf("unmarshal bundle: %v (body %s)", err, body)
	}
	results, ok := bundleResp["simulation_results"].([]any)
	if !ok {
		t.Fatalf("simulation_results = %v, want array", bundleResp["simulation_results"])
	}
	if len(results) != 2 {
		t.Fatalf("results count = %d, want 2", len(results))
	}

	// ===== List networks =====

	body, status = tenderlyGet(t, base+"/api/v1/networks", token)
	if status != 200 {
		t.Fatalf("list networks -> status %d, want 200; body %s", status, body)
	}
	var networks []any
	if err := json.Unmarshal([]byte(body), &networks); err != nil {
		t.Fatalf("unmarshal networks: %v (body %s)", err, body)
	}
	if len(networks) < 1 {
		t.Fatalf("networks count = %d, want >= 1", len(networks))
	}
}

// === Tenderly test helpers ===

func tenderlyGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", rawurl, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func tenderlyPost(t *testing.T, rawurl, token string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func tenderlyNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Post(rawurl, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
