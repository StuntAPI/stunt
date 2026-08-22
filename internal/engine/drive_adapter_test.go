package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
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

// drivePatchAuth sends an authorized PATCH with a JSON body.
func drivePatchAuth(t *testing.T, url, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PATCH", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// driveDeleteAuth sends an authorized DELETE.
func driveDeleteAuth(t *testing.T, url, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// driveListQ runs an authorized files.list with the given query params.
func driveListQ(t *testing.T, base, token string, params url.Values) (map[string]any, int, string) {
	t.Helper()
	body, status := getAuth(t, base+"/drive/v3/files?"+params.Encode(), token)
	var list map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("unmarshal file list: %v (body %s)", err, body)
	}
	return list, status, body
}

// driveFileIDs extracts the ordered id list from a files.list response.
func driveFileIDs(t *testing.T, list map[string]any) []string {
	t.Helper()
	files, ok := list["files"].([]any)
	if !ok {
		t.Fatalf("file list has no files array: %v", list["files"])
	}
	ids := make([]string, 0, len(files))
	for _, f := range files {
		fm, ok := f.(map[string]any)
		if !ok {
			t.Fatalf("file entry is not an object: %v", f)
		}
		ids = append(ids, fm["id"].(string))
	}
	return ids
}

// TestDriveStyleResumableUpload drives the real Drive resumable protocol:
//
//   - POST ?uploadType=resumable → 200 with a Location session URL and an
//     empty body
//   - status probe (empty PUT, and the "bytes */N" form) → 308 Resume
//     Incomplete with a Range header tracking accepted bytes (no Range
//     before the first byte)
//   - three chunks (8 KiB + 4 KiB + 3 KiB binary) → 308s with
//     "Range: bytes=0-…" after each, then the final chunk (end == total-1)
//     → 200 with the file resource
//   - the assembled file downloads byte-exact via ?alt=media
//   - strict Content-Range violations → 400 (chunk start gap, differing
//     total, body shorter than the declared range)
//   - chunks on an unknown session id → 404
//   - cancel (DELETE session URL) → 499: session gone, no file created
func TestDriveStyleResumableUpload(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "drive-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
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
	const token = "ya29.mock_test_token_drive"

	// ===== Initiate =====
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(map[string]any{
		"name":     "resumable.bin",
		"mimeType": "application/octet-stream",
	}); err != nil {
		t.Fatal(err)
	}
	initResp := drivePutAuthBody(t, "POST", base+"/upload/drive/v3/files?uploadType=resumable", token, buf.String(), map[string]string{"Content-Type": "application/json"})
	if initResp.StatusCode != 200 {
		t.Fatalf("initiate -> %d", initResp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(initResp.Body)
	initResp.Body.Close()
	if len(bodyBytes) != 0 {
		t.Fatalf("initiate body = %q, want empty", bodyBytes)
	}
	loc := initResp.Header.Get("Location")
	if loc == "" || !strings.Contains(loc, "/upload/") {
		t.Fatalf("initiate Location = %q, want keyed session URL", loc)
	}

	// ===== Status probe before any bytes: 308, no Range =====
	probe := drivePutAuthBody(t, "PUT", loc, token, "", nil)
	if probe.StatusCode != 308 {
		t.Fatalf("probe -> %d, want 308", probe.StatusCode)
	}
	io.ReadAll(probe.Body)
	probe.Body.Close()
	if got := probe.Header.Get("Range"); got != "" {
		t.Fatalf("probe before first byte Range = %q, want none", got)
	}
	probe = drivePutAuthBody(t, "PUT", loc, token, "", map[string]string{"Content-Range": "bytes */15360"})
	if probe.StatusCode != 308 || probe.Header.Get("Range") != "" {
		t.Fatalf("star-form probe -> %d Range=%q, want 308 no Range", probe.StatusCode, probe.Header.Get("Range"))
	}
	io.ReadAll(probe.Body)
	probe.Body.Close()

	// ===== Chunks: 8 KiB + 4 KiB + final 3 KiB (total 15360) =====
	chunk1 := driveTestBytes(8*1024, 1)
	chunk2 := driveTestBytes(4*1024, 2)
	chunk3 := driveTestBytes(3*1024, 3)
	full := append(append(append([]byte{}, chunk1...), chunk2...), chunk3...)

	resp308 := drivePutAuthBody(t, "PUT", loc, token, string(chunk1),
		map[string]string{"Content-Range": "bytes 0-8191/15360"})
	if resp308.StatusCode != 308 || resp308.Header.Get("Range") != "bytes=0-8191" {
		t.Fatalf("chunk 1 -> %d Range=%q, want 308 bytes=0-8191", resp308.StatusCode, resp308.Header.Get("Range"))
	}
	io.ReadAll(resp308.Body)
	resp308.Body.Close()

	resp308 = drivePutAuthBody(t, "PUT", loc, token, string(chunk2),
		map[string]string{"Content-Range": "bytes 8192-12287/15360"})
	if resp308.StatusCode != 308 || resp308.Header.Get("Range") != "bytes=0-12287" {
		t.Fatalf("chunk 2 -> %d Range=%q, want 308 bytes=0-12287", resp308.StatusCode, resp308.Header.Get("Range"))
	}
	io.ReadAll(resp308.Body)
	resp308.Body.Close()

	// Progress is queryable mid-session.
	probe = drivePutAuthBody(t, "PUT", loc, token, "", nil)
	if probe.StatusCode != 308 || probe.Header.Get("Range") != "bytes=0-12287" {
		t.Fatalf("mid probe -> %d Range=%q, want 308 bytes=0-12287", probe.StatusCode, probe.Header.Get("Range"))
	}
	io.ReadAll(probe.Body)
	probe.Body.Close()

	// ===== Final chunk: 200 + file resource =====
	finalResp := drivePutAuthBody(t, "PUT", loc, token, string(chunk3),
		map[string]string{"Content-Range": "bytes 12288-15359/15360"})
	finalBody, _ := io.ReadAll(finalResp.Body)
	finalResp.Body.Close()
	if finalResp.StatusCode != 200 {
		t.Fatalf("final chunk -> %d; body %s", finalResp.StatusCode, finalBody)
	}
	var file map[string]any
	if err := json.Unmarshal(finalBody, &file); err != nil {
		t.Fatalf("unmarshal final chunk response: %v (body %s)", err, finalBody)
	}
	fileID, _ := file["id"].(string)
	if fileID == "" || file["name"] != "resumable.bin" {
		t.Fatalf("final chunk file = %v, want id + name resumable.bin", file)
	}
	if file["size"] != strconv.Itoa(len(full)) {
		t.Fatalf("final chunk size = %v, want %d (int64-as-string)", file["size"], len(full))
	}

	// Byte-exact download.
	content, status := getAuth(t, base+"/drive/v3/files/"+fileID+"?alt=media", token)
	if status != 200 {
		t.Fatalf("download -> %d", status)
	}
	if !bytes.Equal([]byte(content), full) {
		t.Fatalf("downloaded content mismatch: got %d bytes, want %d", len(content), len(full))
	}

	// The session is gone after completion.
	again := drivePutAuthBody(t, "PUT", loc, token, "", nil)
	io.ReadAll(again.Body)
	again.Body.Close()
	if again.StatusCode != 404 {
		t.Fatalf("probe after completion -> %d, want 404", again.StatusCode)
	}

	// ===== Strict protocol violations on a fresh session =====
	var buf2 bytes.Buffer
	if err := json.NewEncoder(&buf2).Encode(map[string]any{
		"name":     "resumable-cancel.bin",
		"mimeType": "application/octet-stream",
	}); err != nil {
		t.Fatal(err)
	}
	initResp = drivePutAuthBody(t, "POST", base+"/upload/drive/v3/files?uploadType=resumable", token, buf2.String(), map[string]string{"Content-Type": "application/json"})
	io.ReadAll(initResp.Body)
	initResp.Body.Close()
	loc2 := initResp.Header.Get("Location")

	// Skipping ahead (start != next expected) → 400.
	bad := drivePutAuthBody(t, "PUT", loc2, token, string(chunk2),
		map[string]string{"Content-Range": "bytes 8192-12287/15360"})
	if bad.StatusCode != 400 {
		t.Fatalf("chunk with gap -> %d, want 400", bad.StatusCode)
	}
	io.ReadAll(bad.Body)
	bad.Body.Close()

	// First chunk fine; a differing total on the next chunk → 400.
	if r := drivePutAuthBody(t, "PUT", loc2, token, string(chunk1), map[string]string{"Content-Range": "bytes 0-8191/15360"}); r.StatusCode != 308 {
		t.Fatalf("chunk 1 (second session) -> %d, want 308", r.StatusCode)
	} else {
		io.ReadAll(r.Body)
		r.Body.Close()
	}
	bad = drivePutAuthBody(t, "PUT", loc2, token, string(chunk2),
		map[string]string{"Content-Range": "bytes 8192-12287/99999"})
	if bad.StatusCode != 400 {
		t.Fatalf("differing total -> %d, want 400", bad.StatusCode)
	}
	io.ReadAll(bad.Body)
	bad.Body.Close()

	// Body shorter than the declared range → 400.
	bad = drivePutAuthBody(t, "PUT", loc2, token, "short",
		map[string]string{"Content-Range": "bytes 8192-12287/15360"})
	if bad.StatusCode != 400 {
		t.Fatalf("short body -> %d, want 400", bad.StatusCode)
	}
	io.ReadAll(bad.Body)
	bad.Body.Close()

	// ===== Cancel: 499, session discarded, no file created =====
	_, cancelStatus := driveDeleteAuth(t, loc2, token)
	if cancelStatus != 499 {
		t.Fatalf("cancel -> %d, want 499", cancelStatus)
	}
	after := drivePutAuthBody(t, "PUT", loc2, token, "", nil)
	io.ReadAll(after.Body)
	after.Body.Close()
	if after.StatusCode != 404 {
		t.Fatalf("probe after cancel -> %d, want 404", after.StatusCode)
	}
	listBody, status := getAuth(t, base+"/drive/v3/files?q="+url.QueryEscape("name = 'resumable-cancel.bin'"), token)
	if status != 200 {
		t.Fatalf("list after cancel -> %d", status)
	}
	var list map[string]any
	if err := json.Unmarshal([]byte(listBody), &list); err != nil {
		t.Fatal(err)
	}
	if n := len(list["files"].([]any)); n != 0 {
		t.Fatalf("canceled session created %d files, want 0", n)
	}

	// Unknown session id → 404.
	missing := drivePutAuthBody(t, "PUT", base+"/upload/drive/v3/files/sid_nope", token, "", nil)
	io.ReadAll(missing.Body)
	missing.Body.Close()
	if missing.StatusCode != 404 {
		t.Fatalf("unknown session -> %d, want 404", missing.StatusCode)
	}
}

// drivePutAuthBody issues an authorized request with a string body and
// extra headers, returning the raw response.
func drivePutAuthBody(t *testing.T, method, urlStr, token, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, urlStr, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// driveTestBytes returns n deterministic pseudo-random bytes.
func driveTestBytes(n int, seed int) []byte {
	out := make([]byte, n)
	x := uint32(seed)*2654435761 + 55555
	for i := range out {
		x = x*1664525 + 1013904223
		out[i] = byte(x >> 19)
	}
	return out
}

// TestDriveStyleAdapter exercises the broader Google-Drive-style reference
// adapter end-to-end: OAuth2 (authorize → token → refresh, 401s), file
// upload → get metadata → download content → list (q grammar subset,
// unparseable q → 400) → patch (rename) → delete → 404 after delete; folder
// creation; about/quota; and the real changes feed (startPageToken cursor,
// entries recorded by mutations, removed on delete).
// State persists across requests within the session.
func TestDriveStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "drive-style")
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
			"drive": {Adapter: absAdapterDir},
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

	// ===== Auth is enforced =====

	// GET /drive/v3/files without a token → 401
	_, status := get2(t, base+"/drive/v3/files")
	if status != 401 {
		t.Fatalf("GET list without token -> status %d, want 401", status)
	}
	// POST upload with an unknown token → 401
	_, status = postJSONAuth(t, base+"/upload/drive/v3/files", "ya29.not_a_real_token", map[string]any{
		"name": "nope.txt",
	})
	if status != 401 {
		t.Fatalf("POST upload with unknown token -> status %d, want 401", status)
	}

	// ===== OAuth2: authorize → 302, token → access + refresh =====

	const redirectURI = "http://localhost:8080/callback"
	const state = "drive-state-abc"
	const clientID = "test-drive-client-id"
	const clientSecret = "test-drive-client-secret"

	resp := googleGetNoRedirect(t, base+"/o/oauth2/auth?"+
		"client_id="+clientID+
		"&redirect_uri="+url.QueryEscape(redirectURI)+
		"&state="+state+
		"&response_type=code&scope=https%3A%2F%2Fwww.googleapis.com%2Fauth%2Fdrive")
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> status %d, want 302", resp.StatusCode)
	}
	authCode := googleExtractParam(resp.Header.Get("Location"), "code")
	if authCode == "" {
		t.Fatalf("authorize: no code in Location %q", resp.Header.Get("Location"))
	}
	if googleExtractParam(resp.Header.Get("Location"), "state") != state {
		t.Fatalf("authorize: state mismatch in Location %q", resp.Header.Get("Location"))
	}

	body, status := googlePostForm(t, base+"/o/oauth2/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if status != 200 {
		t.Fatalf("token (auth code) -> status %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token response: %v (body %s)", err, body)
	}
	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || !strings.HasPrefix(accessToken, "ya29.") {
		t.Fatalf("access_token = %v, want ya29.* prefix", tokenResp["access_token"])
	}
	refreshToken, ok := tokenResp["refresh_token"].(string)
	if !ok || !strings.HasPrefix(refreshToken, "1//") {
		t.Fatalf("refresh_token = %v, want 1//* prefix", tokenResp["refresh_token"])
	}
	if tokenResp["token_type"] != "Bearer" || tokenResp["expires_in"] != float64(3599) {
		t.Fatalf("token_type/expires_in = %v/%v, want Bearer/3599", tokenResp["token_type"], tokenResp["expires_in"])
	}

	// Code is single-use: replay → 400
	_, status = googlePostForm(t, base+"/o/oauth2/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if status != 400 {
		t.Fatalf("replay code -> status %d, want 400", status)
	}

	// Refresh grant → new access token for the same user; refresh not rotated.
	body, status = googlePostForm(t, base+"/o/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
	if status != 200 {
		t.Fatalf("token (refresh) -> status %d, want 200; body %s", status, body)
	}
	var refreshResp map[string]any
	if err := json.Unmarshal([]byte(body), &refreshResp); err != nil {
		t.Fatalf("unmarshal refresh response: %v (body %s)", err, body)
	}
	if refreshResp["refresh_token"] != refreshToken {
		t.Fatalf("refresh token rotated: %v, want %v", refreshResp["refresh_token"], refreshToken)
	}
	refreshedAccess, _ := refreshResp["access_token"].(string)
	if refreshedAccess == "" || refreshedAccess == accessToken {
		t.Fatalf("refresh grant should mint a new access token, got %q", refreshedAccess)
	}

	// Refresh with an unknown refresh token → 400 (failure path).
	_, status = googlePostForm(t, base+"/o/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"1//unknown_refresh_token"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
	if status != 400 {
		t.Fatalf("refresh with unknown token -> status %d, want 400", status)
	}

	// The refreshed access token is valid for protected routes.
	if _, status = getAuth(t, base+"/drive/v3/about", refreshedAccess); status != 200 {
		t.Fatalf("GET about with refreshed token -> status %d, want 200", status)
	}

	// The static seeded test token also works (insert-once seed).
	if _, status = getAuth(t, base+"/drive/v3/about", "ya29.mock_test_token_drive"); status != 200 {
		t.Fatalf("GET about with static test token -> status %d, want 200", status)
	}

	// Use the flow-minted token for the rest of the lifecycle.

	// ===== Create a folder (for the parents-in q clause) =====

	body, status = postJSONAuth(t, base+"/upload/drive/v3/files", accessToken, map[string]any{
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

	// ===== Upload a file =====

	// POST /upload/drive/v3/files → 201, id with file_ prefix, parents kept
	body, status = postJSONAuth(t, base+"/upload/drive/v3/files", accessToken, map[string]any{
		"name":    "test-document.txt",
		"content": "Hello, Drive-style world!",
		"parents": []string{folderID},
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
	if file["size"] != strconv.Itoa(len("Hello, Drive-style world!")) {
		t.Fatalf("file size = %v, want %d (int64-as-string)", file["size"], len("Hello, Drive-style world!"))
	}
	if ps, ok := file["parents"].([]any); !ok || len(ps) != 1 || ps[0] != folderID {
		t.Fatalf("file parents = %v, want [%s]", file["parents"], folderID)
	}

	// ===== Get metadata =====

	// GET /drive/v3/files/{id} → 200, metadata persisted
	body, status = getAuth(t, base+"/drive/v3/files/"+fileID, accessToken)
	if status != 200 {
		t.Fatalf("GET metadata -> status %d, want 200; body %s", status, body)
	}
	var retrieved map[string]any
	if err := json.Unmarshal([]byte(body), &retrieved); err != nil {
		t.Fatalf("unmarshal retrieved: %v (body %s)", err, body)
	}
	if retrieved["id"] != fileID || retrieved["name"] != "test-document.txt" {
		t.Fatalf("retrieved = %v/%v, want %s/test-document.txt", retrieved["id"], retrieved["name"], fileID)
	}

	// GET /drive/v3/files/{nonexistent} → 404
	_, status = getAuth(t, base+"/drive/v3/files/does-not-exist", accessToken)
	if status != 404 {
		t.Fatalf("GET unknown file -> status %d, want 404", status)
	}

	// ===== Download content =====

	// GET /drive/v3/files/{id}?alt=media → 200, raw content matches
	body, status = getAuth(t, base+"/drive/v3/files/"+fileID+"?alt=media", accessToken)
	if status != 200 {
		t.Fatalf("GET alt=media -> status %d, want 200; body %s", status, body)
	}
	if body != "Hello, Drive-style world!" {
		t.Fatalf("downloaded content = %q, want %q", body, "Hello, Drive-style world!")
	}

	// GET folder ?alt=media → 400 (cannot download folder)
	_, status = getAuth(t, base+"/drive/v3/files/"+folderID+"?alt=media", accessToken)
	if status != 400 {
		t.Fatalf("GET folder alt=media -> status %d, want 400", status)
	}

	// ===== List files =====

	// GET /drive/v3/files → 200, list containing our file + seed folder
	body, status = getAuth(t, base+"/drive/v3/files", accessToken)
	if status != 200 {
		t.Fatalf("GET list -> status %d, want 200; body %s", status, body)
	}
	var fileList map[string]any
	if err := json.Unmarshal([]byte(body), &fileList); err != nil {
		t.Fatalf("unmarshal file list: %v (body %s)", err, body)
	}
	files, ok := fileList["files"].([]any)
	if !ok || len(files) < 3 { // 1 seed folder + 1 folder + 1 uploaded
		t.Fatalf("file list has %d items, want >= 3", len(files))
	}

	// ===== q grammar subset =====

	// name = 'x' → exactly the matching file
	list, status, rawBody := driveListQ(t, base, accessToken, url.Values{
		"q": {"name = 'test-document.txt'"},
	})
	if status != 200 {
		t.Fatalf("q name = -> status %d, want 200; body %s", status, rawBody)
	}
	ids := driveFileIDs(t, list)
	if len(ids) != 1 || ids[0] != fileID {
		t.Fatalf("q name = 'test-document.txt' -> ids %v, want [%s]", ids, fileID)
	}

	// name contains '=' — a literal containing an equals sign
	body, status = postJSONAuth(t, base+"/upload/drive/v3/files", accessToken, map[string]any{
		"name":    "notes=2024.txt",
		"content": "eq",
	})
	if status != 201 {
		t.Fatalf("POST upload eq file -> status %d, want 201; body %s", status, body)
	}
	var eqFile map[string]any
	if err := json.Unmarshal([]byte(body), &eqFile); err != nil {
		t.Fatalf("unmarshal eq file: %v (body %s)", err, body)
	}
	list, status, rawBody = driveListQ(t, base, accessToken, url.Values{
		"q": {"name contains '='"},
	})
	if status != 200 {
		t.Fatalf("q name contains '=' -> status %d, want 200; body %s", status, rawBody)
	}
	ids = driveFileIDs(t, list)
	if len(ids) != 1 || ids[0] != eqFile["id"].(string) {
		t.Fatalf("q name contains '=' -> ids %v, want [%s]", ids, eqFile["id"])
	}

	// mimeType = folder
	list, _, _ = driveListQ(t, base, accessToken, url.Values{
		"q": {"mimeType = 'application/vnd.google-apps.folder'"},
	})
	ids = driveFileIDs(t, list)
	if len(ids) != 2 { // seed folder + our folder
		t.Fatalf("q mimeType folder -> ids %v, want 2 folders", ids)
	}

	// parents in '<folderId>'
	list, _, _ = driveListQ(t, base, accessToken, url.Values{
		"q": {"parents in '" + folderID + "'"},
	})
	ids = driveFileIDs(t, list)
	if len(ids) != 1 || ids[0] != fileID {
		t.Fatalf("q parents in -> ids %v, want [%s]", ids, fileID)
	}

	// modifiedTime > '...' (seed folder is 2024-01-10, uploads are 2024-01-15)
	list, _, _ = driveListQ(t, base, accessToken, url.Values{
		"q": {"modifiedTime > '2024-01-01T00:00:00Z'"},
	})
	ids = driveFileIDs(t, list)
	if len(ids) != 4 {
		t.Fatalf("q modifiedTime > -> ids %v, want 4 files", ids)
	}
	list, _, _ = driveListQ(t, base, accessToken, url.Values{
		"q": {"modifiedTime > '2024-01-15T00:00:00Z'"},
	})
	ids = driveFileIDs(t, list)
	if len(ids) != 3 { // folder + two uploads at 2024-01-15, not the 2024-01-10 seed
		t.Fatalf("q modifiedTime > 2024-01-15 -> ids %v, want 3 files", ids)
	}

	// AND of two clauses
	list, _, _ = driveListQ(t, base, accessToken, url.Values{
		"q": {"name contains 'document' and mimeType != 'application/vnd.google-apps.folder'"},
	})
	ids = driveFileIDs(t, list)
	if len(ids) != 1 || ids[0] != fileID {
		t.Fatalf("q and -> ids %v, want [%s]", ids, fileID)
	}

	// OR of two clauses
	list, _, _ = driveListQ(t, base, accessToken, url.Values{
		"q": {"name = 'no-such-file' or name = 'notes=2024.txt'"},
	})
	ids = driveFileIDs(t, list)
	if len(ids) != 1 || ids[0] != eqFile["id"].(string) {
		t.Fatalf("q or -> ids %v, want [%s]", ids, eqFile["id"])
	}

	// Unparseable q → 400 (unquoted literal)
	_, status, rawBody = driveListQ(t, base, accessToken, url.Values{
		"q": {"name = test-document.txt"},
	})
	if status != 400 {
		t.Fatalf("q unquoted literal -> status %d, want 400; body %s", status, rawBody)
	}
	// Unsupported field → 400
	_, status, rawBody = driveListQ(t, base, accessToken, url.Values{
		"q": {"unknownField = 'x'"},
	})
	if status != 400 {
		t.Fatalf("q unknown field -> status %d, want 400; body %s", status, rawBody)
	}
	// Dangling operator → 400
	_, status, rawBody = driveListQ(t, base, accessToken, url.Values{
		"q": {"name = 'x' and"},
	})
	if status != 400 {
		t.Fatalf("q dangling operator -> status %d, want 400; body %s", status, rawBody)
	}

	// ===== Patch (rename) =====

	// PATCH /drive/v3/files/{id} → 200, name updated
	body, status = drivePatchAuth(t, base+"/drive/v3/files/"+fileID, accessToken, map[string]any{
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
	_, status = drivePatchAuth(t, base+"/drive/v3/files/no-such-file", accessToken, map[string]any{"name": "x"})
	if status != 404 {
		t.Fatalf("PATCH unknown -> status %d, want 404", status)
	}

	// ===== Trash via PATCH =====

	// PATCH /drive/v3/files/{id} with trashed=true → 200
	body, status = drivePatchAuth(t, base+"/drive/v3/files/"+fileID, accessToken, map[string]any{
		"trashed": true,
	})
	if status != 200 {
		t.Fatalf("PATCH trash -> status %d, want 200; body %s", status, body)
	}
	// Trashed file should not appear in default list
	body, status = getAuth(t, base+"/drive/v3/files", accessToken)
	if err := json.Unmarshal([]byte(body), &fileList); err != nil {
		t.Fatalf("unmarshal file list after trash: %v", err)
	}
	files = fileList["files"].([]any)
	for _, f := range files {
		if fm, ok := f.(map[string]any); ok && fm["id"] == fileID {
			t.Fatalf("trashed file %s should not appear in list", fileID)
		}
	}
	// ...but an explicit trashed clause surfaces it
	list, _, _ = driveListQ(t, base, accessToken, url.Values{
		"q": {"trashed = true"},
	})
	ids = driveFileIDs(t, list)
	if len(ids) != 1 || ids[0] != fileID {
		t.Fatalf("q trashed = true -> ids %v, want [%s]", ids, fileID)
	}
	// ...and trashed = false excludes it
	list, _, _ = driveListQ(t, base, accessToken, url.Values{
		"q": {"trashed = false"},
	})
	for _, id := range driveFileIDs(t, list) {
		if id == fileID {
			t.Fatalf("q trashed = false should exclude %s", fileID)
		}
	}

	// ===== Changes feed =====

	// GET /drive/v3/changes without pageToken → 400 (required param)
	_, status = getAuth(t, base+"/drive/v3/changes", accessToken)
	if status != 400 {
		t.Fatalf("GET changes without pageToken -> status %d, want 400", status)
	}

	// changes since token 0 → one entry per mutation so far (folder create,
	// upload, eq upload, rename, trash)
	body, status = getAuth(t, base+"/drive/v3/changes?pageToken=0", accessToken)
	if status != 200 {
		t.Fatalf("GET changes -> status %d, want 200; body %s", status, body)
	}
	var changes map[string]any
	if err := json.Unmarshal([]byte(body), &changes); err != nil {
		t.Fatalf("unmarshal changes: %v (body %s)", err, body)
	}
	changeList, ok := changes["changes"].([]any)
	if !ok || len(changeList) != 5 {
		t.Fatalf("changes has %v entries, want 5", changes["changes"])
	}
	if changes["newStartPageToken"] == nil || changes["newStartPageToken"] == "" {
		t.Fatalf("changes.newStartPageToken = %v, want non-empty", changes["newStartPageToken"])
	}
	// The last recorded change is the trash patch, carrying the file metadata.
	lastChange := changeList[len(changeList)-1].(map[string]any)
	if lastChange["fileId"] != fileID || lastChange["removed"] != false {
		t.Fatalf("last change = %v, want fileId %s removed=false", lastChange, fileID)
	}
	if lastChange["file"].(map[string]any)["trashed"] != true {
		t.Fatalf("last change file.trashed = %v, want true", lastChange["file"].(map[string]any)["trashed"])
	}

	// startPageToken matches newStartPageToken: polling from it sees only
	// future changes.
	body, status = getAuth(t, base+"/drive/v3/changes/startPageToken", accessToken)
	if status != 200 {
		t.Fatalf("GET changes/startPageToken -> status %d, want 200; body %s", status, body)
	}
	var spt map[string]any
	if err := json.Unmarshal([]byte(body), &spt); err != nil {
		t.Fatalf("unmarshal startPageToken: %v (body %s)", err, body)
	}
	cursor, _ := spt["startPageToken"].(string)
	if cursor == "" {
		t.Fatalf("startPageToken = %v, want non-empty", spt["startPageToken"])
	}
	body, status = getAuth(t, base+"/drive/v3/changes?pageToken="+url.QueryEscape(cursor), accessToken)
	if status != 200 {
		t.Fatalf("GET changes from cursor -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &changes); err != nil {
		t.Fatalf("unmarshal changes from cursor: %v (body %s)", err, body)
	}
	if n := len(changes["changes"].([]any)); n != 0 {
		t.Fatalf("changes from fresh cursor has %d entries, want 0", n)
	}

	// ===== Delete =====

	// DELETE /drive/v3/files/{id} → 204
	_, status = driveDeleteAuth(t, base+"/drive/v3/files/"+fileID, accessToken)
	if status != 204 {
		t.Fatalf("DELETE file -> status %d, want 204", status)
	}

	// GET after delete → 404
	_, status = getAuth(t, base+"/drive/v3/files/"+fileID, accessToken)
	if status != 404 {
		t.Fatalf("GET deleted file -> status %d, want 404", status)
	}

	// DELETE unknown → 404
	_, status = driveDeleteAuth(t, base+"/drive/v3/files/no-such-file", accessToken)
	if status != 404 {
		t.Fatalf("DELETE unknown -> status %d, want 404", status)
	}

	// The delete produced a change entry with removed=true and no file.
	body, status = getAuth(t, base+"/drive/v3/changes?pageToken="+url.QueryEscape(cursor), accessToken)
	if status != 200 {
		t.Fatalf("GET changes after delete -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &changes); err != nil {
		t.Fatalf("unmarshal changes after delete: %v (body %s)", err, body)
	}
	changeList = changes["changes"].([]any)
	if len(changeList) != 1 {
		t.Fatalf("changes after delete has %d entries, want 1", len(changeList))
	}
	delChange := changeList[0].(map[string]any)
	if delChange["fileId"] != fileID || delChange["removed"] != true || delChange["file"] != nil {
		t.Fatalf("delete change = %v, want fileId %s removed=true no file", delChange, fileID)
	}

	// ===== About / quota =====

	// GET /drive/v3/about → 200, synthetic storageQuota + user
	body, status = getAuth(t, base+"/drive/v3/about", accessToken)
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

	// ===== Catch-all 404 =====

	_, status = getAuth(t, base+"/drive/v3/no-such-resource", accessToken)
	if status != 404 {
		t.Fatalf("GET unmatched route -> status %d, want 404", status)
	}
}
