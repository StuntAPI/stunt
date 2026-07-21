package gen

import (
	"errors"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/rules"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	return NewRegistry(rules.NewFaker(42))
}

func TestBuiltinUUID(t *testing.T) {
	r := newTestRegistry(t)

	v, err := r.Generate("uuid", nil)
	if err != nil {
		t.Fatalf("Generate(uuid): %v", err)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("uuid = %T, want string", v)
	}
	// A bare uuid has no prefix separator (fk.ID("") returns a bare uuid).
	if strings.Contains(s, "_") {
		t.Fatalf("uuid %q should not contain an underscore", s)
	}
	if len(s) != 36 {
		t.Fatalf("uuid %q has length %d, want 36 (canonical uuid)", s, len(s))
	}
}

func TestBuiltinUUIDDeterministic(t *testing.T) {
	// Same seed → same first uuid.
	r1 := NewRegistry(rules.NewFaker(99))
	r2 := NewRegistry(rules.NewFaker(99))
	a, _ := r1.Generate("uuid", nil)
	b, _ := r2.Generate("uuid", nil)
	if a != b {
		t.Fatalf("uuid not deterministic: %v != %v", a, b)
	}
}

func TestBuiltinTimestamp(t *testing.T) {
	r := newTestRegistry(t)

	before := nowUnix()
	v, err := r.Generate("timestamp", nil)
	if err != nil {
		t.Fatalf("Generate(timestamp): %v", err)
	}
	ts, ok := v.(int64)
	if !ok {
		t.Fatalf("timestamp = %T, want int64", v)
	}
	after := nowUnix()
	if ts < before || ts > after {
		t.Fatalf("timestamp %d outside [%d, %d]", ts, before, after)
	}
}

func TestRegisterAndGenerate(t *testing.T) {
	r := newTestRegistry(t)

	r.Register("greeting", func(ctx map[string]any) (any, error) {
		name, _ := ctx["name"].(string)
		return "hello " + name, nil
	})

	v, err := r.Generate("greeting", map[string]any{"name": "world"})
	if err != nil {
		t.Fatalf("Generate(greeting): %v", err)
	}
	if v != "hello world" {
		t.Fatalf("greeting = %v, want \"hello world\"", v)
	}
}

func TestRegisterOverwrites(t *testing.T) {
	r := newTestRegistry(t)

	r.Register("x", func(ctx map[string]any) (any, error) { return "first", nil })
	r.Register("x", func(ctx map[string]any) (any, error) { return "second", nil })

	v, err := r.Generate("x", nil)
	if err != nil {
		t.Fatalf("Generate(x): %v", err)
	}
	if v != "second" {
		t.Fatalf("x = %v, want \"second\" (overwrite)", v)
	}
}

func TestGenerateUnknown(t *testing.T) {
	r := newTestRegistry(t)

	_, err := r.Generate("nope", nil)
	if !errors.Is(err, ErrUnknownGenerator) {
		t.Fatalf("Generate(unknown): err = %v, want ErrUnknownGenerator", err)
	}
}

func TestGenerateProducerError(t *testing.T) {
	r := newTestRegistry(t)

	boom := errors.New("boom")
	r.Register("fail", func(ctx map[string]any) (any, error) { return nil, boom })

	_, err := r.Generate("fail", nil)
	if !errors.Is(err, boom) {
		t.Fatalf("Generate(fail): err = %v, want boom", err)
	}
}

func TestNames(t *testing.T) {
	r := newTestRegistry(t)

	// Builtins are present.
	names := r.Names()
	if !contains(names, "uuid") {
		t.Errorf("Names() missing uuid: %v", names)
	}
	if !contains(names, "timestamp") {
		t.Errorf("Names() missing timestamp: %v", names)
	}

	// After registering a custom generator, it appears too.
	r.Register("custom", func(ctx map[string]any) (any, error) { return nil, nil })
	names = r.Names()
	if !contains(names, "custom") {
		t.Errorf("Names() missing custom: %v", names)
	}
}

func TestNilContextAllowed(t *testing.T) {
	r := newTestRegistry(t)
	// A producer that tolerates nil ctx should work fine.
	r.Register("ok", func(ctx map[string]any) (any, error) {
		if ctx != nil {
			t.Errorf("expected nil ctx, got %v", ctx)
		}
		return 42, nil
	})
	v, err := r.Generate("ok", nil)
	if err != nil {
		t.Fatalf("Generate(ok): %v", err)
	}
	if v != 42 {
		t.Fatalf("ok = %v, want 42", v)
	}
}

func TestRegisterEmptyName(t *testing.T) {
	r := newTestRegistry(t)

	defer func() {
		if recover() == nil {
			t.Fatal("Register(\"\") should panic")
		}
	}()
	r.Register("", func(ctx map[string]any) (any, error) { return nil, nil })
}

func TestRegisterNilFunc(t *testing.T) {
	r := newTestRegistry(t)

	defer func() {
		if recover() == nil {
			t.Fatal("Register(nil) should panic")
		}
	}()
	r.Register("x", nil)
}

// TestNewRegistryNilFakerPanics verifies that NewRegistry panics early with a
// clear message when fk is nil, rather than deferring the panic to the first
// Generate call.
func TestNewRegistryNilFakerPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewRegistry(nil) should panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "nil") && !strings.Contains(msg, "faker") {
			t.Fatalf("panic message should mention nil faker, got: %v", r)
		}
	}()
	NewRegistry(nil)
}

// --- helpers ---

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
