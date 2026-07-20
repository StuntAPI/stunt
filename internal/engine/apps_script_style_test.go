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

// TestAppsScriptStyleAdapter exercises the apps-script-style adapter:
//
//   - 401 without auth
//   - List projects → default project
//   - Get content → SERVER_JS file with source
//   - Update content → STATEFUL
//   - Run function → result
//   - Create deployment
func TestAppsScriptStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "apps-script-style")
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
			"script": {Adapter: absAdapterDir},
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

	base := addrs["script"]
	token := "mock-oauth2-token"

	// ===== 401 without auth =====

	_, status := appsScriptGet(t, base+"/v1/projects", "")
	if status != 401 {
		t.Fatalf("projects without auth -> status %d, want 401", status)
	}

	// ===== List projects → default project =====

	body, status := appsScriptGet(t, base+"/v1/projects", token)
	if status != 200 {
		t.Fatalf("projects -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	projects, ok := resp["projects"].([]any)
	if !ok || len(projects) < 1 {
		t.Fatalf("projects = %v, want non-empty array", resp["projects"])
	}
	first := projects[0].(map[string]any)
	scriptID, ok := first["scriptId"].(string)
	if !ok || scriptID == "" {
		t.Fatalf("scriptId = %v, want non-empty string", first["scriptId"])
	}
	if _, ok := first["title"].(string); !ok {
		t.Fatalf("title = %v, want string", first["title"])
	}

	// ===== Get content → SERVER_JS file =====

	body, status = appsScriptGet(t, base+"/v1/projects/"+scriptID+"/content", token)
	if status != 200 {
		t.Fatalf("content -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal content: %v (body %s)", err, body)
	}
	files, ok := resp["files"].([]any)
	if !ok || len(files) < 1 {
		t.Fatalf("files = %v, want non-empty array", resp["files"])
	}
	file := files[0].(map[string]any)
	if file["type"] != "SERVER_JS" {
		t.Fatalf("file type = %v, want SERVER_JS", file["type"])
	}
	if _, ok := file["source"].(string); !ok {
		t.Fatalf("source = %v, want string", file["source"])
	}

	// ===== Update content (STATEFUL) =====

	newFiles := []map[string]any{
		{"name": "Code", "type": "SERVER_JS", "source": "function newFunc() { return 42; }"},
		{"name": "Index", "type": "HTML", "source": "<html><body>Hello</body></html>"},
	}
	body, status = appsScriptPost(t, base+"/v1/projects/"+scriptID+"/content", token, map[string]any{
		"files": newFiles,
	})
	if status != 200 {
		t.Fatalf("update content -> status %d, want 200; body %s", status, body)
	}

	// Verify content persists.
	body, status = appsScriptGet(t, base+"/v1/projects/"+scriptID+"/content", token)
	if status != 200 {
		t.Fatalf("content after update -> status %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	files = resp["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("files count after update = %d, want 2", len(files))
	}

	// ===== Run function → result =====

	body, status = appsScriptPost(t, base+"/v1/projects/"+scriptID+"/scripts/helloWorld/run", token, map[string]any{
		"function":   "helloWorld",
		"devMode":    true,
		"parameters": []string{"Alice"},
	})
	if status != 200 {
		t.Fatalf("run -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal run: %v (body %s)", err, body)
	}
	if resp["done"] != true {
		t.Fatalf("done = %v, want true", resp["done"])
	}
	response, ok := resp["response"].(map[string]any)
	if !ok {
		t.Fatalf("response = %v, want object", resp["response"])
	}
	result, ok := response["result"].(string)
	if !ok || result == "" {
		t.Fatalf("result = %v, want non-empty string", response["result"])
	}
	if result != "Hello, Alice!" {
		t.Fatalf("result = %v, want 'Hello, Alice!'", result)
	}

	// ===== Create deployment =====

	body, status = appsScriptPost(t, base+"/v1/projects/"+scriptID+"/deployments", token, map[string]any{
		"versionNumber": 1,
		"deploymentConfig": map[string]any{
			"description": "Test deployment",
		},
	})
	if status != 200 {
		t.Fatalf("deployment -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal deployment: %v (body %s)", err, body)
	}
	if _, ok := resp["deploymentId"].(string); !ok {
		t.Fatalf("deploymentId = %v, want string", resp["deploymentId"])
	}
}

// === Apps Script test helpers ===

func appsScriptGet(t *testing.T, rawurl, token string) (string, int) {
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

func appsScriptPost(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
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
