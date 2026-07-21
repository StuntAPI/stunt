// Package gen provides a named generator registry. Adapters register named
// generators (name -> producer function) that return a Go value, later
// marshaled to JSON when resolving `body.generate: <name>` references.
//
// The registry reuses the existing faker (internal/rules.Faker) for
// deterministic data generation. Two builtins are pre-registered:
//
//   - "uuid"      — a deterministic UUID string (via faker.ID("")).
//   - "timestamp" — the current time as Unix seconds (int64).
package gen

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"stuntapi.com/stunt/internal/rules"
)

// ErrUnknownGenerator is returned by Generate when no generator is
// registered for the requested name.
var ErrUnknownGenerator = errors.New("gen: unknown generator")

// Producer is a generator function: given a context map it returns a Go
// value (later marshaled to JSON) or an error.
type Producer func(ctx map[string]any) (any, error)

// Registry holds named generators. It is safe for concurrent use by
// multiple goroutines.
type Registry struct {
	mu      sync.RWMutex
	faker   *rules.Faker
	genFunc map[string]Producer
}

// NewRegistry creates a Registry backed by the given faker and
// pre-registers the "uuid" and "timestamp" builtins. Panics if fk is nil,
// since a nil faker would cause a nil-pointer panic on the first Generate
// call.
func NewRegistry(fk *rules.Faker) *Registry {
	if fk == nil {
		panic("gen: NewRegistry requires a non-nil faker")
	}
	r := &Registry{
		faker:   fk,
		genFunc: make(map[string]Producer),
	}
	r.registerBuiltins()
	return r
}

// Register adds (or overwrites) a named generator. Panics if name is empty
// or fn is nil.
func (r *Registry) Register(name string, fn Producer) {
	if name == "" {
		panic("gen: empty generator name")
	}
	if fn == nil {
		panic("gen: nil generator function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.genFunc[name] = fn
}

// Generate runs the named generator, passing ctx to the producer function.
// Returns ErrUnknownGenerator if name is not registered. Producer errors
// are returned unwrapped so callers may use errors.Is.
func (r *Registry) Generate(name string, ctx map[string]any) (any, error) {
	r.mu.RLock()
	fn, ok := r.genFunc[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("gen: generate %q: %w", name, ErrUnknownGenerator)
	}
	return fn(ctx)
}

// Names returns the sorted names of all registered generators (builtins
// plus any added via Register).
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.genFunc))
	for name := range r.genFunc {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// --- builtins ---

func (r *Registry) registerBuiltins() {
	r.genFunc["uuid"] = func(_ map[string]any) (any, error) {
		return r.faker.ID(""), nil
	}
	// timestamp returns the current wall-clock time as Unix seconds.
	// Unlike the "uuid" builtin (which is derived from the seeded faker and
	// therefore deterministic), timestamp uses time.Now() and is inherently
	// non-deterministic. This is acceptable for its semantic meaning ("the
	// time this value was generated") but callers should not rely on it for
	// reproducible output.
	r.genFunc["timestamp"] = func(_ map[string]any) (any, error) {
		return time.Now().Unix(), nil
	}
}

// nowUnix is a test helper exposed for assertions in the timestamp test.
func nowUnix() int64 { return time.Now().Unix() }
