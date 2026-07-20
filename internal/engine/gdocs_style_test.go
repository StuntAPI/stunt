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

// TestGDocsStyleAdapter exercises the gdocs-style adapter:
//
//   - 401 without auth
//   - Create document → documentId, title
//   - GET document → structural content model
//   - batchUpdate insertText → replies
//   - GET document → inserted text is now visible (STATEFUL)
//   - Revisions
func TestGDocsStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "gdocs-style")
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
			"docs": {Adapter: absAdapterDir},
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

	base := addrs["docs"]
	token := "mock-oauth2-token"

	// ===== 401 without auth =====

	_, status := gdocsGet(t, base+"/v1/documents/test-doc-id", "")
	if status != 401 {
		t.Fatalf("get without auth -> status %d, want 401", status)
	}

	// ===== Create document =====

	body, status := gdocsPost(t, base+"/v1/documents", token, map[string]any{
		"title": "My Test Document",
	})
	if status != 200 {
		t.Fatalf("create -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	docID, ok := resp["documentId"].(string)
	if !ok || docID == "" {
		t.Fatalf("documentId = %v, want non-empty string", resp["documentId"])
	}
	if resp["title"] != "My Test Document" {
		t.Fatalf("title = %v, want 'My Test Document'", resp["title"])
	}

	// Verify body.content exists.
	docBody, ok := resp["body"].(map[string]any)
	if !ok {
		t.Fatalf("body = %v, want object", resp["body"])
	}
	content, ok := docBody["content"].([]any)
	if !ok || len(content) < 1 {
		t.Fatalf("content = %v, want non-empty array", docBody["content"])
	}

	// ===== batchUpdate insertText =====

	body, status = gdocsPost(t, base+"/v1/documents/"+docID+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"insertText": map[string]any{
					"location": map[string]any{"index": 1},
					"text":     "Hello, World!",
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("batchUpdate -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal batchUpdate: %v (body %s)", err, body)
	}
	if resp["documentId"] != docID {
		t.Fatalf("batchUpdate documentId = %v, want %v", resp["documentId"], docID)
	}
	replies, ok := resp["replies"].([]any)
	if !ok || len(replies) != 1 {
		t.Fatalf("replies = %v, want array of 1", resp["replies"])
	}

	// ===== GET document → inserted text is now visible (STATEFUL) =====

	body, status = gdocsGet(t, base+"/v1/documents/"+docID, token)
	if status != 200 {
		t.Fatalf("get after update -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	docBody = resp["body"].(map[string]any)
	content = docBody["content"].([]any)
	if len(content) < 1 {
		t.Fatalf("content after update = empty")
	}

	// Extract text from the content elements.
	foundText := false
	for _, item := range content {
		itemMap := item.(map[string]any)
		para, ok := itemMap["paragraph"].(map[string]any)
		if !ok {
			continue
		}
		elements, ok := para["elements"].([]any)
		if !ok {
			continue
		}
		for _, elem := range elements {
			elemMap := elem.(map[string]any)
			textRun, ok := elemMap["textRun"].(map[string]any)
			if !ok {
				continue
			}
			textContent, _ := textRun["content"].(string)
			if textContent != "" && (textContent == "Hello, World!\n" || textContent == "Hello, World!") {
				foundText = true
			}
		}
	}
	if !foundText {
		t.Fatalf("inserted text 'Hello, World!' not found in document content after batchUpdate")
	}

	// ===== Revisions =====

	body, status = gdocsGet(t, base+"/v1/documents/"+docID+"/revisions", token)
	if status != 200 {
		t.Fatalf("revisions -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal revisions: %v (body %s)", err, body)
	}
	revisions, ok := resp["revisions"].([]any)
	if !ok || len(revisions) < 1 {
		t.Fatalf("revisions = %v, want non-empty array", resp["revisions"])
	}
}

// === GDocs test helpers ===

func gdocsGet(t *testing.T, rawurl, token string) (string, int) {
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

func gdocsPost(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
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
