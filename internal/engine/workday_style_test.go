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

// TestWorkdayStyleAdapter exercises the Workday-style adapter end-to-end:
//
//   - list workers (Workday {data, total, more} shape)
//   - retrieve worker by id
//   - RaaS custom report
//   - compensation
//   - create worker via Create_Worker → appears in workers list
//   - bearer auth check
//   - basic auth check
//   - 401 without auth
func TestWorkdayStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "workday-style")
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
			"workday": {Adapter: absAdapterDir},
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

	base := addrs["workday"]

	const bearerToken = "Bearer mock-workday-token"

	// ===== List workers =====

	body, status := wdAuthGet(t, base+"/wbs/v40.0/staffing/workers", bearerToken)
	if status != 200 {
		t.Fatalf("list workers -> %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list resp: %v (body %s)", err, body)
	}
	data, ok := listResp["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatalf("data = %v, want non-empty array", listResp["data"])
	}
	// Verify Workday pagination shape.
	if _, ok := listResp["total"]; !ok {
		t.Fatalf("total field missing from response: %v", listResp)
	}
	if _, ok := listResp["more"]; !ok {
		t.Fatalf("more field missing from response: %v", listResp)
	}
	// Verify worker shape.
	worker0 := data[0].(map[string]any)
	if _, ok := worker0["id"].(string); !ok {
		t.Fatalf("worker id = %v, want string", worker0["id"])
	}
	if _, ok := worker0["descriptor"].(string); !ok {
		t.Fatalf("worker descriptor = %v, want string", worker0["descriptor"])
	}

	originalCount := len(data)

	// ===== Retrieve worker by id =====

	body, status = wdAuthGet(t, base+"/wbs/v40.0/staffing/workers/1", bearerToken)
	if status != 200 {
		t.Fatalf("get worker/1 -> %d, want 200; body %s", status, body)
	}
	var worker map[string]any
	if err := json.Unmarshal([]byte(body), &worker); err != nil {
		t.Fatalf("unmarshal worker: %v (body %s)", err, body)
	}
	if worker["id"] != "1" {
		t.Fatalf("worker id = %v, want '1'", worker["id"])
	}
	workerID, ok := worker["workerID"].(map[string]any)
	if !ok {
		t.Fatalf("workerID = %v, want object", worker["workerID"])
	}
	if workerID["id"] != "1" {
		t.Fatalf("workerID.id = %v, want '1'", workerID["id"])
	}

	// ===== 404 for unknown worker =====

	body, status = wdAuthGet(t, base+"/wbs/v40.0/staffing/workers/9999", bearerToken)
	if status != 404 {
		t.Fatalf("get worker/9999 -> %d, want 404; body %s", status, body)
	}

	// ===== RaaS custom report =====

	body, status = wdAuthGet(t, base+"/ccx/v1/test_tenant/RaaS/Custom_Report", bearerToken)
	if status != 200 {
		t.Fatalf("custom report -> %d, want 200; body %s", status, body)
	}
	var reportResp map[string]any
	if err := json.Unmarshal([]byte(body), &reportResp); err != nil {
		t.Fatalf("unmarshal report resp: %v (body %s)", err, body)
	}
	reportEntries, ok := reportResp["Report_Entry"].([]any)
	if !ok || len(reportEntries) == 0 {
		t.Fatalf("Report_Entry = %v, want non-empty array", reportResp["Report_Entry"])
	}
	entry0 := reportEntries[0].(map[string]any)
	if _, ok := entry0["WorkerID"].(string); !ok {
		t.Fatalf("WorkerID = %v, want string", entry0["WorkerID"])
	}

	// ===== Compensation =====

	body, status = wdAuthGet(t, base+"/wbs/v40.0/compensation/workers/1/compensation", bearerToken)
	if status != 200 {
		t.Fatalf("compensation -> %d, want 200; body %s", status, body)
	}
	var compResp map[string]any
	if err := json.Unmarshal([]byte(body), &compResp); err != nil {
		t.Fatalf("unmarshal compensation resp: %v (body %s)", err, body)
	}
	compData, ok := compResp["data"].([]any)
	if !ok || len(compData) == 0 {
		t.Fatalf("compensation data = %v, want non-empty array", compResp["data"])
	}

	// ===== Create worker via Create_Worker (RaaS) =====

	body, status = wdAuthPostJSON(t, base+"/ccx/v1/test_tenant/staffing/Create_Worker", bearerToken, map[string]any{
		"Worker_Name":        "Test Worker",
		"Primary_Work_Email": "testworker@example.net",
		"Primary_Employment_Reference": map[string]any{
			"id":         "4",
			"descriptor": "Regular Full-Time",
		},
	})
	if status != 200 {
		t.Fatalf("create worker -> %d, want 200; body %s", status, body)
	}
	var createdWorker map[string]any
	if err := json.Unmarshal([]byte(body), &createdWorker); err != nil {
		t.Fatalf("unmarshal created worker: %v (body %s)", err, body)
	}
	createdID, ok := createdWorker["id"].(string)
	if !ok || createdID == "" {
		t.Fatalf("created worker id = %v, want non-empty string", createdWorker["id"])
	}

	// ===== Created worker appears in workers list (STATEFUL) =====

	body, status = wdAuthGet(t, base+"/wbs/v40.0/staffing/workers", bearerToken)
	if status != 200 {
		t.Fatalf("list workers (after create) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list resp (after create): %v (body %s)", err, body)
	}
	data, _ = listResp["data"].([]any)
	if len(data) != originalCount+1 {
		t.Fatalf("data count = %d, want %d (created worker must appear)", len(data), originalCount+1)
	}
	found := false
	for _, w := range data {
		wm := w.(map[string]any)
		if wm["id"] == createdID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created worker %s not found in workers list", createdID)
	}

	// ===== Basic auth also works =====

	body, status = wdAuthGet(t, base+"/wbs/v40.0/staffing/workers", "Basic dXNlcjpwYXNz")
	if status != 200 {
		t.Fatalf("list workers with Basic auth -> %d, want 200; body %s", status, body)
	}

	// ===== 401 without auth =====

	body, status = wdNoAuthGet(t, base+"/wbs/v40.0/staffing/workers")
	if status != 401 {
		t.Fatalf("list workers without auth -> %d, want 401; body %s", status, body)
	}
	// Verify Workday error envelope.
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal 401 error: %v (body %s)", err, body)
	}
	errs, ok := errResp["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("errors = %v, want non-empty array", errResp["errors"])
	}
	err0 := errs[0].(map[string]any)
	if _, ok := err0["errorCode"].(string); !ok {
		t.Fatalf("errorCode = %v, want string", err0["errorCode"])
	}
}

// === Workday test helpers ===

func wdAuthGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func wdNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func wdAuthPostJSON(t *testing.T, rawurl, auth string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
