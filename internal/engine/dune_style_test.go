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

// TestDuneStyleAdapter exercises the Dune-style adapter end-to-end:
//
//   - execute query → PENDING
//   - poll status → COMPLETED
//   - get results → rows with metadata
//   - inline result → COMPLETED + rows
//   - auth validate → valid
//   - 401 without auth
func TestDuneStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "dune-style")
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
			"dune": {Adapter: absAdapterDir},
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

	base := addrs["dune"]
	const token = "test-token-dune"

	// ===== Execute query =====

	body, status := dunePost(t, base+"/api/v1/query/12345/execute", token, map[string]any{
		"query_parameters": map[string]any{},
	})
	if status != 200 {
		t.Fatalf("execute -> status %d, want 200; body %s", status, body)
	}
	var execResp map[string]any
	if err := json.Unmarshal([]byte(body), &execResp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	execID, ok := execResp["execution_id"].(string)
	if !ok || execID == "" {
		t.Fatalf("execution_id = %v, want non-empty string", execResp["execution_id"])
	}
	if execResp["state"] != "QUERY_STATE_PENDING" {
		t.Fatalf("state = %v, want QUERY_STATE_PENDING", execResp["state"])
	}

	// ===== 401 without auth =====

	_, status = duneNoAuth(t, base+"/api/v1/query/12345/execute")
	if status != 401 {
		t.Fatalf("no auth -> status %d, want 401", status)
	}

	// ===== Poll status → COMPLETED =====

	body, status = duneGet(t, base+"/api/v1/execution/"+execID+"/status", token)
	if status != 200 {
		t.Fatalf("get status -> status %d, want 200; body %s", status, body)
	}
	var statusResp map[string]any
	if err := json.Unmarshal([]byte(body), &statusResp); err != nil {
		t.Fatalf("unmarshal status: %v (body %s)", err, body)
	}
	if statusResp["state"] != "QUERY_STATE_COMPLETED" {
		t.Fatalf("status state = %v, want QUERY_STATE_COMPLETED", statusResp["state"])
	}

	// ===== Get results → rows =====

	body, status = duneGet(t, base+"/api/v1/execution/"+execID+"/results", token)
	if status != 200 {
		t.Fatalf("get results -> status %d, want 200; body %s", status, body)
	}
	var resultsResp map[string]any
	if err := json.Unmarshal([]byte(body), &resultsResp); err != nil {
		t.Fatalf("unmarshal results: %v (body %s)", err, body)
	}
	if resultsResp["state"] != "QUERY_STATE_COMPLETED" {
		t.Fatalf("results state = %v, want QUERY_STATE_COMPLETED", resultsResp["state"])
	}
	result, ok := resultsResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want object", resultsResp["result"])
	}
	rows, ok := result["rows"].([]any)
	if !ok || len(rows) < 1 {
		t.Fatalf("rows = %v, want non-empty array", result["rows"])
	}
	metadata, ok := result["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %v, want object", result["metadata"])
	}
	if metadata["row_count"] == nil {
		t.Fatalf("metadata.row_count = %v, want non-nil", metadata["row_count"])
	}

	// ===== Deterministic: same query gives same rows =====

	// Execute again for same query_id.
	body2, _ := dunePost(t, base+"/api/v1/query/12345/execute", token, map[string]any{})
	var execResp2 map[string]any
	_ = json.Unmarshal([]byte(body2), &execResp2)
	execID2, _ := execResp2["execution_id"].(string)

	body3, _ := duneGet(t, base+"/api/v1/execution/"+execID2+"/results", token)
	var resultsResp2 map[string]any
	_ = json.Unmarshal([]byte(body3), &resultsResp2)
	result2 := resultsResp2["result"].(map[string]any)
	rows2 := result2["rows"].([]any)

	// Compare first row's amount_usd.
	firstRow := rows[0].(map[string]any)
	firstRow2 := rows2[0].(map[string]any)
	if firstRow["amount_usd"] != firstRow2["amount_usd"] {
		t.Fatalf("deterministic check: amount_usd = %v vs %v", firstRow["amount_usd"], firstRow2["amount_usd"])
	}

	// ===== Inline result =====

	body, status = dunePost(t, base+"/api/v1/query/99999/result", token, map[string]any{
		"query_parameters": map[string]any{},
	})
	if status != 200 {
		t.Fatalf("inline result -> status %d, want 200; body %s", status, body)
	}
	var inlineResp map[string]any
	if err := json.Unmarshal([]byte(body), &inlineResp); err != nil {
		t.Fatalf("unmarshal inline: %v (body %s)", err, body)
	}
	if inlineResp["state"] != "QUERY_STATE_COMPLETED" {
		t.Fatalf("inline state = %v, want QUERY_STATE_COMPLETED", inlineResp["state"])
	}
	inlineResult, ok := inlineResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("inline result = %v, want object", inlineResp["result"])
	}
	inlineRows, ok := inlineResult["rows"].([]any)
	if !ok || len(inlineRows) < 1 {
		t.Fatalf("inline rows = %v, want non-empty array", inlineResult["rows"])
	}

	// ===== Auth validate =====

	body, status = duneGet(t, base+"/api/v1/auth/validate", token)
	if status != 200 {
		t.Fatalf("auth validate -> status %d, want 200; body %s", status, body)
	}
	var validateResp map[string]any
	if err := json.Unmarshal([]byte(body), &validateResp); err != nil {
		t.Fatalf("unmarshal validate: %v (body %s)", err, body)
	}
	if validateResp["valid"] != true {
		t.Fatalf("valid = %v, want true", validateResp["valid"])
	}

	// ===== Unknown execution → 404 =====

	_, status = duneGet(t, base+"/api/v1/execution/nonexistent-id/status", token)
	if status != 404 {
		t.Fatalf("unknown execution -> status %d, want 404", status)
	}
}

// === Dune test helpers ===

func duneGet(t *testing.T, rawurl, token string) (string, int) {
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

func dunePost(t *testing.T, rawurl, token string, body any) (string, int) {
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

func duneNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Post(rawurl, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
