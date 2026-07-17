package kv

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestIncrBasic(t *testing.T) {
	k, err := Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil { t.Fatal(err) }
	defer k.Close()
	for i, want := 1, 1; want <= 5; i, want = i+1, want+1 {
		got, err := k.Incr("ns", "c")
		if err != nil { t.Fatalf("incr: %v", err) }
		if got != want { t.Fatalf("incr #%d = %d, want %d", i, got, want) }
	}
}

func TestIncrConcurrentUnique(t *testing.T) {
	k, err := Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil { t.Fatal(err) }
	defer k.Close()
	const N = 200
	var wg sync.WaitGroup
	results := make([]int, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := k.Incr("ns", "c")
			if err != nil { t.Errorf("incr: %v", err); return }
			results[i] = v
		}(i)
	}
	wg.Wait()
	seen := map[int]bool{}
	for _, v := range results {
		if v < 1 || v > N { t.Errorf("value %d out of range", v) }
		if seen[v] { t.Errorf("DUPLICATE counter value %d (race)", v) }
		seen[v] = true
	}
	if len(seen) != N { t.Errorf("expected %d unique values, got %d", N, len(seen)) }
}
