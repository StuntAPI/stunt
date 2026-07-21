package runtime_test

import (
	"path/filepath"
	"testing"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// newStores creates temp-file-backed stores for a test and registers cleanup.
func newStores(t *testing.T) (*primitives.Store, *kv.KV) {
	t.Helper()
	dir := t.TempDir()

	store, err := primitives.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("primitives.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	kvStore, err := kv.Open(filepath.Join(dir, "kv.db"))
	if err != nil {
		t.Fatalf("kv.Open: %v", err)
	}
	t.Cleanup(func() { kvStore.Close() })

	return store, kvStore
}

// TestCollectionInsert proves that a Starlark handler can insert a document
// into a collection and receive back the generated id.
func TestCollectionInsert(t *testing.T) {
	store, _ := newStores(t)
	builtins := runtime.BuildBuiltins(store, nil, nil)

	src := `
def on_post(req):
    c = store_collection("charges")
    id = c.insert({"amount": req["body"]["amount"], "status": "pending"})
    return respond(201, {"id": id})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_post", starlark.Request{
		Method: "POST",
		Body:   map[string]any{"amount": 99.99},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if resp.Status != 201 {
		t.Fatalf("Status = %d, want 201", resp.Status)
	}
	id, ok := resp.Body["id"].(string)
	if !ok || id == "" {
		t.Fatalf("Body[id] = %v, want non-empty string", resp.Body["id"])
	}
}

// TestCollectionGetStateful proves state persists across two separate handler
// calls within one VM: an insert in one call is readable by a get in another.
func TestCollectionGetStateful(t *testing.T) {
	store, _ := newStores(t)
	builtins := runtime.BuildBuiltins(store, nil, nil)

	// First handler: insert and capture the id.
	insertSrc := `
def on_post(req):
    c = store_collection("charges")
    id = c.insert({"amount": req["body"]["amount"]})
    return respond(201, {"id": id})
`
	vm, err := starlark.Load(insertSrc, builtins)
	if err != nil {
		t.Fatalf("Load insert: %v", err)
	}

	resp, err := vm.Call("on_post", starlark.Request{
		Method: "POST",
		Body:   map[string]any{"amount": 42.0},
	})
	if err != nil {
		t.Fatalf("Call on_post: %v", err)
	}
	id := resp.Body["id"].(string)

	// Second handler (same store, new VM): read back the stored document.
	getSrc := `
def on_get(req):
    c = store_collection("charges")
    d = c.get(req["body"]["id"])
    return respond(200, d)
`
	vm2, err := starlark.Load(getSrc, builtins)
	if err != nil {
		t.Fatalf("Load get: %v", err)
	}

	resp2, err := vm2.Call("on_get", starlark.Request{
		Method: "GET",
		Body:   map[string]any{"id": id},
	})
	if err != nil {
		t.Fatalf("Call on_get: %v", err)
	}

	amount, ok := resp2.Body["amount"].(float64)
	if !ok {
		t.Fatalf("Body[amount] = %v (%T), want float64", resp2.Body["amount"], resp2.Body["amount"])
	}
	if amount != 42.0 {
		t.Fatalf("amount = %v, want 42", amount)
	}
}

// TestCollectionListUpdateDelete exercises the remaining collection methods.
func TestCollectionListUpdateDelete(t *testing.T) {
	store, _ := newStores(t)
	builtins := runtime.BuildBuiltins(store, nil, nil)

	src := `
def on_post(req):
    c = store_collection("items")
    action = req["body"]["action"]
    if action == "setup":
        c.insert({"name": "a"})
        c.insert({"name": "b"})
        return respond(200, {"count": len(c.list())})
    if action == "update":
        docs = c.list()
        id = docs[0]["id"]
        c.update(id, {"name": "updated", "extra": True})
        d = c.get(id)
        return respond(200, d)
    if action == "delete":
        docs = c.list()
        id = docs[0]["id"]
        c.delete(id)
        return respond(200, {"count": len(c.list())})
    return respond(400, {"error": "unknown action"})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// setup: insert two, list count should be 2
	resp, err := vm.Call("on_post", starlark.Request{
		Method: "POST",
		Body:   map[string]any{"action": "setup"},
	})
	if err != nil {
		t.Fatalf("Call setup: %v", err)
	}
	if resp.Body["count"] != int64(2) {
		t.Fatalf("count = %v (%T), want 2", resp.Body["count"], resp.Body["count"])
	}

	// update: update first doc, read it back
	resp, err = vm.Call("on_post", starlark.Request{
		Method: "POST",
		Body:   map[string]any{"action": "update"},
	})
	if err != nil {
		t.Fatalf("Call update: %v", err)
	}
	if resp.Body["name"] != "updated" {
		t.Fatalf("name = %v, want updated", resp.Body["name"])
	}
	if resp.Body["extra"] != true {
		t.Fatalf("extra = %v, want true", resp.Body["extra"])
	}

	// delete: delete first remaining doc, count should be 1
	resp, err = vm.Call("on_post", starlark.Request{
		Method: "POST",
		Body:   map[string]any{"action": "delete"},
	})
	if err != nil {
		t.Fatalf("Call delete: %v", err)
	}
	if resp.Body["count"] != int64(1) {
		t.Fatalf("count = %v, want 1", resp.Body["count"])
	}
}

// TestKVSetGet proves that KV set in one handler call is readable in another.
func TestKVSetGet(t *testing.T) {
	_, kvStore := newStores(t)
	builtins := runtime.BuildBuiltins(nil, kvStore, nil)

	src := `
def on_post(req):
    store_kv_set("svc", "k", "v")
    got = store_kv_get("svc", "k")
    return respond(200, {"value": got})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["value"] != "v" {
		t.Fatalf("value = %v, want v", resp.Body["value"])
	}
}

// TestKVCrossCall proves state persists across separate handler calls/VMs.
func TestKVCrossCall(t *testing.T) {
	_, kvStore := newStores(t)
	builtins := runtime.BuildBuiltins(nil, kvStore, nil)

	setSrc := `
def on_post(req):
    store_kv_set("svc", "token", "abc123")
    return respond(201, {"ok": True})
`
	vm, err := starlark.Load(setSrc, builtins)
	if err != nil {
		t.Fatalf("Load set: %v", err)
	}
	_, err = vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("Call set: %v", err)
	}

	getSrc := `
def on_get(req):
    v = store_kv_get("svc", "token")
    return respond(200, {"token": v})
`
	vm2, err := starlark.Load(getSrc, builtins)
	if err != nil {
		t.Fatalf("Load get: %v", err)
	}
	resp, err := vm2.Call("on_get", starlark.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("Call get: %v", err)
	}
	if resp.Body["token"] != "abc123" {
		t.Fatalf("token = %v, want abc123", resp.Body["token"])
	}
}

// TestKVDelete proves store_kv_delete removes a key.
func TestKVDelete(t *testing.T) {
	_, kvStore := newStores(t)
	builtins := runtime.BuildBuiltins(nil, kvStore, nil)

	src := `
def on_post(req):
    store_kv_set("svc", "ephemeral", "here")
    store_kv_delete("svc", "ephemeral")
    return respond(200, {"done": True})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// Verify from Go side that it's gone.
	if _, err := kvStore.Get("svc", "ephemeral"); err == nil {
		t.Fatal("expected key to be deleted, but Get succeeded")
	}
}

// TestKVGetMissing returns None to the script (not an error).
func TestKVGetMissing(t *testing.T) {
	_, kvStore := newStores(t)
	builtins := runtime.BuildBuiltins(nil, kvStore, nil)

	src := `
def on_get(req):
    v = store_kv_get("svc", "missing")
    if v == None:
        return respond(200, {"found": False})
    return respond(200, {"found": True, "value": v})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	resp, err := vm.Call("on_get", starlark.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["found"] != false {
		t.Fatalf("found = %v, want false", resp.Body["found"])
	}
}

// --- Blob store tests ---

// newBlobStore creates a temp-file-backed blob store for a test and registers
// cleanup.
func newBlobStore(t *testing.T) *blob.Store {
	t.Helper()
	dir := t.TempDir()
	bs, err := blob.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	t.Cleanup(func() { bs.Close() })
	return bs
}

// TestBlobPutGetRoundtrip proves a Starlark handler can put a blob and read
// it back via get.
func TestBlobPutGetRoundtrip(t *testing.T) {
	bs := newBlobStore(t)
	builtins := runtime.BuildBuiltins(nil, nil, bs)

	src := `
def on_post(req):
    b = store_blob("drive")
    id = b.put("report.txt", "hello blob world")
    content = b.get(id)
    return respond(200, {"id": id, "content": content})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["id"] != "report.txt" {
		t.Fatalf("id = %v, want report.txt", resp.Body["id"])
	}
	if resp.Body["content"] != "hello blob world" {
		t.Fatalf("content = %v, want hello blob world", resp.Body["content"])
	}
}

// TestBlobGetStateful proves blob content persists across separate handler
// calls/VMs.
func TestBlobGetStateful(t *testing.T) {
	bs := newBlobStore(t)
	builtins := runtime.BuildBuiltins(nil, nil, bs)

	putSrc := `
def on_post(req):
    b = store_blob("drive")
    b.put("persist.txt", "persisted content")
    return respond(201, {"ok": True})
`
	vm, err := starlark.Load(putSrc, builtins)
	if err != nil {
		t.Fatalf("Load put: %v", err)
	}
	_, err = vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("Call put: %v", err)
	}

	getSrc := `
def on_get(req):
    b = store_blob("drive")
    content = b.get("persist.txt")
    return respond(200, {"content": content})
`
	vm2, err := starlark.Load(getSrc, builtins)
	if err != nil {
		t.Fatalf("Load get: %v", err)
	}
	resp, err := vm2.Call("on_get", starlark.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("Call get: %v", err)
	}
	if resp.Body["content"] != "persisted content" {
		t.Fatalf("content = %v, want persisted content", resp.Body["content"])
	}
}

// TestBlobList proves list returns all blobs in the namespace.
func TestBlobList(t *testing.T) {
	bs := newBlobStore(t)
	builtins := runtime.BuildBuiltins(nil, nil, bs)

	src := `
def on_post(req):
    b = store_blob("drive")
    b.put("a.txt", "aaa")
    b.put("b.txt", "bbb")
    infos = b.list()
    return respond(200, {"count": len(infos)})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	resp, err := vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["count"] != int64(2) {
		t.Fatalf("count = %v, want 2", resp.Body["count"])
	}
}

// TestBlobDelete proves delete removes a blob so a subsequent get returns None.
func TestBlobDelete(t *testing.T) {
	bs := newBlobStore(t)
	builtins := runtime.BuildBuiltins(nil, nil, bs)

	src := `
def on_post(req):
    b = store_blob("drive")
    b.put("temp.txt", "temporary")
    b.delete("temp.txt")
    content = b.get("temp.txt")
    if content == None:
        return respond(200, {"deleted": True})
    return respond(200, {"deleted": False, "content": content})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	resp, err := vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["deleted"] != true {
		t.Fatalf("deleted = %v, want true", resp.Body["deleted"])
	}
}

// TestBlobGetNotFound proves get returns None for a missing blob.
func TestBlobGetNotFound(t *testing.T) {
	bs := newBlobStore(t)
	builtins := runtime.BuildBuiltins(nil, nil, bs)

	src := `
def on_get(req):
    b = store_blob("drive")
    content = b.get("missing.txt")
    if content == None:
        return respond(200, {"found": False})
    return respond(200, {"found": True})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	resp, err := vm.Call("on_get", starlark.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["found"] != false {
		t.Fatalf("found = %v, want false", resp.Body["found"])
	}
}

// TestBlobStat proves stat returns metadata about a blob.
func TestBlobStat(t *testing.T) {
	bs := newBlobStore(t)
	builtins := runtime.BuildBuiltins(nil, nil, bs)

	src := `
def on_post(req):
    b = store_blob("drive")
    b.put("data.bin", "1234567890", content_type="application/octet-stream")
    info = b.stat("data.bin")
    return respond(200, info)
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	resp, err := vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["name"] != "data.bin" {
		t.Fatalf("name = %v, want data.bin", resp.Body["name"])
	}
	if resp.Body["size"] != int64(10) {
		t.Fatalf("size = %v, want 10", resp.Body["size"])
	}
	if resp.Body["content_type"] != "application/octet-stream" {
		t.Fatalf("content_type = %v, want application/octet-stream", resp.Body["content_type"])
	}
}
