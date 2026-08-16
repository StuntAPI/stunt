package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestDuneStyleAdapter exercises the Dune-style adapter end-to-end:
//
//   - execute query → PENDING
//   - poll status immediately → pre-terminal (PENDING/EXECUTING)
//   - simulate_fail execution → FAILED terminal, no results
//   - poll status after the 3s window → COMPLETED
//   - get results → rows with metadata (404 before completion)
//   - inline result → COMPLETED + rows
//   - auth validate → valid
//   - 401 without auth
//   - missing required query parameter → 400 {"error": "Bad Request"}
//   - parameterized executions → distinct rows per parameter set
//   - results pagination: limit/offset honored, next_uri followable
//   - results CSV variant → text/csv honoring limit
//   - unknown execution → 404 {"error": "Object not found"}
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

	// ===== Missing required query parameter → 400 =====
	// Query 4242 declares a required TEXT parameter (wallet_address).

	body, status = dunePost(t, base+"/api/v1/query/4242/execute", token, map[string]any{
		"query_parameters": map[string]any{},
	})
	if status != 400 {
		t.Fatalf("execute missing required param -> status %d, want 400; body %s", status, body)
	}
	var paramErr map[string]any
	if err := json.Unmarshal([]byte(body), &paramErr); err != nil {
		t.Fatalf("unmarshal param error: %v (body %s)", err, body)
	}
	if paramErr["error"] != "Bad Request" {
		t.Fatalf("missing required param error = %v, want \"Bad Request\" (Dune's documented 400 envelope)", paramErr["error"])
	}

	// ===== Poll status immediately → still pre-terminal =====

	body, status = duneGet(t, base+"/api/v1/execution/"+execID+"/status", token)
	if status != 200 {
		t.Fatalf("get status (early) -> status %d, want 200; body %s", status, body)
	}
	var statusResp map[string]any
	if err := json.Unmarshal([]byte(body), &statusResp); err != nil {
		t.Fatalf("unmarshal status: %v (body %s)", err, body)
	}
	earlyState, _ := statusResp["state"].(string)
	if earlyState == "QUERY_STATE_COMPLETED" || earlyState == "QUERY_STATE_FAILED" {
		t.Fatalf("early status state = %v, want pre-terminal (PENDING/EXECUTING)", earlyState)
	}

	// ===== Execute with simulate_fail → FAILED terminal =====

	bodyF, _ := dunePost(t, base+"/api/v1/query/12345/execute", token, map[string]any{
		"simulate_fail": true,
	})
	var execRespF map[string]any
	if err := json.Unmarshal([]byte(bodyF), &execRespF); err != nil {
		t.Fatalf("unmarshal fail exec: %v (body %s)", err, bodyF)
	}
	failID, _ := execRespF["execution_id"].(string)
	if failID == "" {
		t.Fatalf("fail execution_id = %v, want non-empty string", execRespF["execution_id"])
	}

	// Sleep past the simulated execution window (1s running / 3s done).
	time.Sleep(3200 * time.Millisecond)

	// ===== Poll status → COMPLETED =====

	body, status = duneGet(t, base+"/api/v1/execution/"+execID+"/status", token)
	if status != 200 {
		t.Fatalf("get status -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &statusResp); err != nil {
		t.Fatalf("unmarshal status: %v (body %s)", err, body)
	}
	if statusResp["state"] != "QUERY_STATE_COMPLETED" {
		t.Fatalf("status state = %v, want QUERY_STATE_COMPLETED", statusResp["state"])
	}

	// ===== Fail-injected execution → FAILED / no results =====

	body, status = duneGet(t, base+"/api/v1/execution/"+failID+"/status", token)
	if status != 200 {
		t.Fatalf("fail status -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &statusResp); err != nil {
		t.Fatalf("unmarshal fail status: %v (body %s)", err, body)
	}
	if statusResp["state"] != "QUERY_STATE_FAILED" {
		t.Fatalf("fail status state = %v, want QUERY_STATE_FAILED", statusResp["state"])
	}
	_, status = duneGet(t, base+"/api/v1/execution/"+failID+"/results", token)
	if status != 404 {
		t.Fatalf("fail results -> status %d, want 404", status)
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

	// Execute again for same query_id. Results 404 until the window elapses.
	body2, _ := dunePost(t, base+"/api/v1/query/12345/execute", token, map[string]any{})
	var execResp2 map[string]any
	_ = json.Unmarshal([]byte(body2), &execResp2)
	execID2, _ := execResp2["execution_id"].(string)

	_, status = duneGet(t, base+"/api/v1/execution/"+execID2+"/results", token)
	if status != 404 {
		t.Fatalf("early results -> status %d, want 404 (execution still running)", status)
	}

	time.Sleep(3200 * time.Millisecond)

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

	// ===== Unknown execution → 404 (documented error envelope) =====

	body, status = duneGet(t, base+"/api/v1/execution/nonexistent-id/status", token)
	if status != 404 {
		t.Fatalf("unknown execution -> status %d, want 404", status)
	}
	var notFound map[string]any
	if err := json.Unmarshal([]byte(body), &notFound); err != nil {
		t.Fatalf("unmarshal not found: %v (body %s)", err, body)
	}
	if notFound["error"] != "Object not found" {
		t.Fatalf("unknown execution error = %v, want \"Object not found\"", notFound["error"])
	}

	// ===== Parameterized execution: distinct rows per parameter set =====
	// Query 4242 requires wallet_address (SDK {type,value} shape accepted)
	// and takes an optional NUMBER parameter (plain value shape accepted).

	bodyA, _ := dunePost(t, base+"/api/v1/query/4242/execute", token, map[string]any{
		"query_parameters": map[string]any{
			"wallet_address": map[string]any{"type": "TEXT", "value": "0xAbC123dEf456"},
			"min_usd":        250,
		},
	})
	var execA map[string]any
	_ = json.Unmarshal([]byte(bodyA), &execA)
	execAID, _ := execA["execution_id"].(string)
	if execAID == "" {
		t.Fatalf("parameterized execution_id = %v, want non-empty (body %s)", execA["execution_id"], bodyA)
	}

	bodyB, _ := dunePost(t, base+"/api/v1/query/4242/execute", token, map[string]any{
		"query_parameters": map[string]any{
			"wallet_address": "0xFeD987aBc321",
			"min_usd":        250,
		},
	})
	var execB map[string]any
	_ = json.Unmarshal([]byte(bodyB), &execB)
	execBID, _ := execB["execution_id"].(string)
	if execBID == "" {
		t.Fatalf("parameterized (plain-value shape) execution_id = %v, want non-empty (body %s)", execB["execution_id"], bodyB)
	}

	// Query 3971 returns 12 rows (pagination fixture) honoring token_symbol.
	bodyC, _ := dunePost(t, base+"/api/v1/query/3971/execute", token, map[string]any{
		"query_parameters": map[string]any{
			"token_symbol": "WETH",
		},
	})
	var execC map[string]any
	_ = json.Unmarshal([]byte(bodyC), &execC)
	execCID, _ := execC["execution_id"].(string)
	if execCID == "" {
		t.Fatalf("pagination execution_id = %v, want non-empty (body %s)", execC["execution_id"], bodyC)
	}

	time.Sleep(3200 * time.Millisecond)

	// Results for parameter set A: envelope carries the integer query_id.
	body, status = duneGet(t, base+"/api/v1/execution/"+execAID+"/results", token)
	if status != 200 {
		t.Fatalf("parameterized results -> status %d, want 200; body %s", status, body)
	}
	var resA map[string]any
	if err := json.Unmarshal([]byte(body), &resA); err != nil {
		t.Fatalf("unmarshal resA: %v (body %s)", err, body)
	}
	if qid, ok := resA["query_id"].(float64); !ok || int(qid) != 4242 {
		t.Fatalf("results query_id = %v, want 4242", resA["query_id"])
	}
	rowsA := resA["result"].(map[string]any)["rows"].([]any)
	if len(rowsA) != 8 {
		t.Fatalf("query 4242 rows = %d, want 8", len(rowsA))
	}

	// A different wallet_address must produce distinct synthetic rows.
	body, status = duneGet(t, base+"/api/v1/execution/"+execBID+"/results", token)
	if status != 200 {
		t.Fatalf("parameterized (B) results -> status %d, want 200; body %s", status, body)
	}
	var resB map[string]any
	if err := json.Unmarshal([]byte(body), &resB); err != nil {
		t.Fatalf("unmarshal resB: %v (body %s)", err, body)
	}
	rowsB := resB["result"].(map[string]any)["rows"].([]any)
	amtA := rowsA[0].(map[string]any)["amount_usd"]
	amtB := rowsB[0].(map[string]any)["amount_usd"]
	if amtA == amtB {
		t.Fatalf("distinct parameter sets produced identical rows: amount_usd = %v", amtA)
	}

	// ===== Result pagination: limit/offset + next_uri =====

	body, status = duneGet(t, base+"/api/v1/execution/"+execCID+"/results?limit=5", token)
	if status != 200 {
		t.Fatalf("results page 1 -> status %d, want 200; body %s", status, body)
	}
	var page1 map[string]any
	if err := json.Unmarshal([]byte(body), &page1); err != nil {
		t.Fatalf("unmarshal page1: %v (body %s)", err, body)
	}
	p1Result := page1["result"].(map[string]any)
	p1Rows := p1Result["rows"].([]any)
	p1Meta := p1Result["metadata"].(map[string]any)
	if len(p1Rows) != 5 {
		t.Fatalf("page 1 rows = %d, want 5 (limit honored)", len(p1Rows))
	}
	if rc, ok := p1Meta["row_count"].(float64); !ok || int(rc) != 5 {
		t.Fatalf("page 1 metadata.row_count = %v, want 5", p1Meta["row_count"])
	}
	if trc, ok := p1Meta["total_row_count"].(float64); !ok || int(trc) != 12 {
		t.Fatalf("metadata.total_row_count = %v, want 12", p1Meta["total_row_count"])
	}
	if sym := p1Rows[0].(map[string]any)["token_symbol"]; sym != "WETH" {
		t.Fatalf("page 1 token_symbol = %v, want WETH (query parameter substituted)", sym)
	}
	nextURI, _ := page1["next_uri"].(string)
	if nextURI == "" {
		t.Fatalf("next_uri = %v, want a continuation URL (12 rows > page size 5)", page1["next_uri"])
	}
	if !strings.Contains(nextURI, "/results?offset=5&limit=5") {
		t.Fatalf("next_uri = %q, want offset=5&limit=5 continuation", nextURI)
	}

	// The returned next_uri is followable directly.
	body, status = duneGet(t, nextURI, token)
	if status != 200 {
		t.Fatalf("results page 2 via next_uri -> status %d, want 200; body %s", status, body)
	}
	var page2 map[string]any
	if err := json.Unmarshal([]byte(body), &page2); err != nil {
		t.Fatalf("unmarshal page2: %v (body %s)", err, body)
	}
	p2Rows := page2["result"].(map[string]any)["rows"].([]any)
	if len(p2Rows) != 5 {
		t.Fatalf("page 2 rows = %d, want 5", len(p2Rows))
	}
	if page2RowsSame(p1Rows, p2Rows) {
		t.Fatalf("page 2 repeated page 1 rows (offset not honored)")
	}

	// The final page has no continuation.
	body, status = duneGet(t, base+"/api/v1/execution/"+execCID+"/results?limit=5&offset=10", token)
	if status != 200 {
		t.Fatalf("results final page -> status %d, want 200; body %s", status, body)
	}
	var page3 map[string]any
	if err := json.Unmarshal([]byte(body), &page3); err != nil {
		t.Fatalf("unmarshal page3: %v (body %s)", err, body)
	}
	p3Rows := page3["result"].(map[string]any)["rows"].([]any)
	if len(p3Rows) != 2 {
		t.Fatalf("final page rows = %d, want 2", len(p3Rows))
	}
	if page3["next_uri"] != nil {
		t.Fatalf("final page next_uri = %v, want null", page3["next_uri"])
	}

	// ===== CSV variant (text/csv via raw body) =====

	csvBody, csvHeader, status := duneGetFull(t, base+"/api/v1/execution/"+execCID+"/results/csv?limit=3", token)
	if status != 200 {
		t.Fatalf("results csv -> status %d, want 200; body %s", status, csvBody)
	}
	if ct := csvHeader.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("csv Content-Type = %q, want text/csv", ct)
	}
	csvLines := strings.Split(strings.TrimSpace(csvBody), "\n")
	if len(csvLines) != 4 {
		t.Fatalf("csv lines = %d, want 4 (header + 3 rows honoring limit); body %q", len(csvLines), csvBody)
	}
	if csvLines[0] != "block_time,protocol,amount_usd,token_symbol" {
		t.Fatalf("csv header = %q, want the result column names", csvLines[0])
	}
	if !strings.Contains(csvLines[1], "WETH") {
		t.Fatalf("csv row 1 = %q, want WETH parameter substitution", csvLines[1])
	}

	// CSV before completion / unknown execution → 404 JSON error envelope.
	_, status = duneGet(t, base+"/api/v1/execution/"+failID+"/results/csv", token)
	if status != 404 {
		t.Fatalf("failed execution csv -> status %d, want 404", status)
	}
	body, status = duneGet(t, base+"/api/v1/execution/nonexistent-id/results/csv", token)
	if status != 404 {
		t.Fatalf("unknown execution csv -> status %d, want 404", status)
	}
	var csvErr map[string]any
	if err := json.Unmarshal([]byte(body), &csvErr); err != nil {
		t.Fatalf("unmarshal csv error: %v (body %s)", err, body)
	}
	if csvErr["error"] != "Object not found" {
		t.Fatalf("csv unknown execution error = %v, want \"Object not found\"", csvErr["error"])
	}
}

// page2RowsSame reports whether two result pages start with the same row.
func page2RowsSame(a, b []any) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	ra, _ := a[0].(map[string]any)
	rb, _ := b[0].(map[string]any)
	if ra == nil || rb == nil {
		return false
	}
	return ra["amount_usd"] == rb["amount_usd"] && ra["block_time"] == rb["block_time"]
}

// === Dune test helpers ===

// duneGetFull is duneGet but also returns the response headers (needed to
// assert the CSV variant's Content-Type).
func duneGetFull(t *testing.T, rawurl, token string) (string, http.Header, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", rawurl, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.Header, resp.StatusCode
}

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
