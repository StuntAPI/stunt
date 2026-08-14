package runtime

import (
	"testing"

	sk "go.starlark.net/starlark"
)

func doc(vals ...any) sk.Value {
	d := sk.NewDict(len(vals))
	for i := 0; i+1 < len(vals); i += 2 {
		_ = d.SetKey(sk.String(vals[i].(string)), toSK(vals[i+1]))
	}
	return d
}

func toSK(v any) sk.Value {
	switch x := v.(type) {
	case string:
		return sk.String(x)
	case int:
		return sk.MakeInt64(int64(x))
	case float64:
		return sk.Float(x)
	case bool:
		return sk.Bool(x)
	case []string:
		elems := make([]sk.Value, len(x))
		for i, s := range x {
			elems[i] = sk.String(s)
		}
		return sk.NewList(elems)
	case sk.Value:
		return x
	}
	return sk.None
}

// callQuery invokes query_select with a Starlark-eval'd items list.
func callQuery(t *testing.T, items sk.Value, filter sk.Value, kwargs []sk.Tuple) *sk.List {
	t.Helper()
	b := BuildAllBuiltins(BuiltinOptions{})
	fn, ok := b["query_select"]
	if !ok {
		t.Fatal("query_select builtin not registered")
	}
	res, err := sk.Call(new(sk.Thread), fn, sk.Tuple{items, filter}, kwargs)
	if err != nil {
		t.Fatalf("query_select: %v", err)
	}
	l, ok := res.(*sk.List)
	if !ok {
		t.Fatalf("query_select returned %s, want list", res.Type())
	}
	return l
}

func triples(ts ...[]any) sk.Value {
	elems := make([]sk.Value, len(ts))
	for i, tr := range ts {
		elems[i] = sk.NewList([]sk.Value{sk.String(tr[0].(string)), sk.String(tr[1].(string)), toSK(tr[2])})
	}
	return sk.NewList(elems)
}

var orders = sk.NewList([]sk.Value{
	doc("id", "o1", "status", "open", "amount", 100, "customer", doc("name", "acme")),
	doc("id", "o2", "status", "closed", "amount", 250, "customer", doc("name", "globex")),
	doc("id", "o3", "status", "open", "amount", 50, "customer", doc("name", "acme eu")),
	doc("id", "o4", "status", "pending", "amount", 175, "meta", doc("tag", "vip")),
})

func ids(l *sk.List) []string {
	out := make([]string, 0, l.Len())
	for i := 0; i < l.Len(); i++ {
		v, _, _ := l.Index(i).(*sk.Dict).Get(sk.String("id"))
		out = append(out, string(v.(sk.String)))
	}
	return out
}

func eqIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestQuerySelectEqualityAndNot(t *testing.T) {
	l := callQuery(t, orders, triples([]any{"status", "=", "open"}), nil)
	eqIDs(t, ids(l), "o1", "o3")

	l = callQuery(t, orders, triples([]any{"status", "!=", "open"}), nil)
	eqIDs(t, ids(l), "o2", "o4")
}

func TestQuerySelectComparisonAndCombined(t *testing.T) {
	l := callQuery(t, orders, triples([]any{"amount", ">=", 100}), nil)
	eqIDs(t, ids(l), "o1", "o2", "o4")

	// AND of two clauses.
	l = callQuery(t, orders, triples(
		[]any{"status", "=", "open"},
		[]any{"amount", "<", 100},
	), nil)
	eqIDs(t, ids(l), "o3")
}

func TestQuerySelectStringOps(t *testing.T) {
	l := callQuery(t, orders, triples([]any{"customer.name", "contains", "acme"}), nil)
	eqIDs(t, ids(l), "o1", "o3")

	l = callQuery(t, orders, triples([]any{"customer.name", "startswith", "glob"}), nil)
	eqIDs(t, ids(l), "o2")

	l = callQuery(t, orders, triples([]any{"customer.name", "like", "acme%"}), nil)
	eqIDs(t, ids(l), "o1", "o3")

	l = callQuery(t, orders, triples([]any{"customer.name", "like", "acme _u"}), nil)
	eqIDs(t, ids(l), "o3")
}

func TestQuerySelectInAndMissingField(t *testing.T) {
	l := callQuery(t, orders, triples([]any{"status", "in", []string{"open", "pending"}}), nil)
	eqIDs(t, ids(l), "o1", "o3", "o4")

	// Nested field: o4 has meta.tag=vip, the others lack meta entirely —
	// a missing field only matches !=.
	l = callQuery(t, orders, triples([]any{"meta.tag", "!=", "vip"}), nil)
	eqIDs(t, ids(l), "o1", "o2", "o3")
	l = callQuery(t, orders, triples([]any{"meta.tag", "=", "vip"}), nil)
	eqIDs(t, ids(l), "o4")
}

func TestQuerySelectSortOffsetLimit(t *testing.T) {
	l := callQuery(t, orders, sk.None, []sk.Tuple{{sk.String("order_by"), sk.String("amount")}})
	eqIDs(t, ids(l), "o3", "o1", "o4", "o2")

	l = callQuery(t, orders, sk.None, []sk.Tuple{
		{sk.String("order_by"), sk.String("amount")},
		{sk.String("order_dir"), sk.String("desc")},
	})
	eqIDs(t, ids(l), "o2", "o4", "o1", "o3")

	l = callQuery(t, orders, sk.None, []sk.Tuple{
		{sk.String("order_by"), sk.String("amount")},
		{sk.String("limit"), sk.MakeInt64(2)},
		{sk.String("offset"), sk.MakeInt64(1)},
	})
	eqIDs(t, ids(l), "o1", "o4")
}

func TestQuerySelectProjection(t *testing.T) {
	l := callQuery(t, orders, triples([]any{"status", "=", "open"}),
		[]sk.Tuple{{sk.String("fields"), toSK([]string{"id", "amount"})}})
	if l.Len() != 2 {
		t.Fatalf("len = %d, want 2", l.Len())
	}
	for i := 0; i < l.Len(); i++ {
		d := l.Index(i).(*sk.Dict)
		if d.Len() != 2 {
			t.Fatalf("projected dict has %d keys, want 2 (id, amount)", d.Len())
		}
		if _, found, _ := d.Get(sk.String("status")); found {
			t.Fatal("status should be projected out")
		}
	}
}

func TestQuerySelectNumberVsStringParam(t *testing.T) {
	// Query params arrive as strings; amount is numeric. Cross-type equality
	// must be false rather than an error, and numeric filters keep working.
	l := callQuery(t, orders, triples([]any{"amount", "=", "100"}), nil)
	eqIDs(t, ids(l))

	l = callQuery(t, orders, triples([]any{"amount", "=", 100}), nil)
	eqIDs(t, ids(l), "o1")
}

func TestQuerySelectIsoTimestampsCompareChronologically(t *testing.T) {
	events := sk.NewList([]sk.Value{
		doc("id", "e1", "created", "2024-01-02T00:00:00Z"),
		doc("id", "e2", "created", "2023-12-31T23:59:59Z"),
		doc("id", "e3", "created", "2024-06-01T00:00:00Z"),
	})
	l := callQuery(t, events, triples([]any{"created", ">", "2024-01-01T00:00:00Z"}), nil)
	eqIDs(t, ids(l), "e1", "e3")
}

func TestQuerySelectErrors(t *testing.T) {
	b := BuildAllBuiltins(BuiltinOptions{})
	fn := b["query_select"]
	thread := new(sk.Thread)
	if _, err := sk.Call(thread, fn, sk.Tuple{sk.MakeInt64(1), sk.None}, nil); err == nil {
		t.Error("non-iterable items: want error")
	}
	if _, err := sk.Call(thread, fn, sk.Tuple{orders, triples([]any{"status", "~", "x"})}, nil); err == nil {
		t.Error("unknown op: want error")
	}
	badFilter := sk.NewList([]sk.Value{sk.MakeInt64(1)})
	if _, err := sk.Call(thread, fn, sk.Tuple{orders, badFilter}, nil); err == nil {
		t.Error("non-triple filter: want error")
	}
}

func TestQuerySelectOrderingMixedTypes(t *testing.T) {
	// Numeric field vs numeric string compares numerically (query params
	// arrive as strings); a non-numeric string against a number is simply
	// non-matching rather than lexicographic garbage.
	l := callQuery(t, orders, triples([]any{"amount", ">", "100"}), nil)
	eqIDs(t, ids(l), "o2", "o4")
	l = callQuery(t, orders, triples([]any{"amount", "<=", "100"}), nil)
	eqIDs(t, ids(l), "o1", "o3")
	l = callQuery(t, orders, triples([]any{"amount", ">", "zero"}), nil)
	eqIDs(t, ids(l))
}

func TestQuerySelectSnowflakeIntPrecision(t *testing.T) {
	// 2^53+1 vs 2^53 must not collide through float64.
	big1 := doc("id", "a", "n", sk.MakeInt64(1<<53+1))
	big2 := doc("id", "b", "n", sk.MakeInt64(1<<53))
	items := sk.NewList([]sk.Value{big1, big2})
	l := callQuery(t, items, triples([]any{"n", "=", sk.MakeInt64(1<<53 + 1)}), nil)
	eqIDs(t, ids(l), "a")
	l = callQuery(t, items, triples([]any{"n", ">", sk.MakeInt64(1 << 53)}), nil)
	eqIDs(t, ids(l), "a")
}

func TestQuerySelectLikeIsRuneBased(t *testing.T) {
	acc := sk.NewList([]sk.Value{doc("id", "x1", "name", "é")})
	l := callQuery(t, acc, triples([]any{"name", "like", "_"}), nil)
	eqIDs(t, ids(l), "x1")
	l = callQuery(t, acc, triples([]any{"name", "like", "__"}), nil)
	eqIDs(t, ids(l))
}
