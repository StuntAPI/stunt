package starlark

import (
	"math/big"
	"testing"
	"time"

	sk "go.starlark.net/starlark"
)

// This file tests the Go <-> Starlark value conversion layer for
// correctness (lossless round-trips) and safety (errors on unsupported
// types and integer overflow rather than silent corruption).

// roundTrip asserts that GoToStarlark(v) → ValueToGo produces the original
// value (or its moral equivalent).
func roundTrip(t *testing.T, v any, want any) {
	t.Helper()
	sv, err := GoToStarlark(v)
	if err != nil {
		t.Fatalf("GoToStarlark(%T %v): unexpected error: %v", v, v, err)
	}
	got, err := ValueToGo(sv)
	if err != nil {
		t.Fatalf("ValueToGo: unexpected error: %v", err)
	}
	// Compare via a deep-equality check that handles numeric type coercion.
	if !valuesEqual(want, got) {
		t.Errorf("round-trip %T: got %v (%T), want %v (%T)", v, got, got, want, want)
	}
}

// valuesEqual checks equality across numeric type coercion (int vs int64
// vs float64) and deep equality for compound types. Starlark ints come back
// as int64, floats as float64.
func valuesEqual(a, b any) bool {
	switch av := a.(type) {
	case int:
		if bv, ok := b.(int64); ok {
			return int64(av) == bv
		}
		if bv, ok := b.(int); ok {
			return av == bv
		}
	case int64:
		if bv, ok := b.(int); ok {
			return av == int64(bv)
		}
		if bv, ok := b.(int64); ok {
			return av == bv
		}
	case float64:
		if bv, ok := b.(float64); ok {
			return av == bv
		}
	case string:
		if bv, ok := b.(string); ok {
			return av == bv
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return av == bv
		}
	case nil:
		return b == nil
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !valuesEqual(v, bv[k]) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i, v := range av {
			if !valuesEqual(v, bv[i]) {
				return false
			}
		}
		return true
	}
	return false
}

// --- round-trip tests for supported types ---

func TestRoundTripString(t *testing.T) {
	roundTrip(t, "hello", "hello")
}

func TestRoundTripInt(t *testing.T) {
	// Starlark ints come back as int64.
	roundTrip(t, 42, int64(42))
}

func TestRoundTripInt64(t *testing.T) {
	roundTrip(t, int64(1<<40), int64(1<<40))
}

func TestRoundTripBool(t *testing.T) {
	roundTrip(t, true, true)
	roundTrip(t, false, false)
}

func TestRoundTripFloat64(t *testing.T) {
	roundTrip(t, 3.14, 3.14)
}

func TestRoundTripNil(t *testing.T) {
	roundTrip(t, nil, nil)
}

func TestRoundTripMap(t *testing.T) {
	roundTrip(t, map[string]any{
		"name": "Alice",
		"age":  30,
	}, map[string]any{
		"name": "Alice",
		"age":  int64(30),
	})
}

func TestRoundTripNestedMap(t *testing.T) {
	roundTrip(t, map[string]any{
		"nested": map[string]any{
			"deep": map[string]any{
				"value": "found",
			},
		},
	}, map[string]any{
		"nested": map[string]any{
			"deep": map[string]any{
				"value": "found",
			},
		},
	})
}

func TestRoundTripSlice(t *testing.T) {
	roundTrip(t, []any{1, 2, 3}, []any{int64(1), int64(2), int64(3)})
}

func TestRoundTripNestedSlice(t *testing.T) {
	roundTrip(t, []any{
		map[string]any{"id": 1},
		map[string]any{"id": 2},
	}, []any{
		map[string]any{"id": int64(1)},
		map[string]any{"id": int64(2)},
	})
}

func TestRoundTripMixedNested(t *testing.T) {
	roundTrip(t, map[string]any{
		"list":   []any{1, "two", true, nil},
		"count":  5,
		"flag":   false,
		"items":  []any{map[string]any{"x": 1.5}},
		"empty":  []any{},
		"emptyM": map[string]any{},
	}, map[string]any{
		"list":   []any{int64(1), "two", true, nil},
		"count":  int64(5),
		"flag":   false,
		"items":  []any{map[string]any{"x": 1.5}},
		"empty":  []any{},
		"emptyM": map[string]any{},
	})
}

// --- newly-supported types ---

func TestRoundTripUint(t *testing.T) {
	roundTrip(t, uint(7), int64(7))
}

func TestRoundTripUint64(t *testing.T) {
	roundTrip(t, uint64(42), int64(42))
}

func TestRoundTripInt32(t *testing.T) {
	roundTrip(t, int32(99), int64(99))
}

func TestRoundTripFloat32(t *testing.T) {
	sv, err := GoToStarlark(float32(1.5))
	if err != nil {
		t.Fatalf("GoToStarlark(float32): %v", err)
	}
	got, err := ValueToGo(sv)
	if err != nil {
		t.Fatalf("ValueToGo: %v", err)
	}
	if got.(float64) != 1.5 {
		t.Errorf("float32 round-trip: got %v, want 1.5", got)
	}
}

func TestRoundTripByteArray(t *testing.T) {
	// []byte should convert to a Starlark string (Starlark has no bytes type)
	// and round-trip back as a string.
	sv, err := GoToStarlark([]byte("hello"))
	if err != nil {
		t.Fatalf("GoToStarlark([]byte): %v", err)
	}
	got, err := ValueToGo(sv)
	if err != nil {
		t.Fatalf("ValueToGo: %v", err)
	}
	if got.(string) != "hello" {
		t.Errorf("[]byte round-trip: got %v, want 'hello'", got)
	}
}

func TestRoundTripTimeTime(t *testing.T) {
	ts := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	sv, err := GoToStarlark(ts)
	if err != nil {
		t.Fatalf("GoToStarlark(time.Time): %v", err)
	}
	got, err := ValueToGo(sv)
	if err != nil {
		t.Fatalf("ValueToGo: %v", err)
	}
	// time.Time becomes an RFC3339 ISO string.
	want := ts.Format(time.RFC3339)
	if got.(string) != want {
		t.Errorf("time.Time round-trip: got %v, want %v", got, want)
	}
}

// --- error cases: unsupported types must error, not stringify ---

func TestGoToStarlarkUnsupportedStruct(t *testing.T) {
	type myStruct struct{ X int }
	_, err := GoToStarlark(myStruct{X: 1})
	if err == nil {
		t.Fatal("GoToStarlark(struct) should return an error, got nil")
	}
}

func TestGoToStarlarkUnsupportedChan(t *testing.T) {
	ch := make(chan int)
	defer close(ch)
	_, err := GoToStarlark(ch)
	if err == nil {
		t.Fatal("GoToStarlark(chan) should return an error, got nil")
	}
}

func TestGoToStarlarkUnsupportedFunc(t *testing.T) {
	_, err := GoToStarlark(func() {})
	if err == nil {
		t.Fatal("GoToStarlark(func) should return an error, got nil")
	}
}

func TestGoToStarlarkUnsupportedMapValue(t *testing.T) {
	type myStruct struct{ X int }
	_, err := GoToStarlark(map[string]any{
		"bad": myStruct{X: 1},
	})
	if err == nil {
		t.Fatal("GoToStarlark(map with unsupported value) should return an error")
	}
}

func TestGoToStarlarkUnsupportedSliceElem(t *testing.T) {
	type myStruct struct{ X int }
	_, err := GoToStarlark([]any{myStruct{X: 1}})
	if err == nil {
		t.Fatal("GoToStarlark(slice with unsupported element) should return an error")
	}
}

// --- ValueToGo integer overflow ---

func TestValueToGoOverflowInt(t *testing.T) {
	// Create a Starlark int larger than int64 max.
	bigVal := big.NewInt(1)
	bigVal.Lsh(bigVal, 63) // 2^63 = overflow for int64
	starlarkBig := sk.MakeBigInt(bigVal)

	_, err := ValueToGo(starlarkBig)
	if err == nil {
		t.Fatal("ValueToGo(overflow int) should return an error, got nil")
	}
}
