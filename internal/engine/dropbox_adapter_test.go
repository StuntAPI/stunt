package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestDropboxStyleAdapter exercises the broader Dropbox-style reference
// adapter end-to-end: file upload → download (content) → list_folder (sees
// it) → get_metadata → create_folder → list (sees folder) → delete → 409
// after delete. Also covers get_temporary_link and get_current_account.
// State persists across requests within the session.
func TestDropboxStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "dropbox-style")

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"dropbox": {Adapter: adapterDir},
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

	base := addrs["dropbox"]

	// ===== Upload a file =====

	// POST /2/files/upload → 200, id with "id_" prefix
	body, status := postJSON(t, base+"/2/files/upload", map[string]any{
		"path":    "/test-document.txt",
		"content": "Hello, Dropbox-style world!",
	})
	if status != 200 {
		t.Fatalf("POST upload -> status %d, want 200; body %s", status, body)
	}
	var file map[string]any
	if err := json.Unmarshal([]byte(body), &file); err != nil {
		t.Fatalf("unmarshal file: %v (body %s)", err, body)
	}
	fileID, ok := file["id"].(string)
	if !ok || !strings.HasPrefix(fileID, "id_") {
		t.Fatalf("file id = %v, want id_* prefix", file["id"])
	}
	if file[".tag"] != "file" {
		t.Fatalf("file .tag = %v, want file", file[".tag"])
	}
	if file["name"] != "test-document.txt" {
		t.Fatalf("file name = %v, want test-document.txt", file["name"])
	}
	if file["path_display"] != "/test-document.txt" {
		t.Fatalf("file path_display = %v, want /test-document.txt", file["path_display"])
	}
	if file["size"] != float64(len("Hello, Dropbox-style world!")) {
		t.Fatalf("file size = %v, want %d", file["size"], len("Hello, Dropbox-style world!"))
	}

	// ===== Download content =====

	// POST /2/files/download {path} → 200, raw content matches
	body, status = postJSON(t, base+"/2/files/download", map[string]any{
		"path": "/test-document.txt",
	})
	if status != 200 {
		t.Fatalf("POST download -> status %d, want 200; body %s", status, body)
	}
	if body != "Hello, Dropbox-style world!" {
		t.Fatalf("downloaded content = %q, want %q", body, "Hello, Dropbox-style world!")
	}

	// POST /2/files/download {id} → 200, same content
	body, status = postJSON(t, base+"/2/files/download", map[string]any{
		"id": fileID,
	})
	if status != 200 {
		t.Fatalf("POST download by id -> status %d, want 200; body %s", status, body)
	}
	if body != "Hello, Dropbox-style world!" {
		t.Fatalf("downloaded content (by id) = %q, want %q", body, "Hello, Dropbox-style world!")
	}

	// POST /2/files/download {nonexistent} → 409
	_, status = postJSON(t, base+"/2/files/download", map[string]any{
		"path": "/no-such-file.txt",
	})
	if status != 409 {
		t.Fatalf("POST download unknown -> status %d, want 409", status)
	}

	// ===== Get metadata =====

	// POST /2/files/get_metadata {path} → 200, metadata persisted
	body, status = postJSON(t, base+"/2/files/get_metadata", map[string]any{
		"path": "/test-document.txt",
	})
	if status != 200 {
		t.Fatalf("POST get_metadata -> status %d, want 200; body %s", status, body)
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

	// POST /2/files/get_metadata {nonexistent} → 409
	_, status = postJSON(t, base+"/2/files/get_metadata", map[string]any{
		"path": "/does-not-exist",
	})
	if status != 409 {
		t.Fatalf("POST get_metadata unknown -> status %d, want 409", status)
	}

	// ===== List folder =====

	// POST /2/files/list_folder {""} → 200, list containing uploaded file + seed
	body, status = postJSON(t, base+"/2/files/list_folder", map[string]any{
		"path": "",
	})
	if status != 200 {
		t.Fatalf("POST list_folder -> status %d, want 200; body %s", status, body)
	}
	var folderList map[string]any
	if err := json.Unmarshal([]byte(body), &folderList); err != nil {
		t.Fatalf("unmarshal folder list: %v (body %s)", err, body)
	}
	entries, ok := folderList["entries"].([]any)
	if !ok || len(entries) < 2 { // 1 seed folder + 1 uploaded file
		t.Fatalf("entries list has %d items, want >= 2", len(entries))
	}
	if folderList["has_more"] != false {
		t.Fatalf("has_more = %v, want false", folderList["has_more"])
	}

	// ===== Create folder =====

	// POST /2/files/create_folder {path} → 200, .tag:"folder"
	body, status = postJSON(t, base+"/2/files/create_folder", map[string]any{
		"path": "/My Folder",
	})
	if status != 200 {
		t.Fatalf("POST create_folder -> status %d, want 200; body %s", status, body)
	}
	var folder map[string]any
	if err := json.Unmarshal([]byte(body), &folder); err != nil {
		t.Fatalf("unmarshal folder: %v (body %s)", err, body)
	}
	folderID, ok := folder["id"].(string)
	if !ok || !strings.HasPrefix(folderID, "id_") {
		t.Fatalf("folder id = %v, want id_* prefix", folder["id"])
	}
	if folder[".tag"] != "folder" {
		t.Fatalf("folder .tag = %v, want folder", folder[".tag"])
	}
	if folder["name"] != "My Folder" {
		t.Fatalf("folder name = %v, want My Folder", folder["name"])
	}
	if _, hasSize := folder["size"]; hasSize {
		t.Fatalf("folder should not have a size field")
	}

	// POST /2/files/create_folder {existing} → 409 conflict
	_, status = postJSON(t, base+"/2/files/create_folder", map[string]any{
		"path": "/My Folder",
	})
	if status != 409 {
		t.Fatalf("POST create_folder duplicate -> status %d, want 409", status)
	}

	// ===== List (should see folder) =====

	// POST /2/files/list_folder {""} → 200, now 3 entries
	body, status = postJSON(t, base+"/2/files/list_folder", map[string]any{
		"path": "",
	})
	if err := json.Unmarshal([]byte(body), &folderList); err != nil {
		t.Fatalf("unmarshal folder list after folder creation: %v", err)
	}
	entries = folderList["entries"].([]any)
	if len(entries) < 3 { // 1 seed + 1 file + 1 folder
		t.Fatalf("entries list has %d items, want >= 3", len(entries))
	}
	// Verify the folder appears in the listing
	foundFolder := false
	for _, e := range entries {
		if em, ok := e.(map[string]any); ok && em["id"] == folderID {
			foundFolder = true
		}
	}
	if !foundFolder {
		t.Fatalf("folder %s not found in list_folder entries", folderID)
	}

	// ===== Download folder → 409 disallowed =====

	_, status = postJSON(t, base+"/2/files/download", map[string]any{
		"path": "/My Folder",
	})
	if status != 409 {
		t.Fatalf("POST download folder -> status %d, want 409", status)
	}

	// ===== Delete file =====

	// POST /2/files/delete {path} → 200
	body, status = postJSON(t, base+"/2/files/delete", map[string]any{
		"path": "/test-document.txt",
	})
	if status != 200 {
		t.Fatalf("POST delete -> status %d, want 200; body %s", status, body)
	}
	var deleted map[string]any
	if err := json.Unmarshal([]byte(body), &deleted); err != nil {
		t.Fatalf("unmarshal deleted: %v (body %s)", err, body)
	}
	if deleted["id"] != fileID {
		t.Fatalf("deleted id = %v, want %s", deleted["id"], fileID)
	}

	// POST /2/files/get_metadata {deleted} → 409
	_, status = postJSON(t, base+"/2/files/get_metadata", map[string]any{
		"path": "/test-document.txt",
	})
	if status != 409 {
		t.Fatalf("POST get_metadata after delete -> status %d, want 409", status)
	}

	// POST /2/files/delete {nonexistent} → 409
	_, status = postJSON(t, base+"/2/files/delete", map[string]any{
		"path": "/never-existed",
	})
	if status != 409 {
		t.Fatalf("POST delete unknown -> status %d, want 409", status)
	}

	// ===== Get temporary link =====

	// First upload a file to get a link for
	body, status = postJSON(t, base+"/2/files/upload", map[string]any{
		"path":    "/My Folder/temp.txt",
		"content": "temp-link-content",
	})
	if status != 200 {
		t.Fatalf("POST upload for temp link -> status %d, want 200; body %s", status, body)
	}
	body, status = postJSON(t, base+"/2/files/get_temporary_link", map[string]any{
		"path": "/My Folder/temp.txt",
	})
	if status != 200 {
		t.Fatalf("POST get_temporary_link -> status %d, want 200; body %s", status, body)
	}
	var tempLink map[string]any
	if err := json.Unmarshal([]byte(body), &tempLink); err != nil {
		t.Fatalf("unmarshal temp link: %v (body %s)", err, body)
	}
	if _, ok := tempLink["link"].(string); !ok {
		t.Fatalf("temp link response missing 'link' string field: %v", tempLink)
	}
	if _, ok := tempLink["metadata"].(map[string]any); !ok {
		t.Fatalf("temp link response missing 'metadata' object: %v", tempLink)
	}

	// ===== Users: get_current_account =====

	// POST /2/users/get_current_account → 200, synthetic account info
	body, status = postJSON(t, base+"/2/users/get_current_account", map[string]any{})
	if status != 200 {
		t.Fatalf("POST get_current_account -> status %d, want 200; body %s", status, body)
	}
	var account map[string]any
	if err := json.Unmarshal([]byte(body), &account); err != nil {
		t.Fatalf("unmarshal account: %v (body %s)", err, body)
	}
	if _, ok := account["account_id"].(string); !ok {
		t.Fatalf("account.account_id = %v, want a string", account["account_id"])
	}
	if _, ok := account["name"].(map[string]any); !ok {
		t.Fatalf("account.name = %v, want a dict", account["name"])
	}

	// ===== Catch-all 404 =====

	resp, err := http.Get(base + "/2/no-such-resource")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("GET unmatched route -> status %d, want 404", resp.StatusCode)
	}
}

