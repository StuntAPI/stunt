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
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestAnaplanStyleAdapter exercises the anaplan-style adapter:
//
//   - Auth required: 401 without auth
//   - List workspaces → {meta, items}
//   - List models → items
//   - Run import → async task ID (CREATED)
//   - Task status immediately → pre-terminal (NOT_STARTED/IN_PROGRESS)
//   - Get task status after the 3s window → COMPLETE
//   - simulate_fail import → COMPLETE with result.successful=false
//   - List exports
func TestAnaplanStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "anaplan-style")
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
			"anaplan": {Adapter: absAdapterDir},
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

	base := addrs["anaplan"]
	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("test@example.com:password123"))
	wsID := "8a819c8645a0aa8e0005c715c7ad49b9"

	// ===== 401 without auth =====

	_, status := anaplanGet(t, base+"/2/0/workspaces", "")
	if status != 401 {
		t.Fatalf("workspaces without auth -> status %d, want 401", status)
	}

	// ===== List workspaces =====

	body, status := anaplanGet(t, base+"/2/0/workspaces", basicAuth)
	if status != 200 {
		t.Fatalf("workspaces -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	items, ok := resp["items"].([]any)
	if !ok || len(items) < 1 {
		t.Fatalf("items = %v, want non-empty array", resp["items"])
	}
	meta, ok := resp["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta = %v, want object", resp["meta"])
	}
	paging, ok := meta["paging"].(map[string]any)
	if !ok {
		t.Fatalf("paging = %v, want object", meta["paging"])
	}
	if _, ok := paging["totalSize"]; !ok {
		t.Fatalf("paging.totalSize missing")
	}

	// ===== List models =====

	body, status = anaplanGet(t, base+"/2/0/workspaces/"+wsID+"/models", basicAuth)
	if status != 200 {
		t.Fatalf("models -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal models: %v (body %s)", err, body)
	}
	items = resp["items"].([]any)
	if len(items) < 1 {
		t.Fatalf("models count = %d, want >= 1", len(items))
	}
	modelID := items[0].(map[string]any)["id"].(string)

	// ===== Run import → async task =====

	body, status = anaplanPost(t, base+"/2/0/workspaces/"+wsID+"/models/"+modelID+"/imports/imp001/tasks", basicAuth, nil)
	if status != 200 {
		t.Fatalf("run import -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal import: %v (body %s)", err, body)
	}
	taskObj, ok := resp["task"].(map[string]any)
	if !ok {
		t.Fatalf("task = %v, want object", resp["task"])
	}
	taskID, ok := taskObj["taskId"].(string)
	if !ok || taskID == "" {
		t.Fatalf("taskId = %v, want non-empty string", taskObj["taskId"])
	}
	if taskObj["taskState"] != "CREATED" {
		t.Fatalf("taskState = %v, want CREATED", taskObj["taskState"])
	}

	// ===== Run import with simulate_fail → unsuccessful COMPLETE =====

	body, status = anaplanPost(t, base+"/2/0/workspaces/"+wsID+"/models/"+modelID+"/imports/imp001/tasks", basicAuth, map[string]any{
		"simulate_fail": true,
	})
	if status != 200 {
		t.Fatalf("run fail import -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal fail import: %v (body %s)", err, body)
	}
	failTaskObj, ok := resp["task"].(map[string]any)
	if !ok {
		t.Fatalf("fail task = %v, want object", resp["task"])
	}
	failTaskID, ok := failTaskObj["taskId"].(string)
	if !ok || failTaskID == "" {
		t.Fatalf("fail taskId = %v, want non-empty string", failTaskObj["taskId"])
	}

	// ===== Task status immediately → pre-terminal =====

	body, status = anaplanGet(t, base+"/2/0/workspaces/"+wsID+"/models/"+modelID+"/tasks/"+taskID, basicAuth)
	if status != 200 {
		t.Fatalf("task status (early) -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal early task: %v (body %s)", err, body)
	}
	if early, _ := resp["taskState"].(string); early == "COMPLETE" {
		t.Fatalf("early taskState = %v, want NOT_STARTED/IN_PROGRESS", early)
	}

	// Sleep past the simulated task window (1s running / 3s done).
	time.Sleep(3200 * time.Millisecond)

	// ===== Get task status → COMPLETE =====

	body, status = anaplanGet(t, base+"/2/0/workspaces/"+wsID+"/models/"+modelID+"/tasks/"+taskID, basicAuth)
	if status != 200 {
		t.Fatalf("task status -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal task: %v (body %s)", err, body)
	}
	if resp["taskState"] != "COMPLETE" {
		t.Fatalf("taskState = %v, want COMPLETE", resp["taskState"])
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want object", resp["result"])
	}
	if result["successful"] != true {
		t.Fatalf("successful = %v, want true", result["successful"])
	}

	// ===== Fail-injected task → COMPLETE with unsuccessful result =====

	body, status = anaplanGet(t, base+"/2/0/workspaces/"+wsID+"/models/"+modelID+"/tasks/"+failTaskID, basicAuth)
	if status != 200 {
		t.Fatalf("fail task status -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal fail task: %v (body %s)", err, body)
	}
	if resp["taskState"] != "COMPLETE" {
		t.Fatalf("fail taskState = %v, want COMPLETE", resp["taskState"])
	}
	failResult, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("fail result = %v, want object", resp["result"])
	}
	if failResult["successful"] != false {
		t.Fatalf("fail successful = %v, want false", failResult["successful"])
	}

	// ===== List exports =====

	body, status = anaplanGet(t, base+"/2/0/workspaces/"+wsID+"/models/"+modelID+"/exports", basicAuth)
	if status != 200 {
		t.Fatalf("exports -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal exports: %v (body %s)", err, body)
	}
	items = resp["items"].([]any)
	if len(items) < 1 {
		t.Fatalf("exports count = %d, want >= 1", len(items))
	}
}

// === Anaplan test helpers ===

func anaplanGet(t *testing.T, rawurl, auth string) (string, int) {
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

func anaplanPost(t *testing.T, rawurl, auth string, body map[string]any) (string, int) {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest("POST", rawurl, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
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

// anaplanUpload sends a raw-body upload (POST or PUT) with an optional
// Content-Range header.
func anaplanUpload(t *testing.T, method, rawurl, auth, contentRange, body string) (string, int) {
	t.Helper()
	req, err := http.NewRequest(method, rawurl, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/csv")
	if contentRange != "" {
		req.Header.Set("Content-Range", contentRange)
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

// TestAnaplanStyleFileCycleAndJobs covers the file upload cycle (full body,
// Content-Range chunks, download, chunk listing), import/export jobs that
// read/apply/render the file content, and the collection-backed catalog
// lists.
func TestAnaplanStyleFileCycleAndJobs(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "anaplan-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"anaplan": {Adapter: adapterDir},
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

	base := addrs["anaplan"]
	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("test@example.com:password123"))
	wsID := "8a819c8645a0aa8e0005c715c7ad49b9"
	modelPath := base + "/2/0/workspaces/" + wsID + "/models/A101"

	// Import file id, assembled the way the adapter seeds it.
	importFileID := "113" + "000" + "001"
	exportFileID := "114" + "000" + "002"

	// ===== Catalog lists come from the collections =====
	body, status := anaplanGet(t, modelPath+"/imports", basicAuth)
	if status != 200 {
		t.Fatalf("imports list -> %d; body %s", status, body)
	}
	var resp map[string]any
	json.Unmarshal([]byte(body), &resp)
	imports := resp["items"].([]any)
	if len(imports) < 2 {
		t.Fatalf("imports = %d, want >= 2", len(imports))
	}
	if imports[0].(map[string]any)["id"] != "imp001" {
		t.Fatalf("first import id = %v, want imp001", imports[0])
	}

	for _, catalog := range []string{"exports", "actions", "processes"} {
		body, status = anaplanGet(t, modelPath+"/"+catalog, basicAuth)
		if status != 200 {
			t.Fatalf("%s list -> %d; body %s", catalog, status, body)
		}
		json.Unmarshal([]byte(body), &resp)
		if items := resp["items"].([]any); len(items) < 1 {
			t.Fatalf("%s list empty", catalog)
		}
	}

	// ===== Files list includes the seeded upload =====
	body, status = anaplanGet(t, modelPath+"/files", basicAuth)
	if status != 200 {
		t.Fatalf("files list -> %d; body %s", status, body)
	}

	// ===== Full-body upload replaces the file; download round-trips =====
	csv := "Region,Product,Revenue\nNorth,Widget,1000\nSouth,Gadget,2000\nEast,Gizmo,3000\nNorth,Widget,400\n"
	_, status = anaplanUpload(t, "POST", modelPath+"/files/"+importFileID, basicAuth, "", csv)
	if status != 200 {
		t.Fatalf("full upload -> %d", status)
	}
	body, status = anaplanGet(t, modelPath+"/files/"+importFileID, basicAuth)
	if status != 200 {
		t.Fatalf("download -> %d; body %s", status, body)
	}
	if body != csv {
		t.Fatalf("download round-trip mismatch: %q", body)
	}

	// Chunk listing after a full upload: one chunk covering the body.
	body, status = anaplanGet(t, modelPath+"/files/"+importFileID+"/chunks", basicAuth)
	if status != 200 {
		t.Fatalf("chunks list -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	chunks := resp["items"].([]any)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}

	// ===== Chunked upload (PUT + Content-Range) =====
	uploadFileID := "113" + "000" + "009"
	chunk1 := "Region,Product,Revenue\nNorth,Widget,1000\n"
	chunk2 := "South,Gadget,2000\nEast,Gizmo,3000\nNorth,Widget,400\n"
	_, status = anaplanUpload(t, "PUT", modelPath+"/files/"+uploadFileID, basicAuth,
		"bytes 0-"+strconv.Itoa(len(chunk1)-1)+"/"+strconv.Itoa(len(chunk1)+len(chunk2)), chunk1)
	if status != 200 {
		t.Fatalf("chunk 1 upload -> %d", status)
	}

	// A gap in the byte sequence is rejected.
	_, status = anaplanUpload(t, "PUT", modelPath+"/files/"+uploadFileID, basicAuth,
		"bytes "+strconv.Itoa(len(chunk1)+2)+"-"+strconv.Itoa(len(chunk1)+len(chunk2)-1)+"/"+strconv.Itoa(len(chunk1)+len(chunk2)), chunk2)
	if status != 400 {
		t.Fatalf("gap chunk -> %d, want 400", status)
	}

	// A malformed Content-Range is rejected.
	_, status = anaplanUpload(t, "PUT", modelPath+"/files/"+uploadFileID, basicAuth, "not-a-range", chunk2)
	if status != 400 {
		t.Fatalf("bad Content-Range -> %d, want 400", status)
	}

	// The contiguous second chunk completes the upload.
	_, status = anaplanUpload(t, "PUT", modelPath+"/files/"+uploadFileID, basicAuth,
		"bytes "+strconv.Itoa(len(chunk1))+"-"+strconv.Itoa(len(chunk1)+len(chunk2)-1)+"/"+strconv.Itoa(len(chunk1)+len(chunk2)), chunk2)
	if status != 200 {
		t.Fatalf("chunk 2 upload -> %d", status)
	}
	body, status = anaplanGet(t, modelPath+"/files/"+uploadFileID, basicAuth)
	if status != 200 {
		t.Fatalf("chunked download -> %d", status)
	}
	if body != chunk1+chunk2 {
		t.Fatalf("chunked download mismatch: %q", body)
	}

	// Chunk listing shows both chunks with offsets.
	body, status = anaplanGet(t, modelPath+"/files/"+uploadFileID+"/chunks", basicAuth)
	if status != 200 {
		t.Fatalf("chunked chunks list -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	chunks = resp["items"].([]any)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	secondChunk := chunks[1].(map[string]any)
	if secondChunk["offset"].(float64) != float64(len(chunk1)) {
		t.Fatalf("chunk 2 offset = %v, want %d", secondChunk["offset"], len(chunk1))
	}

	// Unknown file -> 404 on download and chunks.
	_, status = anaplanGet(t, modelPath+"/files/999999999", basicAuth)
	if status != 404 {
		t.Fatalf("unknown file download -> %d, want 404", status)
	}

	// ===== Import job (the /jobs alias) applies the uploaded rows =====
	body, status = anaplanPost(t, modelPath+"/imports/imp001/jobs", basicAuth, nil)
	if status != 200 {
		t.Fatalf("run import job -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	importTaskID := resp["task"].(map[string]any)["taskId"].(string)

	// Unknown import id -> 404.
	body, status = anaplanPost(t, modelPath+"/imports/impXXX/tasks", basicAuth, nil)
	if status != 404 {
		t.Fatalf("unknown import -> %d, want 404; body %s", status, body)
	}

	// ===== Export job; its output downloads once COMPLETE =====
	body, status = anaplanPost(t, modelPath+"/exports/exp001/jobs", basicAuth, nil)
	if status != 200 {
		t.Fatalf("run export job -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	exportTaskID := resp["task"].(map[string]any)["taskId"].(string)

	// Before the completion window the export file is not yet written.
	if _, status = anaplanGet(t, modelPath+"/files/"+exportFileID, basicAuth); status != 404 {
		t.Fatalf("export file before completion -> %d, want 404", status)
	}

	time.Sleep(3200 * time.Millisecond)

	// Import task result carries the uploaded file's row count (4 rows).
	body, status = anaplanGet(t, modelPath+"/tasks/"+importTaskID, basicAuth)
	if status != 200 {
		t.Fatalf("import task status -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	if resp["taskState"] != "COMPLETE" {
		t.Fatalf("import taskState = %v, want COMPLETE", resp["taskState"])
	}
	result := resp["result"].(map[string]any)
	if result["successful"] != true {
		t.Fatalf("import successful = %v, want true", result["successful"])
	}
	if result["totalCount"].(float64) != 4 {
		t.Fatalf("import totalCount = %v, want 4", result["totalCount"])
	}

	// Export task completes and its file downloads with the applied rows.
	body, status = anaplanGet(t, modelPath+"/tasks/"+exportTaskID, basicAuth)
	if status != 200 {
		t.Fatalf("export task status -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	if resp["taskState"] != "COMPLETE" {
		t.Fatalf("export taskState = %v, want COMPLETE", resp["taskState"])
	}
	body, status = anaplanGet(t, modelPath+"/files/"+exportFileID, basicAuth)
	if status != 200 {
		t.Fatalf("export download -> %d; body %s", status, body)
	}
	if !strings.Contains(body, "North,Widget,1000") || !strings.Contains(body, "East,Gizmo,3000") {
		t.Fatalf("export content missing applied rows: %q", body)
	}
}
