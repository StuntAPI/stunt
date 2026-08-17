package runtime

import (
	"testing"

	sk "go.starlark.net/starlark"
)

func intList(vals ...int) sk.Value {
	elems := make([]sk.Value, len(vals))
	for i, v := range vals {
		elems[i] = sk.MakeInt64(int64(v))
	}
	return sk.NewList(elems)
}

// callPaginate invokes the paginate builtin and returns (page, next_cursor).
func callPaginate(t *testing.T, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, sk.Value) {
	t.Helper()
	b := BuildAllBuiltins(BuiltinOptions{})
	fn, ok := b["paginate"]
	if !ok {
		t.Fatal("paginate builtin not registered")
	}
	res, err := sk.Call(new(sk.Thread), fn, args, kwargs)
	if err != nil {
		t.Fatalf("paginate%v: %v", args, err)
	}
	tup, ok := res.(sk.Tuple)
	if !ok || tup.Len() != 2 {
		t.Fatalf("paginate returned %s, want 2-tuple", res.Type())
	}
	return tup.Index(0), tup.Index(1)
}

func listToInts(t *testing.T, v sk.Value) []int {
	t.Helper()
	l, ok := v.(*sk.List)
	if !ok {
		t.Fatalf("got %s, want list", v.Type())
	}
	out := make([]int, 0, l.Len())
	it := l.Iterate()
	defer it.Done()
	var x sk.Value
	for it.Next(&x) {
		n, _ := x.(sk.Int).Int64()
		out = append(out, int(n))
	}
	return out
}

// nextTok stringifies the next_cursor value (None -> "").
func nextTok(t *testing.T, v sk.Value) string {
	t.Helper()
	if v == sk.None {
		return ""
	}
	s, ok := v.(sk.String)
	if !ok {
		t.Fatalf("next_cursor got %s, want string or None", v.Type())
	}
	return string(s)
}

func TestPaginateSlicesAndCursors(t *testing.T) {
	items := intList(1, 2, 3, 4, 5)

	// First page of 2 -> [1,2], next "2".
	page, next := callPaginate(t, sk.Tuple{items, sk.MakeInt64(2), sk.None}, nil)
	if got := listToInts(t, page); !equal(got, []int{1, 2}) {
		t.Fatalf("page0 = %v, want [1 2]", got)
	}
	if tok := nextTok(t, next); tok != "2" {
		t.Fatalf("next0 = %q, want 2", tok)
	}

	// Follow the cursor -> [3,4], next "4".
	page, next = callPaginate(t, sk.Tuple{items, sk.MakeInt64(2), sk.String("2")}, nil)
	if got := listToInts(t, page); !equal(got, []int{3, 4}) {
		t.Fatalf("page1 = %v, want [3 4]", got)
	}
	if tok := nextTok(t, next); tok != "4" {
		t.Fatalf("next1 = %q, want 4", tok)
	}

	// Final page -> [5], no next.
	page, next = callPaginate(t, sk.Tuple{items, sk.MakeInt64(2), sk.String("4")}, nil)
	if got := listToInts(t, page); !equal(got, []int{5}) {
		t.Fatalf("page2 = %v, want [5]", got)
	}
	if next != sk.None {
		t.Fatalf("next2 = %v, want None", next)
	}
}

func TestPaginateLimitDisabledReturnsAll(t *testing.T) {
	items := intList(1, 2, 3, 4, 5)
	for _, limit := range []sk.Value{sk.None, sk.MakeInt64(0), sk.MakeInt64(-3)} {
		page, next := callPaginate(t, sk.Tuple{items, limit, sk.None}, nil)
		if got := listToInts(t, page); !equal(got, []int{1, 2, 3, 4, 5}) {
			t.Fatalf("limit=%s: page = %v, want all", limit, got)
		}
		if next != sk.None {
			t.Fatalf("limit=%s: next = %v, want None", limit, next)
		}
	}
}

func TestPaginateEmptyAndOvershoot(t *testing.T) {
	// Empty list -> empty page, None.
	page, next := callPaginate(t, sk.Tuple{intList(), sk.MakeInt64(10), sk.None}, nil)
	if got := listToInts(t, page); len(got) != 0 {
		t.Fatalf("empty: page = %v, want []", got)
	}
	if next != sk.None {
		t.Fatalf("empty: next = %v, want None", next)
	}

	// Cursor past the end clamps to an empty page, None next.
	page, next = callPaginate(t, sk.Tuple{intList(1, 2, 3), sk.MakeInt64(2), sk.String("99")}, nil)
	if got := listToInts(t, page); len(got) != 0 {
		t.Fatalf("overshoot: page = %v, want []", got)
	}
	if next != sk.None {
		t.Fatalf("overshoot: next = %v, want None", next)
	}
}

func TestPaginateKwargsAndErrors(t *testing.T) {
	items := intList(1, 2, 3)
	// Kwargs form works.
	page, next := callPaginate(t, nil, []sk.Tuple{
		{sk.String("items"), items},
		{sk.String("limit"), sk.MakeInt64(2)},
	})
	if got := listToInts(t, page); !equal(got, []int{1, 2}) {
		t.Fatalf("kwargs: page = %v, want [1 2]", got)
	}
	if tok := nextTok(t, next); tok != "2" {
		t.Fatalf("kwargs: next = %q, want 2", tok)
	}

	// Non-int limit errors.
	b := BuildAllBuiltins(BuiltinOptions{})
	fn := b["paginate"]
	if _, err := sk.Call(new(sk.Thread), fn, sk.Tuple{items, sk.Float(2.0), sk.None}, nil); err == nil {
		t.Fatal("float limit: want error, got nil")
	}
	// Garbage cursor token is TOTAL: (None, None) so the handler can
	// answer its provider's 400 instead of an unhandled 500.
	res, err := sk.Call(new(sk.Thread), fn, sk.Tuple{items, sk.MakeInt64(2), sk.String("abc")}, nil)
	if err != nil {
		t.Fatalf("garbage cursor: error %v, want (None, None)", err)
	}
	tup, ok := res.(sk.Tuple)
	if !ok || tup.Len() != 2 || tup.Index(0) != sk.None || tup.Index(1) != sk.None {
		t.Fatalf("garbage cursor = %v, want (None, None)", res.String())
	}
	// Non-iterable items errors.
	if _, err := sk.Call(new(sk.Thread), fn, sk.Tuple{sk.MakeInt64(42), sk.MakeInt64(2), sk.None}, nil); err == nil {
		t.Fatal("int items: want error, got nil")
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
