package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestAnaplanStyleAdapter exercises the anaplan-style adapter:
//
//   - Auth required: 401 without auth
//   - List workspaces → {meta, items}
//   - List models → items
//   - Run import → async task ID
//   - Get task status → COMPLETE
//   - List exports
func TestAnaplanStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "anaplan-style")
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
			"anaplan": {Adapter: absAdapterDir},
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

	base := addrs["anaplan"]
	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("test@example.com:password123"))
	wsID := "8a819c8645a0aa8e0005c715c7ad49b9"

	// ===== 401 without auth =====

	_, status := anaplanGet(t, base+"/2/0/workspaces", "")
	if status != 401 {
		t.Fatalf("workspaces without auth -> status %d, want 401", status)
	}

	// ===== List workspaces =====

	body, status := anaplanGet(t, base+"/2/0/workspaces", basicAuth)
	if status != 200 {
		t.Fatalf("workspaces -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	items, ok := resp["items"].([]any)
	if !ok || len(items) < 1 {
		t.Fatalf("items = %v, want non-empty array", resp["items"])
	}
	meta, ok := resp["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta = %v, want object", resp["meta"])
	}
	paging, ok := meta["paging"].(map[string]any)
	if !ok {
		t.Fatalf("paging = %v, want object", meta["paging"])
	}
	if _, ok := paging["totalSize"]; !ok {
		t.Fatalf("paging.totalSize missing")
	}

	// ===== List models =====

	body, status = anaplanGet(t, base+"/2/0/workspaces/"+wsID+"/models", basicAuth)
	if status != 200 {
		t.Fatalf("models -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal models: %v (body %s)", err, body)
	}
	items = resp["items"].([]any)
	if len(items) < 1 {
		t.Fatalf("models count = %d, want >= 1", len(items))
	}
	modelID := items[0].(map[string]any)["id"].(string)

	// ===== Run import → async task =====

	body, status = anaplanPost(t, base+"/2/0/workspaces/"+wsID+"/models/"+modelID+"/imports/imp001/tasks", basicAuth, nil)
	if status != 200 {
		t.Fatalf("run import -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal import: %v (body %s)", err, body)
	}
	taskObj, ok := resp["task"].(map[string]any)
	if !ok {
		t.Fatalf("task = %v, want object", resp["task"])
	}
	taskID, ok := taskObj["taskId"].(string)
	if !ok || taskID == "" {
		t.Fatalf("taskId = %v, want non-empty string", taskObj["taskId"])
	}
	if taskObj["taskState"] != "CREATED" {
		t.Fatalf("taskState = %v, want CREATED", taskObj["taskState"])
	}

	// ===== Get task status → COMPLETE =====

	body, status = anaplanGet(t, base+"/2/0/workspaces/"+wsID+"/models/"+modelID+"/tasks/"+taskID, basicAuth)
	if status != 200 {
		t.Fatalf("task status -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal task: %v (body %s)", err, body)
	}
	if resp["taskState"] != "COMPLETE" {
		t.Fatalf("taskState = %v, want COMPLETE", resp["taskState"])
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want object", resp["result"])
	}
	if result["successful"] != true {
		t.Fatalf("successful = %v, want true", result["successful"])
	}

	// ===== List exports =====

	body, status = anaplanGet(t, base+"/2/0/workspaces/"+wsID+"/models/"+modelID+"/exports", basicAuth)
	if status != 200 {
		t.Fatalf("exports -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal exports: %v (body %s)", err, body)
	}
	items = resp["items"].([]any)
	if len(items) < 1 {
		t.Fatalf("exports count = %d, want >= 1", len(items))
	}
}

// === Anaplan test helpers ===

func anaplanGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func anaplanPost(t *testing.T, rawurl, auth string, body map[string]any) (string, int) {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest("POST", rawurl, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
