package runtime

import (
	"path/filepath"
	"testing"

	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// TestStoreKVSetAcceptsNonString verifies that store_kv_set accepts any
// value type (int, bool, etc.) and stores it as its string representation.
// This prevents the confusing "got int, want string" error for beginners.
func TestStoreKVSetAcceptsNonString(t *testing.T) {
	dir := t.TempDir()
	store, err := primitives.Open(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	kvstore, err := kv.Open(filepath.Join(dir, "s.kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer kvstore.Close()
	blobStore, err := blob.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	defer blobStore.Close()

	// A handler that sets a counter to an int value, then reads it back.
	src := `
def on_get(req):
    store_kv_set("svc", "counter", 42)
    val = store_kv_get("svc", "counter")
    return respond(200, {"val": val})
`
	builtins := BuildBuiltins(store, kvstore, blobStore)
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	resp, err := vm.Call("on_get", starlark.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	val, ok := resp.Body["val"]
	if !ok {
		t.Fatalf("missing val: %+v", resp.Body)
	}
	if val != "42" {
		t.Errorf("val = %v (%T), want \"42\"", val, val)
	}
}

// TestStoreKVSetAcceptsBool verifies that boolean values are stored as
// "True"/"False" (matching Starlark's str() convention).
func TestStoreKVSetAcceptsBool(t *testing.T) {
	dir := t.TempDir()
	store, err := primitives.Open(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	kvstore, err := kv.Open(filepath.Join(dir, "s.kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer kvstore.Close()
	blobStore, err := blob.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	defer blobStore.Close()

	src := `
def on_get(req):
    store_kv_set("svc", "flag", True)
    val = store_kv_get("svc", "flag")
    return respond(200, {"val": val})
`
	builtins := BuildBuiltins(store, kvstore, blobStore)
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	resp, err := vm.Call("on_get", starlark.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	val, ok := resp.Body["val"]
	if !ok {
		t.Fatalf("missing val: %+v", resp.Body)
	}
	if val != "True" {
		t.Errorf("val = %v (%T), want \"True\"", val, val)
	}
}
