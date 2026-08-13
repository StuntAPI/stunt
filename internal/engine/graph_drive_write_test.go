package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/primitives/clock"
)

// bootGraphService boots the microsoft-graph-style reference adapter on a
// free port with an optional max_body_bytes and returns its base URL.
func bootGraphService(t *testing.T, maxBodyBytes int64) string {
	t.Helper()
	base, _ := bootGraphServiceWithClock(t, maxBodyBytes, nil)
	return base
}

// bootGraphServiceWithClock is like bootGraphService but injects a clock (nil
// = real-time default) and returns the engine so tests can inspect state.
func bootGraphServiceWithClock(t *testing.T, maxBodyBytes int64, clk *clock.Clock) (string, *Engine) {
	t.Helper()

	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "microsoft-graph-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"graph": {Adapter: adapterDir, MaxBodyBytes: maxBodyBytes},
		},
	}
	var opts []Option
	if clk != nil {
		opts = append(opts, WithClock(clk))
	}
	e, err := New(m, opts...)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	t.Cleanup(cancel)
	time.Sleep(50 * time.Millisecond)
	return addrs["graph"], e
}

// graphDo performs an HTTP request with optional bearer auth and body, and
// returns status, body bytes, and the response.
func graphDo(t *testing.T, method, url, token string, payload []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// graphJSON unmarshals a JSON object body or fails the test.
func graphJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %q: %v", b, err)
	}
	return out
}

const graphToken = "mock-bearer-token"

// TestGraphDriveSimpleUpload proves the simple upload path (files under
// 4 MB): PUT root:/{name}:/content stores the bytes, creates a driveItem,
// and the content round-trips byte-exact through GET items/{id}/content.
func TestGraphDriveSimpleUpload(t *testing.T) {
	base := bootGraphService(t, 0)

	original := allByteValues(2048)

	// ===== PUT /v1.0/me/drive/root:/fixture.bin:/content -> 201 driveItem =====

	status, body := graphDo(t, "PUT", base+"/v1.0/me/drive/root:/fixture.bin:/content", graphToken,
		original, map[string]string{"Content-Type": "application/octet-stream"})
	if status != 201 {
		t.Fatalf("simple upload -> status %d, want 201; body %s", status, body)
	}
	item := graphJSON(t, body)
	itemID, _ := item["id"].(string)
	if itemID == "" {
		t.Fatalf("driveItem.id missing: %s", body)
	}
	if item["name"] != "fixture.bin" {
		t.Fatalf("driveItem.name = %v, want fixture.bin", item["name"])
	}
	if int64(item["size"].(float64)) != int64(len(original)) {
		t.Fatalf("driveItem.size = %v, want %d", item["size"], len(original))
	}
	parentRef, ok := item["parentReference"].(map[string]any)
	if !ok {
		t.Fatalf("driveItem.parentReference = %v, want object", item["parentReference"])
	}
	if parentRef["id"] != "root" {
		t.Fatalf("parentReference.id = %v, want root", parentRef["id"])
	}

	// ===== GET /v1.0/me/drive/items/{id}/content -> byte-exact =====

	status, got := graphDo(t, "GET", base+"/v1.0/me/drive/items/"+itemID+"/content", graphToken, nil, nil)
	if status != 200 {
		t.Fatalf("get content -> status %d, want 200", status)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("content is not byte-equal: got %d bytes, want %d", len(got), len(original))
	}

	// ===== default PUT to the same name replaces (200, same id) =====

	replaced := []byte("replaced-content-bytes")
	status, body = graphDo(t, "PUT", base+"/v1.0/me/drive/root:/fixture.bin:/content", graphToken,
		replaced, map[string]string{"Content-Type": "application/octet-stream"})
	if status != 200 {
		t.Fatalf("replace upload -> status %d, want 200; body %s", status, body)
	}
	rep := graphJSON(t, body)
	if rep["id"] != itemID {
		t.Fatalf("replace changed the item id: %v, want %v", rep["id"], itemID)
	}
	status, got = graphDo(t, "GET", base+"/v1.0/me/drive/items/"+itemID+"/content", graphToken, nil, nil)
	if status != 200 || !bytes.Equal(got, replaced) {
		t.Fatalf("replaced content mismatch: status %d, got %q", status, got)
	}

	// ===== conflictBehavior=rename creates "fixture (1).bin" =====

	renamed := []byte("renamed-copy-bytes")
	status, body = graphDo(t, "PUT",
		base+"/v1.0/me/drive/root:/fixture.bin:/content?@microsoft.graph.conflictBehavior=rename",
		graphToken, renamed, map[string]string{"Content-Type": "application/octet-stream"})
	if status != 201 {
		t.Fatalf("rename upload -> status %d, want 201; body %s", status, body)
	}
	ren := graphJSON(t, body)
	if ren["name"] != "fixture (1).bin" {
		t.Fatalf("renamed item name = %v, want %q", ren["name"], "fixture (1).bin")
	}
	if ren["id"] == itemID {
		t.Fatal("rename reused the existing item id; want a new item")
	}

	// ===== conflictBehavior=fail -> 409 =====

	status, body = graphDo(t, "PUT",
		base+"/v1.0/me/drive/root:/fixture.bin:/content?@microsoft.graph.conflictBehavior=fail",
		graphToken, []byte("x"), nil)
	if status != 409 {
		t.Fatalf("fail-on-conflict upload -> status %d, want 409; body %s", status, body)
	}

	// ===== malformed addressing (no trailing colon) -> 400 =====

	status, _ = graphDo(t, "PUT", base+"/v1.0/me/drive/root:/fixture.bin/content", graphToken, []byte("x"), nil)
	if status != 400 {
		t.Fatalf("missing trailing colon -> status %d, want 400", status)
	}

	// ===== no auth -> 401 =====

	status, _ = graphDo(t, "PUT", base+"/v1.0/me/drive/root:/other.bin:/content", "", []byte("x"), nil)
	if status != 401 {
		t.Fatalf("simple upload without auth -> status %d, want 401", status)
	}
}

// TestGraphDriveFoldersAndResolution proves createFolder, per-parent child
// listing, path resolution with select=id, and the folder upload variant.
func TestGraphDriveFoldersAndResolution(t *testing.T) {
	base := bootGraphService(t, 0)

	// ===== POST /v1.0/me/drive/root/children -> createFolder =====

	folderBody, _ := json.Marshal(map[string]any{"name": "Backups", "folder": map[string]any{}})
	status, body := graphDo(t, "POST", base+"/v1.0/me/drive/root/children", graphToken,
		folderBody, map[string]string{"Content-Type": "application/json"})
	if status != 201 {
		t.Fatalf("createFolder -> status %d, want 201; body %s", status, body)
	}
	folder := graphJSON(t, body)
	folderID, _ := folder["id"].(string)
	if folderID == "" {
		t.Fatalf("folder id missing: %s", body)
	}
	if _, ok := folder["folder"].(map[string]any); !ok {
		t.Fatalf("created item lacks folder facet: %s", body)
	}

	// Creating the same folder again defaults to fail -> 409.
	status, _ = graphDo(t, "POST", base+"/v1.0/me/drive/root/children", graphToken,
		folderBody, map[string]string{"Content-Type": "application/json"})
	if status != 409 {
		t.Fatalf("duplicate createFolder -> status %d, want 409", status)
	}

	// ===== GET /v1.0/me/drive/root:/Backups:/?select=id -> folder id =====

	status, body = graphDo(t, "GET", base+"/v1.0/me/drive/root:/Backups:/?select=id", graphToken, nil, nil)
	if status != 200 {
		t.Fatalf("resolve folder -> status %d, want 200; body %s", status, body)
	}
	resolved := graphJSON(t, body)
	if resolved["id"] != folderID {
		t.Fatalf("resolved id = %v, want %v", resolved["id"], folderID)
	}

	// Missing path -> 404.
	status, _ = graphDo(t, "GET", base+"/v1.0/me/drive/root:/NoSuchFolder:/?select=id", graphToken, nil, nil)
	if status != 404 {
		t.Fatalf("resolve missing folder -> status %d, want 404", status)
	}

	// ===== upload into the folder: PUT items/{parentId}:/{name}:/content =====

	payload := []byte("folder-scoped-content")
	status, body = graphDo(t, "PUT",
		base+"/v1.0/me/drive/items/"+folderID+":/photo.bin:/content", graphToken,
		payload, map[string]string{"Content-Type": "application/octet-stream"})
	if status != 201 {
		t.Fatalf("folder upload -> status %d, want 201; body %s", status, body)
	}
	item := graphJSON(t, body)
	itemID, _ := item["id"].(string)
	parentRef, _ := item["parentReference"].(map[string]any)
	if parentRef == nil || parentRef["id"] != folderID {
		t.Fatalf("folder upload parentReference = %v, want id %v", item["parentReference"], folderID)
	}

	// Unknown parent id -> 404.
	status, _ = graphDo(t, "PUT",
		base+"/v1.0/me/drive/items/folder-does-not-exist:/photo.bin:/content", graphToken,
		payload, nil)
	if status != 404 {
		t.Fatalf("upload to unknown parent -> status %d, want 404", status)
	}

	// ===== per-parent listing =====

	// The folder's children contain photo.bin.
	status, body = graphDo(t, "GET", base+"/v1.0/me/drive/items/"+folderID+"/children", graphToken, nil, nil)
	if status != 200 {
		t.Fatalf("list folder children -> status %d, want 200; body %s", status, body)
	}
	var children graphODataList
	if err := json.Unmarshal(body, &children); err != nil {
		t.Fatalf("unmarshal children: %v", err)
	}
	foundInFolder := false
	for _, c := range children.Value {
		if c["id"] == itemID {
			foundInFolder = true
		}
	}
	if !foundInFolder {
		t.Fatalf("photo.bin not listed in its folder's children: %s", body)
	}

	// Root children contain the folder but NOT the nested file.
	status, body = graphDo(t, "GET", base+"/v1.0/me/drive/root/children", graphToken, nil, nil)
	if status != 200 {
		t.Fatalf("list root children -> status %d, want 200", status)
	}
	var rootChildren graphODataList
	if err := json.Unmarshal(body, &rootChildren); err != nil {
		t.Fatalf("unmarshal root children: %v", err)
	}
	rootHasFolder, rootHasNested := false, false
	for _, c := range rootChildren.Value {
		if c["id"] == folderID {
			rootHasFolder = true
		}
		if c["id"] == itemID {
			rootHasNested = true
		}
	}
	if !rootHasFolder {
		t.Fatal("created folder missing from root children")
	}
	if rootHasNested {
		t.Fatal("nested file leaked into root children; listing must be per-parent")
	}

	// createFolder inside a folder via items/{id}/children.
	subBody, _ := json.Marshal(map[string]any{"name": "Nested", "folder": map[string]any{}})
	status, body = graphDo(t, "POST", base+"/v1.0/me/drive/items/"+folderID+"/children", graphToken,
		subBody, map[string]string{"Content-Type": "application/json"})
	if status != 201 {
		t.Fatalf("nested createFolder -> status %d, want 201; body %s", status, body)
	}
	sub := graphJSON(t, body)
	subParent, _ := sub["parentReference"].(map[string]any)
	if subParent == nil || subParent["id"] != folderID {
		t.Fatalf("nested folder parentReference = %v, want id %v", sub["parentReference"], folderID)
	}

	// ===== quota still answers a select=quota query =====

	status, body = graphDo(t, "GET", base+"/v1.0/me/drive?select=quota", graphToken, nil, nil)
	if status != 200 {
		t.Fatalf("drive select=quota -> status %d, want 200", status)
	}
	drive := graphJSON(t, body)
	if _, ok := drive["quota"].(map[string]any); !ok {
		t.Fatalf("drive response lacks quota object: %s", body)
	}
}

// TestGraphDriveUploadSession proves the strict resumable protocol:
// sequential contiguous chunks, 416 on violations, 202 + nextExpectedRanges
// mid-session, 201 + driveItem on the final range, session gone afterwards.
func TestGraphDriveUploadSession(t *testing.T) {
	base := bootGraphService(t, 0)
	host := strings.TrimPrefix(base, "http://")

	original := allByteValues(2500)
	total := len(original)

	// ===== POST createUploadSession =====

	sessBody, _ := json.Marshal(map[string]any{
		"item": map[string]any{"@microsoft.graph.conflictBehavior": "rename", "name": "big.bin"},
	})
	status, body := graphDo(t, "POST", base+"/v1.0/me/drive/root:/big.bin:/createUploadSession",
		graphToken, sessBody, map[string]string{"Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("createUploadSession -> status %d, want 200; body %s", status, body)
	}
	sess := graphJSON(t, body)
	uploadURL, _ := sess["uploadUrl"].(string)
	if !strings.HasPrefix(uploadURL, "http://"+host+"/v1.0/_upload/") {
		t.Fatalf("uploadUrl = %q, want prefix http://%s/v1.0/_upload/", uploadURL, host)
	}
	if _, ok := sess["expirationDateTime"].(string); !ok {
		t.Fatalf("expirationDateTime missing: %s", body)
	}
	// expirationDateTime is now ~48h ahead (was a 2030 placeholder); prove the
	// TTL is real without depending on an exact wall clock.
	if exp, _ := sess["expirationDateTime"].(string); exp != "" {
		when, err := time.Parse(time.RFC3339, exp)
		if err != nil {
			t.Fatalf("expirationDateTime not RFC3339: %q", exp)
		}
		if d := time.Until(when); d < 47*time.Hour || d > 49*time.Hour {
			t.Fatalf("expirationDateTime = %s (%v ahead), want ~48h", exp, d)
		}
	}

	putChunk := func(start, end int, declaredTotal int) (int, []byte) {
		return graphDo(t, "PUT", uploadURL, "", original[start:end+1], map[string]string{
			"Content-Range": fmt.Sprintf("bytes %d-%d/%d", start, end, declaredTotal),
			"Content-Type":  "application/octet-stream",
		})
	}

	// ===== chunk 1: bytes 0-999 -> 202 with nextExpectedRanges ["1000-"] =====

	status, body = putChunk(0, 999, total)
	if status != 202 {
		t.Fatalf("chunk 1 -> status %d, want 202; body %s", status, body)
	}
	mid := graphJSON(t, body)
	ranges, _ := mid["nextExpectedRanges"].([]any)
	if len(ranges) != 1 || ranges[0] != "1000-" {
		t.Fatalf("nextExpectedRanges = %v, want [\"1000-\"]", mid["nextExpectedRanges"])
	}
	if _, ok := mid["expirationDateTime"].(string); !ok {
		t.Fatalf("202 lacks expirationDateTime: %s", body)
	}

	// ===== violations -> 416 =====

	// Out-of-order: resending the first chunk when offset 1000 is expected.
	status, _ = putChunk(0, 999, total)
	if status != 416 {
		t.Fatalf("out-of-order chunk -> status %d, want 416", status)
	}
	// Gap: skipping ahead.
	status, _ = putChunk(2000, 2499, total)
	if status != 416 {
		t.Fatalf("gap chunk -> status %d, want 416", status)
	}
	// Inconsistent total across chunks.
	status, _ = graphDo(t, "PUT", uploadURL, "", original[1000:2000], map[string]string{
		"Content-Range": fmt.Sprintf("bytes 1000-1999/%d", total+7),
	})
	if status != 416 {
		t.Fatalf("inconsistent total -> status %d, want 416", status)
	}
	// end < start.
	status, _ = graphDo(t, "PUT", uploadURL, "", []byte("x"), map[string]string{
		"Content-Range": fmt.Sprintf("bytes 1000-999/%d", total),
	})
	if status != 416 {
		t.Fatalf("end < start -> status %d, want 416", status)
	}
	// Malformed header -> 400.
	status, _ = graphDo(t, "PUT", uploadURL, "", []byte("x"), map[string]string{
		"Content-Range": "bytes nonsense",
	})
	if status != 400 {
		t.Fatalf("malformed Content-Range -> status %d, want 400", status)
	}

	// ===== chunk 2: bytes 1000-1999 -> 202, next 2000 =====

	status, body = putChunk(1000, 1999, total)
	if status != 202 {
		t.Fatalf("chunk 2 -> status %d, want 202; body %s", status, body)
	}
	mid = graphJSON(t, body)
	ranges, _ = mid["nextExpectedRanges"].([]any)
	if len(ranges) != 1 || ranges[0] != "2000-" {
		t.Fatalf("nextExpectedRanges after chunk 2 = %v, want [\"2000-\"]", mid["nextExpectedRanges"])
	}

	// ===== final chunk: bytes 2000-2499 -> 201 + driveItem =====

	status, body = putChunk(2000, total-1, total)
	if status != 201 {
		t.Fatalf("final chunk -> status %d, want 201; body %s", status, body)
	}
	item := graphJSON(t, body)
	itemID, _ := item["id"].(string)
	if itemID == "" {
		t.Fatalf("final driveItem id missing: %s", body)
	}
	if item["name"] != "big.bin" {
		t.Fatalf("final driveItem name = %v, want big.bin", item["name"])
	}
	if int64(item["size"].(float64)) != int64(total) {
		t.Fatalf("final driveItem size = %v, want %d", item["size"], total)
	}

	// ===== chunks after completion are rejected =====

	status, _ = putChunk(0, 999, total)
	if status != 404 {
		t.Fatalf("chunk after completion -> status %d, want 404 (session gone)", status)
	}

	// ===== assembled content round-trips byte-exact =====

	status, got := graphDo(t, "GET", base+"/v1.0/me/drive/items/"+itemID+"/content", graphToken, nil, nil)
	if status != 200 {
		t.Fatalf("get assembled content -> status %d, want 200", status)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("assembled content not byte-equal: got %d bytes, want %d", len(got), len(original))
	}

	// ===== unknown session -> 404 =====

	status, _ = graphDo(t, "PUT", base+"/v1.0/_upload/sess-does-not-exist", "", []byte("x"), map[string]string{
		"Content-Range": "bytes 0-0/1",
	})
	if status != 404 {
		t.Fatalf("unknown session -> status %d, want 404", status)
	}
}

// TestGraphDriveSessionInFolder proves the items/{parentId}:/{name}: session
// variant: the final driveItem lands in the folder.
func TestGraphDriveSessionInFolder(t *testing.T) {
	base := bootGraphService(t, 0)

	folderBody, _ := json.Marshal(map[string]any{"name": "Uploads", "folder": map[string]any{}})
	status, body := graphDo(t, "POST", base+"/v1.0/me/drive/root/children", graphToken,
		folderBody, map[string]string{"Content-Type": "application/json"})
	if status != 201 {
		t.Fatalf("createFolder -> status %d, want 201; body %s", status, body)
	}
	folderID, _ := graphJSON(t, body)["id"].(string)

	status, body = graphDo(t, "POST",
		base+"/v1.0/me/drive/items/"+folderID+":/video.bin:/createUploadSession",
		graphToken, []byte("{}"), map[string]string{"Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("createUploadSession in folder -> status %d, want 200; body %s", status, body)
	}
	uploadURL, _ := graphJSON(t, body)["uploadUrl"].(string)
	if uploadURL == "" {
		t.Fatalf("uploadUrl missing: %s", body)
	}

	payload := []byte("single-chunk-session-payload")
	status, body = graphDo(t, "PUT", uploadURL, "", payload, map[string]string{
		"Content-Range": fmt.Sprintf("bytes 0-%d/%d", len(payload)-1, len(payload)),
	})
	if status != 201 {
		t.Fatalf("single final chunk -> status %d, want 201; body %s", status, body)
	}
	item := graphJSON(t, body)
	parentRef, _ := item["parentReference"].(map[string]any)
	if parentRef == nil || parentRef["id"] != folderID {
		t.Fatalf("session item parentReference = %v, want id %v", item["parentReference"], folderID)
	}

	itemID, _ := item["id"].(string)
	status, got := graphDo(t, "GET", base+"/v1.0/me/drive/items/"+itemID+"/content", graphToken, nil, nil)
	if status != 200 || !bytes.Equal(got, payload) {
		t.Fatalf("folder session content mismatch: status %d", status)
	}
}

// TestGraphDriveOversizeBody413 proves a small max_body_bytes turns oversize
// simple uploads into a 413 instead of silent truncation.
func TestGraphDriveOversizeBody413(t *testing.T) {
	base := bootGraphService(t, 1024)

	status, _ := graphDo(t, "PUT", base+"/v1.0/me/drive/root:/big.bin:/content", graphToken,
		bytes.Repeat([]byte{0x5A}, 2048), nil)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize simple upload -> status %d, want 413", status)
	}
}

// TestGraphResumableUpload_ConcurrentRetryIs416 proves the per-session
// serialization (concurrency_key: session) closes the TOCTOU in on_upload_chunk.
// N concurrent PUTs of the SAME chunk (bytes 0-999) at the expected offset must
// yield exactly one 202 (the winner advances next) and N-1 416s (losers re-read
// next, now advanced, and reject the stale offset). Pre-fix this was a race:
// both read next=0, both passed validation, both concat-put — last write won,
// corrupting the assembled blob.
func TestGraphResumableUpload_ConcurrentRetryIs416(t *testing.T) {
	base := bootGraphService(t, 0)
	original := allByteValues(2500)
	total := len(original)

	sessBody, _ := json.Marshal(map[string]any{
		"item": map[string]any{"@microsoft.graph.conflictBehavior": "rename", "name": "race.bin"},
	})
	status, body := graphDo(t, "POST", base+"/v1.0/me/drive/root:/race.bin:/createUploadSession",
		graphToken, sessBody, map[string]string{"Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("createUploadSession -> %d: %s", status, body)
	}
	uploadURL, _ := graphJSON(t, body)["uploadUrl"].(string)

	putFirstChunk := func() (int, []byte) {
		return graphDo(t, "PUT", uploadURL, "", original[0:1000], map[string]string{
			"Content-Range": fmt.Sprintf("bytes 0-999/%d", total),
			"Content-Type":  "application/octet-stream",
		})
	}

	const n = 20
	results := make(chan int, n)
	var wg sync.WaitGroup
	var barrier sync.WaitGroup
	barrier.Add(1)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			barrier.Wait() // maximize overlap so the race window is wide
			st, _ := putFirstChunk()
			results <- st
		}()
	}
	barrier.Done()
	wg.Wait()
	close(results)

	var ok202, err416, other int
	for s := range results {
		switch s {
		case http.StatusAccepted: // 202 — the winner
			ok202++
		case http.StatusRequestedRangeNotSatisfiable: // 416 — stale-offset loser
			err416++
		default:
			other++
		}
	}
	// Exactly one winner (202); every other contender must be 416. More than
	// one 202 is the TOCTOU — the lock is missing or not held.
	if ok202 != 1 || err416 != n-1 || other != 0 {
		t.Fatalf("concurrent same-chunk PUTs: %d×202, %d×416, %d×other — want 1×202 and %d×416 (TOCTOU if >1 winner)", ok202, err416, other, n-1)
	}

	// The session is still usable: the next contiguous chunk and the final
	// chunk land, and the assembled content is byte-exact (catches corruption).
	if status, _ = graphDo(t, "PUT", uploadURL, "", original[1000:2000], map[string]string{
		"Content-Range": fmt.Sprintf("bytes 1000-1999/%d", total),
	}); status != 202 {
		t.Fatalf("post-race chunk 2 -> %d, want 202", status)
	}
	status, body = graphDo(t, "PUT", uploadURL, "", original[2000:total], map[string]string{
		"Content-Range": fmt.Sprintf("bytes 2000-%d/%d", total-1, total),
	})
	if status != 201 {
		t.Fatalf("final chunk -> %d, want 201; body %s", status, body)
	}
	itemID, _ := graphJSON(t, body)["id"].(string)
	status, got := graphDo(t, "GET", base+"/v1.0/me/drive/items/"+itemID+"/content", graphToken, nil, nil)
	if status != 200 || !bytes.Equal(got, original) {
		t.Fatalf("assembled content corrupted: status=%d got=%dB want=%dB", status, len(got), total)
	}
}

// sessionIDFromURL extracts the trailing session id from an uploadUrl.
func sessionIDFromURL(t *testing.T, url string) string {
	t.Helper()
	idx := strings.LastIndex(url, "/")
	if idx < 0 {
		t.Fatalf("no '/' in uploadUrl %q", url)
	}
	return url[idx+1:]
}

// firstChunkPUT sends a valid first chunk (bytes 0-99) to uploadURL and fails
// the test unless it is accepted (202), creating the partial blob via the
// adapter's own append path — the contract the GC tests must exercise.
func firstChunkPUT(t *testing.T, uploadURL string, total int) {
	t.Helper()
	status, body := graphDo(t, "PUT", uploadURL, "", allByteValues(total)[0:100], map[string]string{
		"Content-Range": fmt.Sprintf("bytes 0-99/%d", total),
		"Content-Type":  "application/octet-stream",
	})
	if status != 202 {
		t.Fatalf("first chunk -> %d, want 202; body %s", status, body)
	}
}

// TestGraphDriveUploadSessionExpiresOnAccess proves a session past its 48h TTL
// is reaped on next use: the chunk returns 404 and BOTH the session row and the
// partial blob — created by the adapter's own append path — are gone. Uses an
// injected virtual clock so the TTL elapses instantly.
func TestGraphDriveUploadSessionExpiresOnAccess(t *testing.T) {
	vc := clock.NewVirtualClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	base, e := bootGraphServiceWithClock(t, 0, vc)

	const total = 500
	sessBody, _ := json.Marshal(map[string]any{
		"item": map[string]any{"@microsoft.graph.conflictBehavior": "rename", "name": "later.bin"},
	})
	status, body := graphDo(t, "POST", base+"/v1.0/me/drive/root:/later.bin:/createUploadSession",
		graphToken, sessBody, map[string]string{"Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("createUploadSession -> %d: %s", status, body)
	}
	uploadURL, _ := graphJSON(t, body)["uploadUrl"].(string)
	sid := sessionIDFromURL(t, uploadURL)

	firstChunkPUT(t, uploadURL, total) // append-creates up-<sid> via the adapter path
	st := e.states["graph"]
	if _, err := st.blobStore.Get("drive", "up-"+sid); err != nil {
		t.Fatalf("partial blob not created by first chunk: %v", err)
	}

	vc.Advance(49 * time.Hour) // past the 48h TTL

	// A chunk to the now-expired session → 404 (same as a missing session).
	status, _ = graphDo(t, "PUT", uploadURL, "", allByteValues(total)[100:200], map[string]string{
		"Content-Range": fmt.Sprintf("bytes 100-199/%d", total),
	})
	if status != 404 {
		t.Fatalf("chunk to expired session -> %d, want 404", status)
	}

	// Expire-on-access reaped the session row AND the append-created partial blob.
	col, _ := st.store.Collection("sessions")
	if _, err := col.Get(sid); err == nil {
		t.Fatal("session row still present after expiry")
	}
	if _, err := st.blobStore.Get("drive", "up-"+sid); err == nil {
		t.Fatal("partial blob still present after expiry")
	}
}

// TestGraphDriveUploadSessionSweepOnCreate proves creating a new session sweeps
// abandoned ones past TTL (row + the append-created partial blob), an in-window
// session survives, and the session_seq KV counter persists (not reset).
func TestGraphDriveUploadSessionSweepOnCreate(t *testing.T) {
	vc := clock.NewVirtualClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	base, e := bootGraphServiceWithClock(t, 0, vc)

	const total = 500
	create := func(name string) string {
		sessBody, _ := json.Marshal(map[string]any{
			"item": map[string]any{"@microsoft.graph.conflictBehavior": "rename", "name": name},
		})
		status, body := graphDo(t, "POST", base+"/v1.0/me/drive/root:/"+name+":/createUploadSession",
			graphToken, sessBody, map[string]string{"Content-Type": "application/json"})
		if status != 200 {
			t.Fatalf("createUploadSession %s -> %d: %s", name, status, body)
		}
		url, _ := graphJSON(t, body)["uploadUrl"].(string)
		return sessionIDFromURL(t, url)
	}

	sidA := create("a.bin")
	firstChunkPUT(t, base+"/v1.0/_upload/"+sidA, total) // append-creates up-<sidA>

	vc.Advance(49 * time.Hour) // A is now abandoned past TTL
	sidB := create("b.bin")    // triggers _sweep_expired_sessions

	st := e.states["graph"]
	col, _ := st.store.Collection("sessions")
	if _, err := col.Get(sidA); err == nil {
		t.Fatal("abandoned session A row still present after sweep-on-create")
	}
	if _, err := st.blobStore.Get("drive", "up-"+sidA); err == nil {
		t.Fatal("abandoned partial blob A still present after sweep-on-create")
	}
	if _, err := col.Get(sidB); err != nil {
		t.Fatalf("fresh session B missing after sweep: %v", err)
	}
	// session_seq must persist across the sweep (GC never touches KV counters).
	if sidB != "sess-000002" {
		t.Fatalf("session B id = %q, want sess-000002 (session_seq must persist)", sidB)
	}
}

// TestGraphDriveUploadSessionInWindowSurvives pins the TTL value: a session
// advanced LESS than 48h must NOT be reaped (by expire-on-access or by a sweep).
// Without this, a regression that drops the TTL term (reaping every session on
// the next access/sweep) would pass the two tests above.
func TestGraphDriveUploadSessionInWindowSurvives(t *testing.T) {
	vc := clock.NewVirtualClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	base, e := bootGraphServiceWithClock(t, 0, vc)

	const total = 500
	sessBody, _ := json.Marshal(map[string]any{
		"item": map[string]any{"@microsoft.graph.conflictBehavior": "rename", "name": "live.bin"},
	})
	status, body := graphDo(t, "POST", base+"/v1.0/me/drive/root:/live.bin:/createUploadSession",
		graphToken, sessBody, map[string]string{"Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("createUploadSession -> %d: %s", status, body)
	}
	uploadURL, _ := graphJSON(t, body)["uploadUrl"].(string)
	sid := sessionIDFromURL(t, uploadURL)
	firstChunkPUT(t, uploadURL, total)

	vc.Advance(47 * time.Hour) // still within the 48h TTL

	// A sweep triggered by a fresh createUploadSession must leave the in-window
	// session (and its partial blob) intact.
	if status, body := graphDo(t, "POST", base+"/v1.0/me/drive/root:/other.bin:/createUploadSession",
		graphToken, []byte("{}"), map[string]string{"Content-Type": "application/json"}); status != 200 {
		t.Fatalf("sweep-triggering create -> %d: %s", status, body)
	}
	st := e.states["graph"]
	col, _ := st.store.Collection("sessions")
	if _, err := col.Get(sid); err != nil {
		t.Fatal("in-window session row was reaped by the sweep")
	}
	if _, err := st.blobStore.Get("drive", "up-"+sid); err != nil {
		t.Fatal("in-window partial blob was reaped by the sweep")
	}

	// And expire-on-access must still accept a chunk (202), not 404 it.
	status, _ = graphDo(t, "PUT", uploadURL, "", allByteValues(total)[100:200], map[string]string{
		"Content-Range": fmt.Sprintf("bytes 100-199/%d", total),
	})
	if status != 202 {
		t.Fatalf("chunk to in-window session -> %d, want 202", status)
	}
}
