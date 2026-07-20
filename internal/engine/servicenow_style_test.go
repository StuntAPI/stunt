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

// TestServicenowStyleAdapter exercises the ServiceNow-style adapter end-to-end:
//
//   - list incidents (ServiceNow {result: [...]} shape)
//   - create incident (INC0010... auto-numbered)
//   - encoded query filter (active=true)
//   - PATCH state
//   - basic auth check
//   - bearer auth check
//   - 401 without auth
func TestServicenowStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "servicenow-style")
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
			"servicenow": {Adapter: absAdapterDir},
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

	base := addrs["servicenow"]

	const basicAuth = "Basic YWRtaW46cGFzcw=="
	const bearerAuth = "Bearer mock-snow-token"

	// ===== List incidents =====

	body, status := snowAuthGet(t, base+"/api/now/table/incident", basicAuth)
	if status != 200 {
		t.Fatalf("list incidents -> %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list resp: %v (body %s)", err, body)
	}
	results, ok := listResp["result"].([]any)
	if !ok || len(results) == 0 {
		t.Fatalf("result = %v, want non-empty array", listResp["result"])
	}
	// Verify incident shape.
	inc0 := results[0].(map[string]any)
	if _, ok := inc0["sys_id"].(string); !ok {
		t.Fatalf("sys_id = %v, want string", inc0["sys_id"])
	}
	if _, ok := inc0["number"].(string); !ok {
		t.Fatalf("number = %v, want string", inc0["number"])
	}

	originalCount := len(results)

	// ===== Create incident =====

	body, status = snowAuthPostJSON(t, base+"/api/now/table/incident", basicAuth, map[string]any{
		"short_description": "Server room temperature critical",
		"description":       "Temperature sensor reports 45C in server room",
		"urgency":           "1",
		"priority":          "1",
	})
	if status != 201 {
		t.Fatalf("create incident -> %d, want 201; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal create resp: %v (body %s)", err, body)
	}
	createdInc, ok := createResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want object", createResp["result"])
	}
	createdID, ok := createdInc["sys_id"].(string)
	if !ok || createdID == "" {
		t.Fatalf("created sys_id = %v, want non-empty string", createdInc["sys_id"])
	}
	createdNumber, ok := createdInc["number"].(string)
	if !ok || createdNumber == "" {
		t.Fatalf("created number = %v, want non-empty string", createdInc["number"])
	}
	// Verify INC prefix and numbering pattern.
	if createdNumber[:3] != "INC" {
		t.Fatalf("created number prefix = %v, want INC", createdNumber[:3])
	}
	if createdNumber[7:8] != "A" {
		t.Fatalf("created number format = %v, want INC0010A0NN pattern", createdNumber)
	}
	if createdInc["short_description"] != "Server room temperature critical" {
		t.Fatalf("short_description = %v, want 'Server room temperature critical'", createdInc["short_description"])
	}

	// ===== Created incident appears in list (STATEFUL) =====

	body, status = snowAuthGet(t, base+"/api/now/table/incident", basicAuth)
	if status != 200 {
		t.Fatalf("list incidents (after create) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list resp (after create): %v (body %s)", err, body)
	}
	results, _ = listResp["result"].([]any)
	if len(results) != originalCount+1 {
		t.Fatalf("result count = %d, want %d (created incident must appear)", len(results), originalCount+1)
	}

	// ===== Encoded query filter (active=true) =====

	body, status = snowAuthGet(t, base+"/api/now/table/incident?sysparm_query=active=true", basicAuth)
	if status != 200 {
		t.Fatalf("list incidents (filtered) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal filtered list: %v (body %s)", err, body)
	}
	results, _ = listResp["result"].([]any)
	// All results should have active=true.
	for _, r := range results {
		rm := r.(map[string]any)
		if rm["active"] != true {
			t.Fatalf("filtered result has active = %v, want true", rm["active"])
		}
	}
	// The resolved incident (active=false) should NOT appear.
	if len(results) == 0 {
		t.Fatal("no active incidents found after filter")
	}

	// ===== Encoded query filter (short_description LIKE Email) =====

	body, status = snowAuthGet(t, base+"/api/now/table/incident?sysparm_query=short_descriptionLIKEEmail", basicAuth)
	if status != 200 {
		t.Fatalf("list incidents (LIKE filter) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal LIKE-filtered list: %v (body %s)", err, body)
	}
	results, _ = listResp["result"].([]any)
	if len(results) == 0 {
		t.Fatal("LIKE filter returned no results for 'Email'")
	}
	for _, r := range results {
		rm := r.(map[string]any)
		desc := rm["short_description"].(string)
		if !contains(desc, "Email") {
			t.Fatalf("LIKE filter returned non-matching result: %s", desc)
		}
	}

	// ===== PATCH state on a record =====

	body, status = snowAuthPatchJSON(t, base+"/api/now/table/incident/sysid_inc_001", basicAuth, map[string]any{
		"state":         "6",
		"state_display": "Resolved",
		"active":        false,
	})
	if status != 200 {
		t.Fatalf("patch incident -> %d, want 200; body %s", status, body)
	}
	var patchResp map[string]any
	if err := json.Unmarshal([]byte(body), &patchResp); err != nil {
		t.Fatalf("unmarshal patch resp: %v (body %s)", err, body)
	}
	patchedInc, ok := patchResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want object", patchResp["result"])
	}
	if patchedInc["state"] != "6" {
		t.Fatalf("state = %v, want '6'", patchedInc["state"])
	}
	if patchedInc["active"] != false {
		t.Fatalf("active = %v, want false", patchedInc["active"])
	}

	// Verify PATCH took effect via GET.
	body, status = snowAuthGet(t, base+"/api/now/table/incident/sysid_inc_001", basicAuth)
	if status != 200 {
		t.Fatalf("get incident (after patch) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &patchResp); err != nil {
		t.Fatalf("unmarshal get (after patch): %v (body %s)", err, body)
	}
	getInc, _ := patchResp["result"].(map[string]any)
	if getInc["state"] != "6" {
		t.Fatalf("state after GET = %v, want '6'", getInc["state"])
	}

	// ===== Bearer auth also works =====

	body, status = snowAuthGet(t, base+"/api/now/table/incident", bearerAuth)
	if status != 200 {
		t.Fatalf("list incidents with bearer -> %d, want 200; body %s", status, body)
	}

	// ===== 401 without auth =====

	body, status = snowNoAuthGet(t, base+"/api/now/table/incident")
	if status != 401 {
		t.Fatalf("list incidents without auth -> %d, want 401; body %s", status, body)
	}
	// Verify ServiceNow error envelope.
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal 401 error: %v (body %s)", err, body)
	}
	errObj, ok := errResp["error"].(map[string]any)
	if !ok {
		t.Fatalf("error = %v, want object", errResp["error"])
	}
	if _, ok := errObj["message"].(string); !ok {
		t.Fatalf("error.message = %v, want string", errObj["message"])
	}

	// ===== DELETE an incident =====

	body, status = snowAuthDelete(t, base+"/api/now/table/incident/sysid_inc_002", basicAuth)
	if status != 204 {
		t.Fatalf("delete incident -> %d, want 204; body %s", status, body)
	}

	// Verify DELETE took effect (404 on GET).
	body, status = snowAuthGet(t, base+"/api/now/table/incident/sysid_inc_002", basicAuth)
	if status != 404 {
		t.Fatalf("get deleted incident -> %d, want 404; body %s", status, body)
	}

	// ===== Import set =====

	body, status = snowAuthPostJSON(t, base+"/api/now/import/u_my_table", basicAuth, map[string]any{
		"records": []map[string]any{
			{"name": "Imported Item A", "value": "100"},
			{"name": "Imported Item B", "value": "200"},
		},
	})
	if status != 201 {
		t.Fatalf("import -> %d, want 201; body %s", status, body)
	}
	var importResp map[string]any
	if err := json.Unmarshal([]byte(body), &importResp); err != nil {
		t.Fatalf("unmarshal import resp: %v (body %s)", err, body)
	}
	importResults, ok := importResp["result"].([]any)
	if !ok || len(importResults) != 2 {
		t.Fatalf("import result = %v, want array of 2", importResp["result"])
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(substr) > 0 && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// === ServiceNow test helpers ===

func snowAuthGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func snowNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func snowAuthPostJSON(t *testing.T, rawurl, auth string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func snowAuthPatchJSON(t *testing.T, rawurl, auth string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PATCH", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func snowAuthDelete(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
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
