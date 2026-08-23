package adapters

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// Drives the anaplan-style adapter scripts directly (lib.star preloaded)
// over a shared store and a VIRTUAL clock: the Basic/Bearer gate, the
// workspace → model catalog with Anaplan paging, the file upload cycle
// (full-body and Content-Range chunks), and the async import/export task
// state machine (CREATED → NOT_STARTED → IN_PROGRESS → COMPLETE at +1s/+3s)
// whose completion applies the import's file to the model data and renders
// the export's output file.

const (
	anaplanHost     = "anaplan-style.test"
	anaplanBearer   = "Bearer anaplan-session-token"
	anaplanWS1      = "8a819c8645a0aa8e0005c715c7ad49b9" // Supply Chain Planning
	anaplanWS2      = "8a819c8645b1bb9f0006c825d8be50c0" // Financial Forecasting
	anaplanImportFn = "113000001"                        // imp001/imp002 upload target
	anaplanExportFn = "114000002"                        // exp001 output file
)

// anaplanBasic is the HTTP Basic form the auth gate accepts structurally.
var anaplanBasic = "Basic " + base64.StdEncoding.EncodeToString([]byte("user@example.test:secret"))

// anaplanFixture is one shared store + virtual clock with a loaded VM per
// handler script (workspaces/models/files/tasks/catalog each get their own
// VM but observe the same collections/kv/blob state, like the engine).
type anaplanFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newAnaplanFixture(t *testing.T, start time.Time) *anaplanFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "anaplan-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	tmp := t.TempDir()
	store, _ := primitives.Open(filepath.Join(tmp, "s.db"))
	t.Cleanup(func() { store.Close() })
	kvStore, _ := kv.Open(filepath.Join(tmp, "s.kv.db"))
	t.Cleanup(func() { kvStore.Close() })
	blobStore, _ := blob.Open(filepath.Join(tmp, "blobs"))
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: events.NewEmitter(),
	})
	load := func(script string) *starlark.VM {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(root, "scripts", script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
		if err != nil {
			t.Fatalf("LoadWithLib %s: %v", script, err)
		}
		return vm
	}
	return &anaplanFixture{t: t, vc: vc, host: anaplanHost, vms: map[string]*starlark.VM{
		"ws": load("workspaces.star"), "models": load("models.star"),
		"files": load("files.star"), "tasks": load("tasks.star"), "catalog": load("catalog.star"),
	}}
}

// call invokes a handler in the given script group. params carries the
// {workspaceId}/{modelId}/... route captures the engine normally extracts.
func (f *anaplanFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// upload drives on_upload_file (POST or PUT) with a raw body and optional
// Content-Range header.
func (f *anaplanFixture) upload(method, ws, mid, fid, raw, contentRange, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	if contentRange != "" {
		headers["Content-Range"] = contentRange
	}
	resp, err := f.vms["files"].Call("on_upload_file", starlark.Request{
		Method: method, Path: anaplanModelPath(ws, mid) + "/files/" + fid, Host: f.host,
		Headers: headers, RawBody: raw,
		Params: map[string]string{"workspaceId": ws, "modelId": mid, "fileId": fid},
	})
	if err != nil {
		f.t.Fatalf("upload %s %s: %v", method, fid, err)
	}
	return resp
}

// anaplanModelPath builds the /2/0/models-scoped route prefix.
func anaplanModelPath(ws, mid string) string {
	return "/2/0/workspaces/" + ws + "/models/" + mid
}

// anaplanNum coerces a response number to float64: collection round-trips
// turn ints into floats, so handlers emit a mix of int64 and float64.
func anaplanNum(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	}
	t.Fatalf("value %v (%T) is not a number", v, v)
	return 0
}

// anaplanItems pulls the items array out of an Anaplan list envelope.
func anaplanItems(t *testing.T, body map[string]any) []any {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items = %v (%T), want array", body["items"], body["items"])
	}
	return items
}

// anaplanPaging pulls the meta.paging block out of a list envelope.
func anaplanPaging(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	meta, ok := body["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta = %v, want object", body["meta"])
	}
	paging, ok := meta["paging"].(map[string]any)
	if !ok {
		t.Fatalf("meta.paging = %v, want object", meta["paging"])
	}
	return paging
}

func TestAnaplanAuthGateAndCatalog(t *testing.T) {
	f := newAnaplanFixture(t, time.Unix(1_750_000_000, 0).UTC())

	// ===== missing or unrecognized credentials draw the 401 failure envelope =====
	// Any scheme-less Authorization header is as good as none; both documented
	// schemes (Basic/Bearer) pass structural validation.
	for _, auth := range []string{"", "Token abcdef"} {
		r := f.call("ws", "on_list_workspaces", "GET", "/2/0/workspaces", nil, nil, nil, auth)
		if r.Status != 401 || r.Body["status"] != "FAILURE" {
			t.Fatalf("auth %q -> %d %v, want 401 FAILURE", auth, r.Status, r.Body)
		}
		if !strings.Contains(r.Body["statusMessage"].(string), "Authentication is required") {
			t.Fatalf("401 statusMessage = %v", r.Body["statusMessage"])
		}
	}
	for _, auth := range []string{anaplanBasic, anaplanBearer} {
		if r := f.call("ws", "on_list_workspaces", "GET", "/2/0/workspaces", nil, nil, nil, auth); r.Status != 200 {
			t.Fatalf("auth %q -> %d, want 200", auth[:6], r.Status)
		}
	}

	// ===== the workspace list is seeded with Anaplan paging metadata =====
	// The self link is pinned to api.anaplan.com regardless of the request
	// Host (asserted as-is: a deliberate deviation).
	r := f.call("ws", "on_list_workspaces", "GET", "/2/0/workspaces", nil, nil, nil, anaplanBasic)
	if r.Status != 200 {
		t.Fatalf("workspaces -> %d: %v", r.Status, r.Body)
	}
	items := anaplanItems(t, r.Body)
	if len(items) != 2 {
		t.Fatalf("workspaces = %d items, want the 2 seeded", len(items))
	}
	first := items[0].(map[string]any)
	if first["id"] != anaplanWS1 || first["name"] != "Supply Chain Planning" || first["active"] != true {
		t.Fatalf("first workspace = %v", first)
	}
	if got := anaplanNum(t, first["size"]); got != 1048576 {
		t.Fatalf("first workspace size = %v, want 1 MiB", got)
	}
	paging := anaplanPaging(t, r.Body)
	if anaplanNum(t, paging["totalSize"]) != 2 || anaplanNum(t, paging["currentPageSize"]) != 2 {
		t.Fatalf("paging = %v", paging)
	}
	if _, has := paging["nextCursor"]; has {
		t.Fatalf("unpaged list carries nextCursor: %v", paging)
	}
	links := r.Body["links"].([]any)
	if links[0].(map[string]any)["href"] != "https://api.anaplan.com/2/0/workspaces" {
		t.Fatalf("self link = %v, want the pinned api.anaplan.com href", links[0])
	}

	// ===== paging walks the opaque offset cursor and rejects a bad token =====
	p1 := f.call("ws", "on_list_workspaces", "GET", "/2/0/workspaces", nil,
		map[string]string{"limit": "1"}, nil, anaplanBasic)
	if got := len(anaplanItems(t, p1.Body)); got != 1 {
		t.Fatalf("limit=1 page = %d items, want 1", got)
	}
	paging = anaplanPaging(t, p1.Body)
	if paging["nextCursor"] != "1" || anaplanNum(t, paging["offset"]) != 0 {
		t.Fatalf("page 1 paging = %v, want nextCursor 1 offset 0", paging)
	}
	p2 := f.call("ws", "on_list_workspaces", "GET", "/2/0/workspaces", nil,
		map[string]string{"limit": "1", "offset": "1"}, nil, anaplanBasic)
	items = anaplanItems(t, p2.Body)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "Financial Forecasting" {
		t.Fatalf("page 2 = %v, want the second workspace", items)
	}
	if paging = anaplanPaging(t, p2.Body); paging["nextCursor"] != nil {
		t.Fatalf("last page paging = %v, want no nextCursor", paging)
	}
	p3 := f.call("ws", "on_list_workspaces", "GET", "/2/0/workspaces", nil,
		map[string]string{"limit": "1", "offset": "2"}, nil, anaplanBasic)
	if got := len(anaplanItems(t, p3.Body)); got != 0 {
		t.Fatalf("page 3 = %d items, want the empty tail page", got)
	}
	bad := f.call("ws", "on_list_workspaces", "GET", "/2/0/workspaces", nil,
		map[string]string{"limit": "1", "offset": "abc"}, nil, anaplanBasic)
	if bad.Status != 400 || bad.Body["statusMessage"] != "Invalid offset parameter." {
		t.Fatalf("offset=abc -> %d %v, want 400 Invalid offset parameter.", bad.Status, bad.Body)
	}

	// ===== models are workspace-scoped and a single model round-trips or 404s =====
	m1 := f.call("models", "on_list_models", "GET", "/2/0/workspaces/"+anaplanWS1+"/models",
		map[string]string{"workspaceId": anaplanWS1}, nil, nil, anaplanBasic)
	items = anaplanItems(t, m1.Body)
	if len(items) != 2 || items[0].(map[string]any)["id"] != "A101" || items[1].(map[string]any)["id"] != "A102" {
		t.Fatalf("ws1 models = %v, want A101 + A102", items)
	}
	if items[0].(map[string]any)["modelType"] != "Production" {
		t.Fatalf("A101 = %v, want modelType Production", items[0])
	}
	m2 := f.call("models", "on_list_models", "GET", "/2/0/workspaces/"+anaplanWS2+"/models",
		map[string]string{"workspaceId": anaplanWS2}, nil, nil, anaplanBearer)
	if got := len(anaplanItems(t, m2.Body)); got != 1 {
		t.Fatalf("ws2 models = %d, want only B201", got)
	}
	one := f.call("models", "on_get_model", "GET", anaplanModelPath(anaplanWS1, "A101"),
		map[string]string{"workspaceId": anaplanWS1, "modelId": "A101"}, nil, nil, anaplanBasic)
	if one.Status != 200 || one.Body["name"] != "Demand Planning Model" || one.Body["id"] != "A101" {
		t.Fatalf("get model -> %d %v", one.Status, one.Body)
	}
	miss := f.call("models", "on_get_model", "GET", anaplanModelPath(anaplanWS1, "A999"),
		map[string]string{"workspaceId": anaplanWS1, "modelId": "A999"}, nil, nil, anaplanBasic)
	if miss.Status != 404 || miss.Body["statusMessage"] != "Model A999 not found" {
		t.Fatalf("unknown model -> %d %v", miss.Status, miss.Body)
	}
	// An unknown workspace yields an empty model list (asserted as-is: the
	// real API 404s on the workspace itself).
	empty := f.call("models", "on_list_models", "GET", "/2/0/workspaces/nosuch/models",
		map[string]string{"workspaceId": "nosuch"}, nil, nil, anaplanBasic)
	if empty.Status != 200 || len(anaplanItems(t, empty.Body)) != 0 {
		t.Fatalf("unknown workspace models -> %d %v, want 200 empty", empty.Status, empty.Body)
	}

	// ===== catalog lists serve the seeded per-model action catalog =====
	cases := []struct {
		group, handler, kind string
		wantID, wantName     string
		n                    int
	}{
		{"catalog", "on_list_imports", "imports", "imp001", "Load Revenue Data", 2},
		{"tasks", "on_list_exports", "exports", "exp001", "Revenue Export", 2},
		{"catalog", "on_list_actions", "actions", "act001", "Copy Revenue Actuals", 1},
		{"catalog", "on_list_processes", "processes", "proc001", "Monthly Close Process", 1},
	}
	for _, c := range cases {
		r := f.call(c.group, c.handler, "GET", anaplanModelPath(anaplanWS1, "A101")+"/"+c.kind,
			map[string]string{"workspaceId": anaplanWS1, "modelId": "A101"}, nil, nil, anaplanBasic)
		if r.Status != 200 {
			t.Fatalf("%s list -> %d: %v", c.kind, r.Status, r.Body)
		}
		items = anaplanItems(t, r.Body)
		if len(items) != c.n {
			t.Fatalf("%s = %d items, want %d", c.kind, len(items), c.n)
		}
		first := items[0].(map[string]any)
		if first["id"] != c.wantID || first["name"] != c.wantName {
			t.Fatalf("%s[0] = %v, want %s/%s", c.kind, first, c.wantID, c.wantName)
		}
	}
	// The exports list carries the export-only format field.
	exp := f.call("tasks", "on_list_exports", "GET", anaplanModelPath(anaplanWS1, "A101")+"/exports",
		map[string]string{"workspaceId": anaplanWS1, "modelId": "A101"}, nil, nil, anaplanBasic)
	if got := anaplanItems(t, exp.Body)[0].(map[string]any)["format"]; got != "CSV" {
		t.Fatalf("export format = %v, want CSV", got)
	}
}

func TestAnaplanFileUploadCycle(t *testing.T) {
	f := newAnaplanFixture(t, time.Unix(1_750_000_000, 0).UTC())
	const fid = "113000777"
	dl := func(id string) starlark.Response {
		return f.call("files", "on_download_file", "GET", anaplanModelPath(anaplanWS1, "A101")+"/files/"+id,
			map[string]string{"workspaceId": anaplanWS1, "modelId": "A101", "fileId": id}, nil, nil, anaplanBasic)
	}
	chunks := func(id string) []any {
		r := f.call("files", "on_list_chunks", "GET", anaplanModelPath(anaplanWS1, "A101")+"/files/"+id+"/chunks",
			map[string]string{"workspaceId": anaplanWS1, "modelId": "A101", "fileId": id}, nil, nil, anaplanBasic)
		if r.Status != 200 {
			t.Fatalf("chunks %s -> %d: %v", id, r.Status, r.Body)
		}
		return anaplanItems(t, r.Body)
	}

	// ===== a full-body upload replaces file content byte-exact on POST and PUT =====
	csv1 := "Region,Product,Revenue\nNorth,Widget,100\n"
	csv2 := "Region,Product,Revenue\nNorth,Widget,200\nSouth,Gadget,300\n"
	if r := f.upload("POST", anaplanWS1, "A101", fid, csv1, "", anaplanBasic); r.Status != 200 {
		t.Fatalf("POST full upload -> %d: %v", r.Status, r.Body)
	}
	if r := dl(fid); r.Status != 200 || r.RawBody != csv1 {
		t.Fatalf("download after POST -> %d %q", r.Status, r.RawBody)
	}
	if r := f.upload("PUT", anaplanWS1, "A101", fid, csv2, "", anaplanBasic); r.Status != 200 {
		t.Fatalf("PUT full upload -> %d: %v", r.Status, r.Body)
	}
	r := dl(fid)
	if r.Status != 200 || r.RawBody != csv2 {
		t.Fatalf("download after PUT -> %d %q, want replaced content", r.Status, r.RawBody)
	}
	if r.Headers["Content-Type"] != "text/csv" {
		t.Fatalf("download Content-Type = %q", r.Headers["Content-Type"])
	}
	// A full-body upload records a single chunk covering the whole body.
	if got := len(chunks(fid)); got != 1 {
		t.Fatalf("chunks after full upload = %d, want 1", got)
	}

	// ===== chunked uploads append in order and reject gaps and malformed ranges =====
	const cfid = "113000009"
	chunk1 := "Region,Product,Revenue\nNorth,Widget,1000\n"
	chunk2 := "South,Gadget,2000\nEast,Gizmo,3000\n"
	total := len(chunk1) + len(chunk2)
	rangeHdr := func(start, end int) string {
		return "bytes " + strconv.Itoa(start) + "-" + strconv.Itoa(end) + "/" + strconv.Itoa(total)
	}
	if r := f.upload("PUT", anaplanWS1, "A101", cfid, chunk1, rangeHdr(0, len(chunk1)-1), anaplanBasic); r.Status != 200 {
		t.Fatalf("chunk 1 -> %d: %v", r.Status, r.Body)
	}
	// A gap in the byte sequence is rejected with the current size.
	gap := f.upload("PUT", anaplanWS1, "A101", cfid, chunk2, rangeHdr(len(chunk1)+2, total-1), anaplanBasic)
	if gap.Status != 400 || !strings.Contains(gap.Body["statusMessage"].(string), strconv.Itoa(len(chunk1))) {
		t.Fatalf("gap chunk -> %d %v, want 400 naming the current size", gap.Status, gap.Body)
	}
	malformed := f.upload("PUT", anaplanWS1, "A101", cfid, chunk2, "not-a-range", anaplanBasic)
	if malformed.Status != 400 || !strings.Contains(malformed.Body["statusMessage"].(string), "Invalid Content-Range") {
		t.Fatalf("malformed Content-Range -> %d %v, want 400", malformed.Status, malformed.Body)
	}
	if r := f.upload("POST", anaplanWS1, "A101", cfid, chunk2, rangeHdr(len(chunk1), total-1), anaplanBasic); r.Status != 200 {
		t.Fatalf("chunk 2 -> %d: %v", r.Status, r.Body)
	}
	if r := dl(cfid); r.Status != 200 || r.RawBody != chunk1+chunk2 {
		t.Fatalf("chunked download -> %d %q, want the concatenated chunks", r.Status, r.RawBody)
	}
	cs := chunks(cfid)
	if len(cs) != 2 {
		t.Fatalf("chunked upload = %d chunks, want 2", len(cs))
	}
	second := cs[1].(map[string]any)
	if second["id"] != "1" || second["name"] != cfid ||
		anaplanNum(t, second["offset"]) != float64(len(chunk1)) ||
		anaplanNum(t, second["size"]) != float64(len(chunk2)) {
		t.Fatalf("chunk 2 metadata = %v", second)
	}
	// The files list exposes the upload with its byte size, scoped to the model.
	lst := f.call("files", "on_list_files", "GET", anaplanModelPath(anaplanWS1, "A101")+"/files",
		map[string]string{"workspaceId": anaplanWS1, "modelId": "A101"}, nil, nil, anaplanBasic)
	if lst.Status != 200 {
		t.Fatalf("files list -> %d: %v", lst.Status, lst.Body)
	}
	sizes := map[string]float64{}
	for _, it := range anaplanItems(t, lst.Body) {
		doc := it.(map[string]any)
		sizes[doc["id"].(string)] = anaplanNum(t, doc["size"])
	}
	if sizes[cfid] != float64(total) || sizes[anaplanImportFn] == 0 {
		t.Fatalf("files list sizes = %v, want upload %d + the seeded import file", sizes, total)
	}
	other := f.call("files", "on_list_files", "GET", anaplanModelPath(anaplanWS1, "A102")+"/files",
		map[string]string{"workspaceId": anaplanWS1, "modelId": "A102"}, nil, nil, anaplanBasic)
	for _, it := range anaplanItems(t, other.Body) {
		if it.(map[string]any)["id"] == cfid {
			t.Fatal("A101 upload leaked into the A102 file list")
		}
	}

	// ===== invalid identifiers and unknown files get 400/404 envelopes =====
	if r := f.upload("POST", anaplanWS1, "A101", "bad/id", "x", "", anaplanBasic); r.Status != 400 || r.Body["statusMessage"] != "Invalid identifier." {
		t.Fatalf("path chars in fileId -> %d %v, want 400 Invalid identifier.", r.Status, r.Body)
	}
	if r := dl("999999999"); r.Status != 404 || r.Body["statusMessage"] != "File 999999999 not found" {
		t.Fatalf("unknown file download -> %d %v", r.Status, r.Body)
	}
	if r := f.call("files", "on_list_chunks", "GET", anaplanModelPath(anaplanWS1, "A101")+"/files/999999999/chunks",
		map[string]string{"workspaceId": anaplanWS1, "modelId": "A101", "fileId": "999999999"}, nil, nil, anaplanBasic); r.Status != 404 {
		t.Fatalf("unknown file chunks -> %d, want 404", r.Status)
	}
}

func TestAnaplanImportExportTaskLifecycle(t *testing.T) {
	base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	f := newAnaplanFixture(t, base)
	ws, mid := anaplanWS1, "A101"
	dl := func(id string) starlark.Response {
		return f.call("files", "on_download_file", "GET", anaplanModelPath(ws, mid)+"/files/"+id,
			map[string]string{"workspaceId": ws, "modelId": mid, "fileId": id}, nil, nil, anaplanBasic)
	}
	poll := func(taskID string) map[string]any {
		r := f.call("tasks", "on_get_task", "GET", anaplanModelPath(ws, mid)+"/tasks/"+taskID,
			map[string]string{"workspaceId": ws, "modelId": mid, "taskId": taskID}, nil, nil, anaplanBasic)
		if r.Status != 200 {
			t.Fatalf("poll %s -> %d: %v", taskID, r.Status, r.Body)
		}
		return r.Body
	}
	run := func(handler, path string, params map[string]string, body map[string]any) string {
		r := f.call("tasks", handler, "POST", anaplanModelPath(ws, mid)+path, params, nil, body, anaplanBasic)
		if r.Status != 200 {
			t.Fatalf("run %s -> %d: %v", path, r.Status, r.Body)
		}
		task := r.Body["task"].(map[string]any)
		if task["taskState"] != "CREATED" {
			t.Fatalf("run %s taskState = %v, want CREATED", path, task["taskState"])
		}
		id, _ := task["taskId"].(string)
		if !strings.HasPrefix(id, "task-") {
			t.Fatalf("taskId = %q, want task-N", id)
		}
		return id
	}
	importParams := func(id string) map[string]string {
		return map[string]string{"workspaceId": ws, "modelId": mid, "importId": id}
	}
	complete := func(taskID string) map[string]any {
		f.vc.Advance(4 * time.Second)
		return poll(taskID)
	}

	csv := "Region,Product,Revenue\nNorth,Widget,1000\nSouth,Gadget,2000\nEast,Gizmo,3000\nNorth,Widget,400\n"

	// ===== an import task walks CREATED → NOT_STARTED → IN_PROGRESS → COMPLETE =====
	// The upload lands before any catalog touch, so the import run is also the
	// seed's first catalog write (the client's file must win over the default).
	if r := f.upload("POST", ws, mid, anaplanImportFn, csv, "", anaplanBasic); r.Status != 200 {
		t.Fatalf("upload import file -> %d: %v", r.Status, r.Body)
	}
	importTask := run("on_run_import", "/imports/imp001/tasks", importParams("imp001"), nil)

	early := poll(importTask)
	if early["taskState"] != "NOT_STARTED" || early["creationTime"] != "2026-04-01T09:00:00Z" {
		t.Fatalf("import at t0 = %v, want NOT_STARTED with the virtual creation time", early)
	}
	if _, has := early["result"]; has {
		t.Fatalf("pre-completion poll carries a result: %v", early)
	}

	f.vc.Advance(2 * time.Second)
	running := poll(importTask)
	if running["taskState"] != "IN_PROGRESS" {
		t.Fatalf("import at +2s = %v, want IN_PROGRESS", running["taskState"])
	}
	if _, has := running["completionTime"]; has {
		t.Fatalf("IN_PROGRESS poll carries completionTime: %v", running)
	}

	done := complete(importTask)
	if done["taskState"] != "COMPLETE" || done["completionTime"] != "2026-04-01T09:00:03Z" {
		t.Fatalf("import at +4s = %v, want COMPLETE at the frozen +3s mark", done)
	}
	result := done["result"].(map[string]any)
	if result["successful"] != true || anaplanNum(t, result["totalCount"]) != 4 ||
		anaplanNum(t, result["successCount"]) != 4 || anaplanNum(t, result["failureCount"]) != 0 {
		t.Fatalf("import result = %v, want 4 uploaded rows all successful", result)
	}
	// The result block is frozen: repolling reproduces it (the stored copy
	// round-trips as floats; compare numerically, as on the wire).
	again := poll(importTask)
	if got := anaplanNum(t, again["result"].(map[string]any)["totalCount"]); got != anaplanNum(t, result["totalCount"]) {
		t.Fatalf("repoll changed the frozen result: %v", again["result"])
	}

	// ===== completion applies the import once and the export renders the model data =====
	// The /jobs alias starts an export against the same machinery as /tasks.
	exportTask := run("on_run_export", "/exports/exp001/jobs",
		map[string]string{"workspaceId": ws, "modelId": mid, "exportId": "exp001"}, nil)
	if r := dl(anaplanExportFn); r.Status != 404 {
		t.Fatalf("export file before completion -> %d, want 404", r.Status)
	}
	expDone := complete(exportTask)
	if expDone["taskState"] != "COMPLETE" || expDone["completionTime"] != "2026-04-01T09:00:09Z" {
		t.Fatalf("export at completion = %v, want COMPLETE at its +3s mark", expDone)
	}
	expResult := expDone["result"].(map[string]any)
	if expResult["successful"] != true || anaplanNum(t, expResult["totalCount"]) != 4 {
		t.Fatalf("export result = %v, want the 4 applied rows", expResult)
	}
	out := dl(anaplanExportFn)
	// The renderer joins header + rows with \n and no trailing newline.
	if out.Status != 200 || out.RawBody != strings.TrimSuffix(csv, "\n") {
		t.Fatalf("export download -> %d %q, want the model data re-rendered", out.Status, out.RawBody)
	}
	// The rendered file is registered in the model's file list.
	lst := f.call("files", "on_list_files", "GET", anaplanModelPath(ws, mid)+"/files",
		map[string]string{"workspaceId": ws, "modelId": mid}, nil, nil, anaplanBasic)
	found := false
	for _, it := range anaplanItems(t, lst.Body) {
		if it.(map[string]any)["id"] == anaplanExportFn {
			found = true
		}
	}
	if !found {
		t.Fatalf("export file missing from the files list: %v", lst.Body)
	}

	// ===== simulate_fail completes with every row failed =====
	failTask := run("on_run_import", "/imports/imp001/tasks", importParams("imp001"), map[string]any{"simulate_fail": true})
	failDone := complete(failTask)
	if failDone["taskState"] != "COMPLETE" {
		t.Fatalf("simulate_fail taskState = %v, want COMPLETE (failure lives in the result)", failDone["taskState"])
	}
	fr := failDone["result"].(map[string]any)
	if fr["successful"] != false || anaplanNum(t, fr["failureCount"]) != 4 || anaplanNum(t, fr["successCount"]) != 0 {
		t.Fatalf("simulate_fail result = %v, want all 4 rows failed", fr)
	}

	// ===== an import over an empty upload reports no contents =====
	if r := f.upload("PUT", ws, mid, anaplanImportFn, "", "", anaplanBasic); r.Status != 200 {
		t.Fatalf("empty upload -> %d: %v", r.Status, r.Body)
	}
	emptyTask := run("on_run_import", "/imports/imp002/jobs", importParams("imp002"), nil)
	emptyDone := complete(emptyTask)
	er := emptyDone["result"].(map[string]any)
	if er["successful"] != false || er["failureReason"] != "No file contents uploaded for this import" {
		t.Fatalf("empty-upload result = %v, want the no-contents failure", er)
	}
	if anaplanNum(t, er["failureCount"]) != 1 {
		t.Fatalf("empty-upload failureCount = %v, want 1", er["failureCount"])
	}
	// The earlier applied rows survive the empty import.
	if out := dl(anaplanExportFn); !strings.Contains(out.RawBody, "North,Widget,1000") {
		t.Fatalf("export after empty import = %q, want the previously applied rows intact", out.RawBody)
	}
}

func TestAnaplanErrorEnvelopesAndScope(t *testing.T) {
	f := newAnaplanFixture(t, time.Unix(1_750_000_000, 0).UTC())
	ws, mid := anaplanWS1, "A101"

	// ===== unknown import, export, and task ids get 404 failure envelopes =====
	for _, tc := range []struct {
		handler, kind, entity, param, label string
	}{
		{"on_run_import", "imports", "impXXX", "importId", "Import"},
		{"on_run_export", "exports", "expXXX", "exportId", "Export"},
	} {
		r := f.call("tasks", tc.handler, "POST", anaplanModelPath(ws, mid)+"/"+tc.kind+"/"+tc.entity+"/tasks",
			map[string]string{"workspaceId": ws, "modelId": mid, tc.param: tc.entity}, nil, nil, anaplanBasic)
		if r.Status != 404 || r.Body["status"] != "FAILURE" {
			t.Fatalf("run unknown %s -> %d %v, want 404 FAILURE", tc.kind, r.Status, r.Body)
		}
		if want := tc.label + " " + tc.entity + " not found"; r.Body["statusMessage"] != want {
			t.Fatalf("unknown %s statusMessage = %v, want %q", tc.kind, r.Body["statusMessage"], want)
		}
	}
	missing := f.call("tasks", "on_get_task", "GET", anaplanModelPath(ws, mid)+"/tasks/task-999",
		map[string]string{"workspaceId": ws, "modelId": mid, "taskId": "task-999"}, nil, nil, anaplanBasic)
	if missing.Status != 404 || missing.Body["statusMessage"] != "Task task-999 not found" {
		t.Fatalf("unknown task -> %d %v", missing.Status, missing.Body)
	}

	// ===== catalog ids resolve only within their model =====
	r := f.call("tasks", "on_run_import", "POST", anaplanModelPath(ws, "ZZZ9")+"/imports/imp001",
		map[string]string{"workspaceId": ws, "modelId": "ZZZ9", "importId": "imp001"}, nil, nil, anaplanBasic)
	if r.Status != 404 {
		t.Fatalf("imp001 under an unseeded model -> %d, want 404", r.Status)
	}

	// ===== task status is scoped to its workspace and model =====
	created := f.call("tasks", "on_run_import", "POST", anaplanModelPath(ws, mid)+"/imports/imp001/tasks",
		map[string]string{"workspaceId": ws, "modelId": mid, "importId": "imp001"}, nil, nil, anaplanBasic)
	taskID := created.Body["task"].(map[string]any)["taskId"].(string)
	for _, scope := range []struct{ w, m string }{
		{anaplanWS1, "A102"}, {anaplanWS2, "B201"},
	} {
		r := f.call("tasks", "on_get_task", "GET", anaplanModelPath(scope.w, scope.m)+"/tasks/"+taskID,
			map[string]string{"workspaceId": scope.w, "modelId": scope.m, "taskId": taskID}, nil, nil, anaplanBasic)
		if r.Status != 404 {
			t.Fatalf("poll under %s/%s -> %d, want 404 (task belongs to A101)", scope.m, scope.w, r.Status)
		}
	}
	own := f.call("tasks", "on_get_task", "GET", anaplanModelPath(ws, mid)+"/tasks/"+taskID,
		map[string]string{"workspaceId": ws, "modelId": mid, "taskId": taskID}, nil, nil, anaplanBasic)
	if own.Status != 200 || own.Body["taskState"] != "NOT_STARTED" {
		t.Fatalf("poll under the owning model -> %d %v, want 200 NOT_STARTED", own.Status, own.Body)
	}
}
