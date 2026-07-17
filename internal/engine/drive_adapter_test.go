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

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// patchJSON sends a PATCH request with a JSON body and returns the body + status.
func patchJSON(t *testing.T, url string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PATCH", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// TestDriveStyleAdapter exercises the broader Google-Drive-style reference
// adapter end-to-end: file upload → get metadata → download content → list →
// patch (rename) → delete → 404 after delete; folder creation; about/quota.
// State persists across requests within the session.
func TestDriveStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "drive-style")

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"drive": {Adapter: adapterDir},
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

	base := addrs["drive"]

	// ===== Upload a file =====

	// POST /upload/drive/v3/files → 201, id with file_ prefix
	body, status := postJSON(t, base+"/upload/drive/v3/files", map[string]any{
		"name":    "test-document.txt",
		"content": "Hello, Drive-style world!",
	})
	if status != 201 {
		t.Fatalf("POST upload -> status %d, want 201; body %s", status, body)
	}
	var file map[string]any
	if err := json.Unmarshal([]byte(body), &file); err != nil {
		t.Fatalf("unmarshal file: %v (body %s)", err, body)
	}
	fileID, ok := file["id"].(string)
	if !ok || !strings.HasPrefix(fileID, "file_") {
		t.Fatalf("file id = %v, want file_* prefix", file["id"])
	}
	if file["name"] != "test-document.txt" {
		t.Fatalf("file name = %v, want test-document.txt", file["name"])
	}
	if file["size"] != float64(len("Hello, Drive-style world!")) {
		t.Fatalf("file size = %v, want %d", file["size"], len("Hello, Drive-style world!"))
	}

	// ===== Get metadata =====

	// GET /drive/v3/files/{id} → 200, metadata persisted
	body, status = get2(t, base+"/drive/v3/files/"+fileID)
	if status != 200 {
		t.Fatalf("GET metadata -> status %d, want 200; body %s", status, body)
	}
	var retrieved map[string]any
	if err := json.Unmarshal([]byte(body), &retrieved); err != nil {
		t.Fatalf("unmarshal retrieved: %v (body %s)", err, body)
	}
	if retrieved["id"] != fileID {
		t.Fatalf("retrieved id = %v, want %s", retrieved["id"], fileID)
	}
	if retrieved["name"] != "test-document.txt" {
		t.Fatalf("retrieved name = %v, want test-document.txt", retrieved["name"])
	}

	// GET /drive/v3/files/{nonexistent} → 404
	_, status = get2(t, base+"/drive/v3/files/does-not-exist")
	if status != 404 {
		t.Fatalf("GET unknown file -> status %d, want 404", status)
	}

	// ===== Download content =====

	// GET /drive/v3/files/{id}?alt=media → 200, raw content matches
	body, status = get2(t, base+"/drive/v3/files/"+fileID+"?alt=media")
	if status != 200 {
		t.Fatalf("GET alt=media -> status %d, want 200; body %s", status, body)
	}
	if body != "Hello, Drive-style world!" {
		t.Fatalf("downloaded content = %q, want %q", body, "Hello, Drive-style world!")
	}

	// ===== List files =====

	// GET /drive/v3/files → 200, list containing our file + seed folder
	body, status = get2(t, base+"/drive/v3/files")
	if status != 200 {
		t.Fatalf("GET list -> status %d, want 200; body %s", status, body)
	}
	var fileList map[string]any
	if err := json.Unmarshal([]byte(body), &fileList); err != nil {
		t.Fatalf("unmarshal file list: %v (body %s)", err, body)
	}
	files, ok := fileList["files"].([]any)
	if !ok || len(files) < 2 { // 1 seed folder + 1 uploaded
		t.Fatalf("file list has %d items, want >= 2", len(files))
	}

	// ===== Patch (rename) =====

	// PATCH /drive/v3/files/{id} → 200, name updated
	body, status = patchJSON(t, base+"/drive/v3/files/"+fileID, map[string]any{
		"name": "renamed-document.txt",
	})
	if status != 200 {
		t.Fatalf("PATCH rename -> status %d, want 200; body %s", status, body)
	}
	var patched map[string]any
	if err := json.Unmarshal([]byte(body), &patched); err != nil {
		t.Fatalf("unmarshal patched: %v (body %s)", err, body)
	}
	if patched["name"] != "renamed-document.txt" {
		t.Fatalf("patched name = %v, want renamed-document.txt", patched["name"])
	}
	// ID should be preserved
	if patched["id"] != fileID {
		t.Fatalf("patched id = %v, want %s (should be preserved)", patched["id"], fileID)
	}

	// PATCH unknown → 404
	_, status = patchJSON(t, base+"/drive/v3/files/no-such-file", map[string]any{"name": "x"})
	if status != 404 {
		t.Fatalf("PATCH unknown -> status %d, want 404", status)
	}

	// ===== Trash via PATCH =====

	// PATCH /drive/v3/files/{id} with trashed=true → 200
	body, status = patchJSON(t, base+"/drive/v3/files/"+fileID, map[string]any{
		"trashed": true,
	})
	if status != 200 {
		t.Fatalf("PATCH trash -> status %d, want 200; body %s", status, body)
	}
	// Trashed file should not appear in default list
	body, status = get2(t, base+"/drive/v3/files")
	if err := json.Unmarshal([]byte(body), &fileList); err != nil {
		t.Fatalf("unmarshal file list after trash: %v", err)
	}
	files = fileList["files"].([]any)
	for _, f := range files {
		if fm, ok := f.(map[string]any); ok && fm["id"] == fileID {
			t.Fatalf("trashed file %s should not appear in list", fileID)
		}
	}

	// ===== Delete =====

	// DELETE /drive/v3/files/{id} → 204
	_, status = deleteReq(t, base+"/drive/v3/files/"+fileID)
	if status != 204 {
		t.Fatalf("DELETE file -> status %d, want 204", status)
	}

	// GET after delete → 404
	_, status = get2(t, base+"/drive/v3/files/"+fileID)
	if status != 404 {
		t.Fatalf("GET deleted file -> status %d, want 404", status)
	}

	// DELETE unknown → 404
	_, status = deleteReq(t, base+"/drive/v3/files/no-such-file")
	if status != 404 {
		t.Fatalf("DELETE unknown -> status %d, want 404", status)
	}

	// ===== Create a folder =====

	// POST /upload/drive/v3/files with folder mimeType → 201
	body, status = postJSON(t, base+"/upload/drive/v3/files", map[string]any{
		"name":     "My Folder",
		"mimeType": "application/vnd.google-apps.folder",
	})
	if status != 201 {
		t.Fatalf("POST create folder -> status %d, want 201; body %s", status, body)
	}
	var folder map[string]any
	if err := json.Unmarshal([]byte(body), &folder); err != nil {
		t.Fatalf("unmarshal folder: %v (body %s)", err, body)
	}
	folderID, ok := folder["id"].(string)
	if !ok || !strings.HasPrefix(folderID, "file_") {
		t.Fatalf("folder id = %v, want file_* prefix", folder["id"])
	}
	if folder["mimeType"] != "application/vnd.google-apps.folder" {
		t.Fatalf("folder mimeType = %v, want application/vnd.google-apps.folder", folder["mimeType"])
	}
	if folder["size"] != float64(0) {
		t.Fatalf("folder size = %v, want 0", folder["size"])
	}

	// GET folder metadata → 200
	body, status = get2(t, base+"/drive/v3/files/"+folderID)
	if status != 200 {
		t.Fatalf("GET folder metadata -> status %d, want 200", status)
	}

	// GET folder ?alt=media → 400 (cannot download folder)
	_, status = get2(t, base+"/drive/v3/files/"+folderID+"?alt=media")
	if status != 400 {
		t.Fatalf("GET folder alt=media -> status %d, want 400", status)
	}

	// ===== About / quota =====

	// GET /drive/v3/about → 200, synthetic storageQuota + user
	body, status = get2(t, base+"/drive/v3/about")
	if status != 200 {
		t.Fatalf("GET about -> status %d, want 200; body %s", status, body)
	}
	var about map[string]any
	if err := json.Unmarshal([]byte(body), &about); err != nil {
		t.Fatalf("unmarshal about: %v (body %s)", err, body)
	}
	if _, ok := about["storageQuota"].(map[string]any); !ok {
		t.Fatalf("about.storageQuota = %v, want a dict", about["storageQuota"])
	}
	if _, ok := about["user"].(map[string]any); !ok {
		t.Fatalf("about.user = %v, want a dict", about["user"])
	}

	// ===== Changes =====

	// GET /drive/v3/changes → 200, minimal change list
	body, status = get2(t, base+"/drive/v3/changes")
	if status != 200 {
		t.Fatalf("GET changes -> status %d, want 200; body %s", status, body)
	}
	var changes map[string]any
	if err := json.Unmarshal([]byte(body), &changes); err != nil {
		t.Fatalf("unmarshal changes: %v (body %s)", err, body)
	}
	if _, ok := changes["changes"].([]any); !ok {
		t.Fatalf("changes.changes = %v, want a list", changes["changes"])
	}

	// ===== Catch-all 404 =====

	_, status = get2(t, base+"/drive/v3/no-such-resource")
	if status != 404 {
		t.Fatalf("GET unmatched route -> status %d, want 404", status)
	}
}
