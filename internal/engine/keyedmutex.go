package engine

import "sync"

// keyedMutex hands out one mutex per key and removes an entry once its last
// holder releases, so a long-lived service doesn't accumulate one map entry
// per key (e.g. per upload-session id). Used to serialize handler calls that
// share a concurrency_key (declared in the adapter manifest) so a handler's
// read-modify-write across stores runs atomically per key.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*kentry
}

type kentry struct {
	mu sync.Mutex
	n  int // holders waiting + holding
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*kentry)}
}

// acquire locks the per-key mutex and returns a release func. The map entry
// is deleted once the last holder releases.
func (k *keyedMutex) acquire(key string) func() {
	k.mu.Lock()
	e, ok := k.locks[key]
	if !ok {
		e = &kentry{}
		k.locks[key] = e
	}
	e.n++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.n--
		if e.n == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
