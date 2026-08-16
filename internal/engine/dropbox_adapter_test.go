package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestDropboxStyleAdapter exercises the broader Dropbox-style reference
// adapter end-to-end: file upload → download (content) → list_folder (sees
// it) → get_metadata → create_folder → list (sees folder) → delete → 409
// after delete. Also covers get_temporary_link and get_current_account.
// State persists across requests within the session.
func TestDropboxStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "dropbox-style")
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
			"dropbox": {Adapter: absAdapterDir},
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

	// ===== 401 with an unknown (bogus) bearer token =====
	// (No Authorization header is still accepted — see the adapter README.)

	badReq, err := http.NewRequest("POST", base+"/2/users/get_current_account", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	badReq.Header.Set("Content-Type", "application/json")
	badReq.Header.Set("Authorization", "Bearer bogus-dropbox-token")
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	badBody, _ := io.ReadAll(badResp.Body)
	badResp.Body.Close()
	if badResp.StatusCode != 401 {
		t.Fatalf("POST get_current_account with bogus token -> status %d, want 401; body %s", badResp.StatusCode, badBody)
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

// dropboxContentHash mirrors the adapter's content hash: Dropbox's own
// scheme of SHA-256 over the concatenated per-4MiB-block SHA-256 digests.
func dropboxContentHash(b []byte) string {
	const block = 4 << 20
	h := sha256.New()
	for i := 0; i == 0 || i < len(b); i += block {
		end := i + block
		if end > len(b) {
			end = len(b)
		}
		d := sha256.Sum256(b[i:end])
		h.Write(d[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestDropboxStyleContentHashAndClock verifies that uploads carry a
// content-derived hash in Dropbox's multi-block sha256 scheme (same bytes
// -> same hash, different bytes -> different hash — never a counter) and
// that server_modified is a live clock timestamp while client_modified
// echoes the client-declared value.
func TestDropboxStyleContentHashAndClock(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "dropbox-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
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

	start := time.Now().UTC()

	contentA := "clock-adoption content A"
	body, status := postJSON(t, base+"/2/files/upload", map[string]any{
		"path":            "/hash-a.txt",
		"content":         contentA,
		"client_modified": "2020-02-03T04:05:06Z",
	})
	if status != 200 {
		t.Fatalf("upload A -> %d, want 200; body %s", status, body)
	}
	var fileA map[string]any
	if err := json.Unmarshal([]byte(body), &fileA); err != nil {
		t.Fatalf("unmarshal file A: %v (body %s)", err, body)
	}
	if got := fileA["content_hash"]; got != dropboxContentHash([]byte(contentA)) {
		t.Fatalf("content_hash A = %v, want Go-computed %s", got, dropboxContentHash([]byte(contentA)))
	}
	if fileA["client_modified"] != "2020-02-03T04:05:06Z" {
		t.Fatalf("client_modified = %v, want the client-declared value echoed", fileA["client_modified"])
	}
	sm, _ := fileA["server_modified"].(string)
	ts, err := time.Parse(time.RFC3339, sm)
	if err != nil {
		t.Fatalf("server_modified %q is not RFC 3339: %v", sm, err)
	}
	if ts.Before(start.Add(-time.Minute)) || ts.After(time.Now().Add(time.Minute)) {
		t.Fatalf("server_modified %v not live (start %v)", ts, start)
	}

	// Different content -> the OTHER Go-computed hash (content-derived, and
	// distinct from A's — a counter would collide here).
	contentB := "clock-adoption content B (different bytes)"
	body, status = postJSON(t, base+"/2/files/upload", map[string]any{
		"path":    "/hash-b.txt",
		"content": contentB,
	})
	if status != 200 {
		t.Fatalf("upload B -> %d, want 200; body %s", status, body)
	}
	var fileB map[string]any
	if err := json.Unmarshal([]byte(body), &fileB); err != nil {
		t.Fatalf("unmarshal file B: %v (body %s)", err, body)
	}
	if got := fileB["content_hash"]; got != dropboxContentHash([]byte(contentB)) {
		t.Fatalf("content_hash B = %v, want Go-computed %s", got, dropboxContentHash([]byte(contentB)))
	}
	if fileB["content_hash"] == fileA["content_hash"] {
		t.Fatalf("distinct contents produced identical content_hash %v", fileA["content_hash"])
	}

	// Same content again -> same hash (deterministic in content alone).
	body, status = postJSON(t, base+"/2/files/upload", map[string]any{
		"path":    "/hash-a-again.txt",
		"content": contentA,
	})
	if status != 200 {
		t.Fatalf("upload A again -> %d, want 200; body %s", status, body)
	}
	var fileA2 map[string]any
	if err := json.Unmarshal([]byte(body), &fileA2); err != nil {
		t.Fatalf("unmarshal file A2: %v (body %s)", err, body)
	}
	if fileA2["content_hash"] != fileA["content_hash"] {
		t.Fatalf("same content, different hash: %v vs %v", fileA2["content_hash"], fileA["content_hash"])
	}
}

// TestDropboxStyleCascadeDelete proves files_v2 delete semantics: the delete
// is permanent (no restore) and a FOLDER delete cascades to every nested
// entry — no descendant survives under a deleted parent, and nothing is
// left dangling in the tree.
func TestDropboxStyleCascadeDelete(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "dropbox-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
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

	// Tree: /Projects (+ /Projects/Archive subfolder), two files inside,
	// plus a sibling file at the root that must survive.
	must200 := func(action string, payload map[string]any) map[string]any {
		t.Helper()
		b, st := postJSON(t, base+"/2/files/"+action, payload)
		if st != 200 {
			t.Fatalf("POST %s %v -> %d; body %s", action, payload, st, b)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(b), &out); err != nil {
			t.Fatalf("unmarshal %s resp: %v (%s)", action, err, b)
		}
		return out
	}

	proj := must200("create_folder", map[string]any{"path": "/Projects"})
	must200("create_folder", map[string]any{"path": "/Projects/Archive"})
	plan := must200("upload", map[string]any{"path": "/Projects/plan.txt", "content": "cascade plan"})
	old := must200("upload", map[string]any{"path": "/Projects/Archive/2019.txt", "content": "old stuff"})
	keep := must200("upload", map[string]any{"path": "/keep-me.txt", "content": "survivor"})
	_ = keep

	// ===== DELETE the folder: everything beneath it goes too =====
	del := must200("delete", map[string]any{"path": "/Projects"})
	if del["id"] != proj["id"] {
		t.Fatalf("delete response id = %v, want the folder %v", del["id"], proj["id"])
	}

	// Every cascaded entry is gone from every read path.
	for _, path := range []string{"/Projects", "/Projects/plan.txt", "/Projects/Archive", "/Projects/Archive/2019.txt"} {
		if _, st := postJSON(t, base+"/2/files/get_metadata", map[string]any{"path": path}); st != 409 {
			t.Fatalf("get_metadata %s after folder delete -> %d, want 409", path, st)
		}
	}
	if _, st := postJSON(t, base+"/2/files/download", map[string]any{"path": "/Projects/plan.txt"}); st != 409 {
		t.Fatalf("download cascaded file -> %d, want 409", st)
	}
	// By id too: metadata rows were removed, not just path-indexed.
	if _, st := postJSON(t, base+"/2/files/download", map[string]any{"id": plan["id"].(string)}); st != 409 {
		t.Fatalf("download cascaded file by id -> %d, want 409", st)
	}
	if _, st := postJSON(t, base+"/2/files/download", map[string]any{"id": old["id"].(string)}); st != 409 {
		t.Fatalf("download nested cascaded file by id -> %d, want 409", st)
	}

	// Root listing no longer contains the folder or its descendants, and the
	// sibling file survives.
	b, st := postJSON(t, base+"/2/files/list_folder", map[string]any{"path": ""})
	if st != 200 {
		t.Fatalf("list_folder -> %d; body %s", st, b)
	}
	var rootList map[string]any
	json.Unmarshal([]byte(b), &rootList)
	sawKept, sawAny := false, false
	for _, e := range rootList["entries"].([]any) {
		em := e.(map[string]any)
		if em["path_display"] == "/keep-me.txt" {
			sawKept = true
		}
		p, _ := em["path_display"].(string)
		if strings.HasPrefix(p, "/Projects") {
			sawAny = true
		}
	}
	if sawAny {
		t.Fatalf("/Projects subtree leaked into root listing: %s", b)
	}
	if !sawKept {
		t.Fatalf("sibling /keep-me.txt disappeared from root listing: %s", b)
	}

	// Listing the deleted folder 409s (its entries cannot be listed either).
	if _, st := postJSON(t, base+"/2/files/list_folder", map[string]any{"path": "/Projects"}); st != 409 {
		t.Fatalf("list_folder deleted folder -> %d, want 409", st)
	}

	// ===== no restore: re-creating at a deleted path is a fresh entry =====
	fresh := must200("upload", map[string]any{"path": "/Projects/plan.txt", "content": "new plan"})
	if fresh["id"] == plan["id"] {
		t.Fatalf("re-upload at deleted path reused the old id %v (delete is permanent, no tombstone reuse)", plan["id"])
	}

	// ===== single-file delete still works standalone =====
	if _, st := postJSON(t, base+"/2/files/delete", map[string]any{"path": "/keep-me.txt"}); st != 200 {
		t.Fatalf("delete sibling -> %d, want 200", st)
	}
	if _, st := postJSON(t, base+"/2/files/get_metadata", map[string]any{"path": "/keep-me.txt"}); st != 409 {
		t.Fatalf("get_metadata deleted sibling -> %d, want 409", st)
	}

	// Failure path: deleting a missing path stays 409 path/not_found.
	b, st = postJSON(t, base+"/2/files/delete", map[string]any{"path": "/Projects/Archive"})
	if st != 409 {
		t.Fatalf("delete missing -> %d, want 409", st)
	}
	var errBody map[string]any
	json.Unmarshal([]byte(b), &errBody)
	if es, _ := errBody["error_summary"].(string); !strings.HasPrefix(es, "path/not_found") {
		t.Fatalf("delete missing error_summary = %v, want path/not_found/..", errBody["error_summary"])
	}
}
