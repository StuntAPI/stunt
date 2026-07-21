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

// TestGTasksStyleAdapter exercises the gtasks-style adapter:
//
//   - 401 without auth
//   - List task lists → default list
//   - Create task → task with id
//   - Get task → STATEFUL round-trip
//   - Move task → position updated
//   - Update task → status changes
func TestGTasksStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "gtasks-style")
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
			"tasks": {Adapter: absAdapterDir},
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

	base := addrs["tasks"]
	token := "mock-oauth2-token"

	// ===== 401 without auth =====

	_, status := gtasksGet(t, base+"/tasks/v1/lists", "")
	if status != 401 {
		t.Fatalf("list without auth -> status %d, want 401", status)
	}

	// ===== List task lists → default list =====

	body, status := gtasksGet(t, base+"/tasks/v1/lists", token)
	if status != 200 {
		t.Fatalf("lists -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	items, ok := resp["items"].([]any)
	if !ok || len(items) < 1 {
		t.Fatalf("items = %v, want non-empty array", resp["items"])
	}
	first := items[0].(map[string]any)
	listID, ok := first["id"].(string)
	if !ok || listID == "" {
		t.Fatalf("list id = %v, want string", first["id"])
	}
	if _, ok := first["title"].(string); !ok {
		t.Fatalf("list title = %v, want string", first["title"])
	}

	// ===== Create task =====

	body, status = gtasksPost(t, base+"/tasks/v1/lists/"+listID+"/tasks", token, map[string]any{
		"title": "Buy groceries",
		"notes": "Milk and bread",
		"due":   "2024-01-15T00:00:00.000Z",
	})
	if status != 200 {
		t.Fatalf("create task -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	taskID, ok := resp["id"].(string)
	if !ok || taskID == "" {
		t.Fatalf("task id = %v, want string", resp["id"])
	}
	if resp["title"] != "Buy groceries" {
		t.Fatalf("task title = %v, want 'Buy groceries'", resp["title"])
	}
	if resp["status"] != "needsAction" {
		t.Fatalf("task status = %v, want needsAction", resp["status"])
	}

	// ===== Get task → STATEFUL round-trip =====

	body, status = gtasksGet(t, base+"/tasks/v1/lists/"+listID+"/tasks/"+taskID, token)
	if status != 200 {
		t.Fatalf("get task -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	if resp["id"] != taskID {
		t.Fatalf("get task id = %v, want %v", resp["id"], taskID)
	}
	if resp["title"] != "Buy groceries" {
		t.Fatalf("get task title = %v, want 'Buy groceries'", resp["title"])
	}

	// ===== Move task → position changes =====

	body, status = gtasksPost(t, base+"/tasks/v1/lists/"+listID+"/tasks/"+taskID+"/move", token, map[string]any{
		"parent":   None(),
		"previous": "",
	})
	if status != 200 {
		t.Fatalf("move task -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal move: %v (body %s)", err, body)
	}
	pos, ok := resp["position"].(string)
	if !ok || pos == "" {
		t.Fatalf("task position after move = %v, want non-empty string", resp["position"])
	}

	// ===== Update task → status changes =====

	body, status = gtasksPatch(t, base+"/tasks/v1/lists/"+listID+"/tasks/"+taskID, token, map[string]any{
		"status": "completed",
	})
	if status != 200 {
		t.Fatalf("update task -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal update: %v (body %s)", err, body)
	}
	if resp["status"] != "completed" {
		t.Fatalf("updated task status = %v, want completed", resp["status"])
	}

	// ===== List tasks → includes our task =====

	body, status = gtasksGet(t, base+"/tasks/v1/lists/"+listID+"/tasks", token)
	if status != 200 {
		t.Fatalf("list tasks -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal list tasks: %v (body %s)", err, body)
	}
	items = resp["items"].([]any)
	if len(items) < 1 {
		t.Fatalf("list tasks count = %d, want >= 1", len(items))
	}

	// ===== Delete task → 204 =====

	body, status = gtasksDelete(t, base+"/tasks/v1/lists/"+listID+"/tasks/"+taskID, token)
	if status != 204 {
		t.Fatalf("delete task -> status %d, want 204; body %s", status, body)
	}
}

// None returns a JSON null value.
func None() any {
	return nil
}

// === GTasks test helpers ===

func gtasksGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
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

func gtasksPost(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
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

func gtasksPatch(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PATCH", rawurl, bytes.NewReader(data))
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

func gtasksDelete(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
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
