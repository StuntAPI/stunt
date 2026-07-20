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

// TestBrazeStyleAdapter exercises the braze-style adapter:
//
//   - 401 without auth
//   - users/track → attributes processed
//   - messages/send → success with dispatch_id
//   - segments/list → segments
//   - campaigns/trigger/send
func TestBrazeStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "braze-style")
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
			"braze": {Adapter: absAdapterDir},
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

	base := addrs["braze"]
	token := "test-app-group-api-key"

	// ===== 401 without auth =====

	_, status := brazePost(t, base+"/users/track", "", map[string]any{
		"attributes": []map[string]any{{"external_id": "user1"}},
	})
	if status != 401 {
		t.Fatalf("track without auth -> status %d, want 401", status)
	}

	// ===== users/track → attributes processed =====

	body, status := brazePost(t, base+"/users/track", token, map[string]any{
		"attributes": []map[string]any{
			{"external_id": "user001", "first_name": "Alice", "email": "alice@example.com"},
			{"external_id": "user002", "first_name": "Bob", "email": "bob@example.com"},
		},
		"events": []map[string]any{
			{"external_id": "user001", "name": "purchase", "time": "2024-01-01T00:00:00Z"},
		},
	})
	if status != 200 {
		t.Fatalf("track -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if resp["message"] != "success" {
		t.Fatalf("message = %v, want success", resp["message"])
	}
	if resp["attributes_processed"] != float64(2) {
		t.Fatalf("attributes_processed = %v, want 2", resp["attributes_processed"])
	}

	// ===== messages/send → success =====

	body, status = brazePost(t, base+"/messages/send", token, map[string]any{
		"messages": map[string]any{
			"email": map[string]any{
				"app_id":  "app-001",
				"subject": "Hello from Braze!",
				"from":    "noreply@example.com",
				"body":    "This is a test message.",
			},
		},
		"external_user_ids": []string{"user001", "user002"},
	})
	if status != 200 {
		t.Fatalf("send -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal send: %v (body %s)", err, body)
	}
	if resp["message"] != "success" {
		t.Fatalf("message = %v, want success", resp["message"])
	}
	if _, ok := resp["dispatch_id"].(string); !ok {
		t.Fatalf("dispatch_id = %v, want string", resp["dispatch_id"])
	}
	if resp["recipients"] != float64(2) {
		t.Fatalf("recipients = %v, want 2", resp["recipients"])
	}

	// ===== segments/list → segments =====

	body, status = brazeGet(t, base+"/segments/list", token)
	if status != 200 {
		t.Fatalf("segments -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal segments: %v (body %s)", err, body)
	}
	segments, ok := resp["segments"].([]any)
	if !ok || len(segments) < 1 {
		t.Fatalf("segments = %v, want non-empty array", resp["segments"])
	}
	seg := segments[0].(map[string]any)
	if _, ok := seg["id"].(string); !ok {
		t.Fatalf("segment id = %v, want string", seg["id"])
	}
	if _, ok := seg["name"].(string); !ok {
		t.Fatalf("segment name = %v, want string", seg["name"])
	}

	// ===== campaigns/trigger/send =====

	body, status = brazePost(t, base+"/campaigns/trigger/send", token, map[string]any{
		"campaign_id":       "cmp001",
		"external_user_ids": []string{"user001"},
	})
	if status != 200 {
		t.Fatalf("trigger -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal trigger: %v (body %s)", err, body)
	}
	if resp["message"] != "success" {
		t.Fatalf("message = %v, want success", resp["message"])
	}

	// ===== x-authorization header also works =====

	req, err := http.NewRequest("GET", base+"/segments/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-authorization", token)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("segments with x-authorization -> status %d, want 200", resp2.StatusCode)
	}
}

// === Braze test helpers ===

func brazeGet(t *testing.T, rawurl, token string) (string, int) {
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

func brazePost(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
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
