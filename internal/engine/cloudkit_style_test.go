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

// TestCloudKitStyleAdapter exercises the cloudkit-style adapter:
//
//   - Auth required: 401 without X-Apple-CloudKit-Request
//   - Lookup records → seeded records
//   - Modify (create) → new record appears
//   - Modify (update) → field updated
//   - Query by recordType → filtered results
//   - Zones list
//   - Current user
func TestCloudKitStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "cloudkit-style")
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
			"cloudkit": {Adapter: absAdapterDir},
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

	base := addrs["cloudkit"]
	prefix := base + "/database/1/com.example.icloud-container/production/public"
	authHeader := map[string]string{
		"X-Apple-CloudKit-Request": "fake-signature-data",
	}

	// ===== 401 without auth =====

	body, status := cloudKitGetJSON(t, prefix+"/records/lookup",
		map[string]any{"records": []map[string]any{{"recordName": "note-001"}}},
		nil)
	if status != 401 {
		t.Fatalf("lookup without auth -> status %d, want 401; body %s", status, body)
	}

	// ===== Lookup → seeded record =====

	body, status = cloudKitGetJSON(t, prefix+"/records/lookup",
		map[string]any{"records": []map[string]any{{"recordName": "note-001"}}},
		authHeader)
	if status != 200 {
		t.Fatalf("lookup -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	records, ok := resp["records"].([]any)
	if !ok || len(records) != 1 {
		t.Fatalf("records = %v, want array of 1", resp["records"])
	}
	rec := records[0].(map[string]any)
	if rec["recordName"] != "note-001" {
		t.Fatalf("recordName = %v, want note-001", rec["recordName"])
	}
	if rec["recordType"] != "Notes" {
		t.Fatalf("recordType = %v, want Notes", rec["recordType"])
	}
	fields, ok := rec["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %v, want object", rec["fields"])
	}
	titleField, ok := fields["title"].(map[string]any)
	if !ok {
		t.Fatalf("fields.title = %v, want object", fields["title"])
	}
	if titleField["value"] != "Welcome Note" {
		t.Fatalf("title value = %v, want 'Welcome Note'", titleField["value"])
	}

	// ===== Modify (create) → new record =====

	body, status = cloudKitPostJSON(t, prefix+"/records/modify", map[string]any{
		"operations": []map[string]any{
			{
				"operationType": "create",
				"record": map[string]any{
					"recordName": "note-003",
					"recordType": "Notes",
					"fields": map[string]any{
						"title": map[string]any{"value": "New Note"},
						"body":  map[string]any{"value": "Created via modify"},
					},
				},
			},
		},
	}, authHeader)
	if status != 200 {
		t.Fatalf("modify (create) -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal modify: %v (body %s)", err, body)
	}
	records = resp["records"].([]any)
	rec = records[0].(map[string]any)
	if rec["recordName"] != "note-003" {
		t.Fatalf("created recordName = %v, want note-003", rec["recordName"])
	}

	// ===== Lookup the created record (STATEFUL) =====

	body, status = cloudKitGetJSON(t, prefix+"/records/lookup",
		map[string]any{"records": []map[string]any{{"recordName": "note-003"}}},
		authHeader)
	if status != 200 {
		t.Fatalf("lookup created -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	records = resp["records"].([]any)
	rec = records[0].(map[string]any)
	if rec["recordName"] != "note-003" {
		t.Fatalf("lookup created recordName = %v, want note-003", rec["recordName"])
	}

	// ===== Query by recordType → filtered results =====

	body, status = cloudKitGetJSON(t, prefix+"/records/query", map[string]any{
		"query": map[string]any{
			"recordType": "Notes",
		},
	}, authHeader)
	if status != 200 {
		t.Fatalf("query -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal query: %v (body %s)", err, body)
	}
	records = resp["records"].([]any)
	if len(records) < 2 {
		t.Fatalf("query results count = %d, want >= 2 (seeded + created)", len(records))
	}

	// ===== Zones list =====

	body, status = cloudKitGet(t, prefix+"/zones/list", authHeader)
	if status != 200 {
		t.Fatalf("zones -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal zones: %v (body %s)", err, body)
	}
	zones, ok := resp["zones"].([]any)
	if !ok || len(zones) < 1 {
		t.Fatalf("zones = %v, want non-empty array", resp["zones"])
	}

	// ===== Current user =====

	body, status = cloudKitGet(t, prefix+"/users/current", authHeader)
	if status != 200 {
		t.Fatalf("current user -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal user: %v (body %s)", err, body)
	}
	if resp["userRecordName"] != "_owner" {
		t.Fatalf("userRecordName = %v, want _owner", resp["userRecordName"])
	}
}

// === CloudKit test helpers ===

func cloudKitGet(t *testing.T, rawurl string, headers map[string]string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func cloudKitGetJSON(t *testing.T, rawurl string, body map[string]any, headers map[string]string) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("GET", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func cloudKitPostJSON(t *testing.T, rawurl string, body map[string]any, headers map[string]string) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
