package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestAzureDevOpsStyleAdapter exercises the azure-devops-style adapter:
//
//   - PAT auth required: 401 without auth
//   - List projects → {value, count}
//   - List git repos for a project
//   - Create work item (PATCH-style body) → retrievable by id
//   - Get work item by id → seeded item
//   - Iterations
func TestAzureDevOpsStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "azure-devops-style")
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
			"ado": {Adapter: absAdapterDir},
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

	base := addrs["ado"]

	// PAT as Basic auth (PAT:emptyPassword)
	patBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("testPAT:"))

	// ===== 401 without auth =====

	body, status := adoGet(t, base+"/myorg/_apis/projects", "")
	if status != 401 {
		t.Fatalf("projects without auth -> status %d, want 401; body %s", status, body)
	}

	// ===== List projects =====

	body, status = adoGet(t, base+"/myorg/_apis/projects", patBasic)
	if status != 200 {
		t.Fatalf("projects -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	value, ok := resp["value"].([]any)
	if !ok || len(value) < 1 {
		t.Fatalf("value = %v, want non-empty array", resp["value"])
	}
	first := value[0].(map[string]any)
	if _, ok := first["id"].(string); !ok {
		t.Fatalf("project id = %v, want string", first["id"])
	}
	if _, ok := first["name"].(string); !ok {
		t.Fatalf("project name = %v, want string", first["name"])
	}
	if resp["count"] != float64(len(value)) {
		t.Fatalf("count = %v, want %d", resp["count"], len(value))
	}

	// ===== List git repos =====

	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/git/repositories", patBasic)
	if status != 200 {
		t.Fatalf("repos -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal repos: %v (body %s)", err, body)
	}
	value = resp["value"].([]any)
	if len(value) < 1 {
		t.Fatalf("repos count = %d, want >= 1", len(value))
	}

	// ===== Create work item (PATCH-style body) =====

	createBody := []map[string]any{
		{"op": "add", "path": "/fields/System.Title", "value": "New task from API"},
		{"op": "add", "path": "/fields/System.Description", "value": "Created via stunt test"},
	}
	body, status = adoPostJSON(t, base+"/myorg/MyFirstProject/_apis/wit/workitems", patBasic, createBody)
	if status != 200 {
		t.Fatalf("create work item -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal created wi: %v (body %s)", err, body)
	}
	newID, ok := resp["id"].(float64)
	if !ok {
		t.Fatalf("created wi id = %v, want number", resp["id"])
	}
	fields, ok := resp["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %v, want object", resp["fields"])
	}
	if fields["System.Title"] != "New task from API" {
		t.Fatalf("System.Title = %v, want 'New task from API'", fields["System.Title"])
	}

	// ===== Get work item by id (seeded) =====

	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/wit/workitems/1", patBasic)
	if status != 200 {
		t.Fatalf("get work item -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal wi: %v (body %s)", err, body)
	}
	if resp["id"] != float64(1) {
		t.Fatalf("wi id = %v, want 1", resp["id"])
	}

	// ===== Get created work item by id (STATEFUL) =====

	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/wit/workitems/"+strconv.Itoa(int(newID)), patBasic)
	if status != 200 {
		t.Fatalf("get created wi -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if resp["id"] != newID {
		t.Fatalf("created wi id = %v, want %v", resp["id"], newID)
	}

	// ===== Iterations =====

	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/work/teamsettings/iterations", patBasic)
	if status != 200 {
		t.Fatalf("iterations -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal iterations: %v (body %s)", err, body)
	}
	value = resp["value"].([]any)
	if len(value) < 1 {
		t.Fatalf("iterations count = %d, want >= 1", len(value))
	}

	// ===== Bearer auth also works =====

	body, status = adoGet(t, base+"/myorg/_apis/projects", "Bearer testPAT")
	if status != 200 {
		t.Fatalf("projects with bearer -> status %d, want 200; body %s", status, body)
	}
}

// === Azure DevOps test helpers ===

func adoGet(t *testing.T, rawurl, auth string) (string, int) {
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

func adoPostJSON(t *testing.T, rawurl, auth string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json-patch+json")
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
