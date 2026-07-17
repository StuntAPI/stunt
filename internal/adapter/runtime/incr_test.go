package runtime

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/stunt-adapters/stunt/internal/primitives"
	"github.com/stunt-adapters/stunt/internal/primitives/blob"
	"github.com/stunt-adapters/stunt/internal/primitives/kv"
	"github.com/stunt-adapters/stunt/internal/starlark"
)

// TestStoreKVIncrAtomic guards against the read-modify-write ID race that
// affected adapter id generation. Concurrent store_kv_incr calls must each
// return a distinct, monotonically-increasing value.
func TestStoreKVIncrAtomic(t *testing.T) {
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

	// A handler that returns the next counter value from store_kv_incr.
	src := `
def on_next(req):
    return respond(200, {"n": store_kv_incr("svc", "counter")})
`
	const N = 80
	results := make([]int64, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			builtins := BuildBuiltins(store, kvstore, blobStore)
			vm, err := starlark.Load(src, builtins)
			if err != nil {
				t.Errorf("load: %v", err)
				return
			}
			resp, err := vm.Call("on_next", starlark.Request{Method: "GET"})
			if err != nil {
				t.Errorf("call: %v", err)
				return
			}
			n, ok := resp.Body["n"]
			if !ok {
				t.Errorf("missing n: %+v", resp.Body)
				return
			}
			var iv int64
			switch v := n.(type) {
			case int64:
				iv = v
			case float64:
				iv = int64(v)
			default:
				t.Errorf("n not a number: %T", n)
				return
			}
			results[i] = iv
		}(i)
	}
	wg.Wait()

	seen := map[int64]bool{}
	for _, v := range results {
		if v < 1 || v > int64(N) {
			t.Errorf("value %d out of range", v)
		}
		if seen[v] {
			t.Errorf("DUPLICATE counter value %d (race regressed)", v)
		}
		seen[v] = true
	}
	if got := len(seen); got != N {
		t.Errorf("expected %d unique counter values, got %d", N, got)
	}
}
