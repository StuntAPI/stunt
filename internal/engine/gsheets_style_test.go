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

// TestGSheetsStyleAdapter exercises the gsheets-style adapter:
//
//   - Create spreadsheet → metadata
//   - PUT values A1:B2 → write cells
//   - GET values → returns the written cells (round-trip)
//   - Append values after existing data
//   - 401 without bearer
func TestGSheetsStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "gsheets-style")
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
			"sheets": {Adapter: absAdapterDir},
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

	base := addrs["sheets"]
	token := "mock-oauth2-token"

	// ===== Create spreadsheet =====

	createBody := map[string]any{
		"properties": map[string]any{
			"title": "My Test Sheet",
		},
	}
	body, status := gsheetsPostJSON(t, base+"/v4/spreadsheets", token, createBody)
	if status != 200 {
		t.Fatalf("create spreadsheet -> status %d, want 200; body %s", status, body)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("unmarshal created spreadsheet: %v (body %s)", err, body)
	}
	ssID, ok := created["spreadsheetId"].(string)
	if !ok || ssID == "" {
		t.Fatalf("spreadsheetId = %v, want non-empty string", created["spreadsheetId"])
	}
	props, ok := created["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %v, want map", created["properties"])
	}
	if props["title"] != "My Test Sheet" {
		t.Fatalf("title = %v, want 'My Test Sheet'", props["title"])
	}
	sheets, ok := created["sheets"].([]any)
	if !ok || len(sheets) == 0 {
		t.Fatalf("sheets = %v, want non-empty list", created["sheets"])
	}

	// ===== GET spreadsheet metadata =====

	body, status = gsheetsGet(t, base+"/v4/spreadsheets/"+ssID, token)
	if status != 200 {
		t.Fatalf("get spreadsheet -> status %d, want 200; body %s", status, body)
	}
	var ssMeta map[string]any
	if err := json.Unmarshal([]byte(body), &ssMeta); err != nil {
		t.Fatalf("unmarshal spreadsheet metadata: %v (body %s)", err, body)
	}
	if ssMeta["spreadsheetId"] != ssID {
		t.Fatalf("spreadsheetId = %v, want %s", ssMeta["spreadsheetId"], ssID)
	}

	// ===== PUT values A1:B2 =====

	putBody := map[string]any{
		"values": [][]string{
			{"Name", "City"},
			{"Alice", "NYC"},
		},
	}
	body, status = gsheetsDo(t, "PUT", base+"/v4/spreadsheets/"+ssID+"/values/Sheet1!A1:B2", token, putBody)
	if status != 200 {
		t.Fatalf("PUT values -> status %d, want 200; body %s", status, body)
	}
	var putResp map[string]any
	if err := json.Unmarshal([]byte(body), &putResp); err != nil {
		t.Fatalf("unmarshal PUT response: %v (body %s)", err, body)
	}
	if putResp["updatedRows"] != float64(2) {
		t.Fatalf("updatedRows = %v, want 2", putResp["updatedRows"])
	}
	if putResp["updatedColumns"] != float64(2) {
		t.Fatalf("updatedColumns = %v, want 2", putResp["updatedColumns"])
	}
	if putResp["updatedCells"] != float64(4) {
		t.Fatalf("updatedCells = %v, want 4", putResp["updatedCells"])
	}

	// ===== GET values — round-trip test =====

	body, status = gsheetsGet(t, base+"/v4/spreadsheets/"+ssID+"/values/Sheet1!A1:B3", token)
	if status != 200 {
		t.Fatalf("GET values -> status %d, want 200; body %s", status, body)
	}
	var getResp map[string]any
	if err := json.Unmarshal([]byte(body), &getResp); err != nil {
		t.Fatalf("unmarshal GET values: %v (body %s)", err, body)
	}
	if getResp["majorDimension"] != "ROWS" {
		t.Fatalf("majorDimension = %v, want ROWS", getResp["majorDimension"])
	}
	values, ok := getResp["values"].([]any)
	if !ok || len(values) != 2 {
		t.Fatalf("values = %v, want 2 rows (trailing empty trimmed)", getResp["values"])
	}
	row0 := values[0].([]any)
	if row0[0] != "Name" || row0[1] != "City" {
		t.Fatalf("row 0 = %v, want [Name City]", row0)
	}
	row1 := values[1].([]any)
	if row1[0] != "Alice" || row1[1] != "NYC" {
		t.Fatalf("row 1 = %v, want [Alice NYC]", row1)
	}

	// ===== Append values =====

	appendBody := map[string]any{
		"values": [][]string{
			{"Bob", "LA"},
		},
	}
	body, status = gsheetsPostJSON(t, base+"/v4/spreadsheets/"+ssID+"/values/Sheet1!A1:B10:append", token, appendBody)
	if status != 200 {
		t.Fatalf("append values -> status %d, want 200; body %s", status, body)
	}
	var appendResp map[string]any
	if err := json.Unmarshal([]byte(body), &appendResp); err != nil {
		t.Fatalf("unmarshal append response: %v (body %s)", err, body)
	}
	updates, ok := appendResp["updates"].(map[string]any)
	if !ok {
		t.Fatalf("updates = %v, want map", appendResp["updates"])
	}
	if updates["updatedRows"] != float64(1) {
		t.Fatalf("updatedRows = %v, want 1", updates["updatedRows"])
	}

	// Verify append is readable.
	body, status = gsheetsGet(t, base+"/v4/spreadsheets/"+ssID+"/values/Sheet1!A1:B10", token)
	json.Unmarshal([]byte(body), &getResp)
	values = getResp["values"].([]any)
	if len(values) != 3 {
		t.Fatalf("after append, values len = %d, want 3", len(values))
	}
	row2 := values[2].([]any)
	if row2[0] != "Bob" || row2[1] != "LA" {
		t.Fatalf("row 2 = %v, want [Bob LA]", row2)
	}

	// ===== 401 without bearer =====

	body, status = gsheetsGet(t, base+"/v4/spreadsheets/"+ssID+"/values/Sheet1!A1:B2", "")
	if status != 401 {
		t.Fatalf("GET values without token -> status %d, want 401; body %s", status, body)
	}
	if !strings.Contains(body, "UNAUTHENTICATED") {
		t.Fatalf("error body should contain UNAUTHENTICATED: %s", body)
	}
}

// === Helpers ===

func gsheetsGet(t *testing.T, url, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func gsheetsPostJSON(t *testing.T, url, token string, body map[string]any) (string, int) {
	t.Helper()
	return gsheetsDo(t, "POST", url, token, body)
}

func gsheetsDo(t *testing.T, method, url, token string, body map[string]any) (string, int) {
	t.Helper()
	var req *http.Request
	var err error
	if body != nil {
		data, _ := json.Marshal(body)
		req, err = http.NewRequest(method, url, bytes.NewReader(data))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
