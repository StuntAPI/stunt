package runtime

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"

	sk "go.starlark.net/starlark"
)

// buildQueryBuiltins registers the list query/filter builtin. It is pure, like
// paginate: no backing store, available in every handler VM.
//
//	query_select(items, filter?, order_by?, order_dir?, limit?, offset?, fields?)
//
// filter is a list of [field, op, value] triples (AND'ed). op is one of
// =, !=, >, >=, <, <=, contains, startswith, endswith, in, like. field may be
// a dotted path ("address.city"). order_by sorts (order_dir "asc" default /
// "desc"). limit/offset slice. fields projects each dict to those keys.
//
// This is the semantic core; the adapter owns translating its provider's
// query syntax ($filter, SOQL WHERE, q=, sysparm_query, …) into triples.
func buildQueryBuiltins() sk.StringDict {
	return sk.StringDict{
		"query_select": sk.NewBuiltin("query_select", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var itemsVal, filterVal, fieldsVal sk.Value = sk.None, sk.None, sk.None
			var orderByVal, orderDirVal sk.Value = sk.None, sk.None
			var limitVal sk.Value = sk.None
			var offsetVal sk.Value = sk.None
			if err := sk.UnpackArgs("query_select", args, kwargs,
				"items", &itemsVal,
				"filter?", &filterVal,
				"order_by?", &orderByVal,
				"order_dir?", &orderDirVal,
				"limit?", &limitVal,
				"offset?", &offsetVal,
				"fields?", &fieldsVal,
			); err != nil {
				return nil, err
			}
			orderBy, _ := sk.AsString(orderByVal)
			orderDir, _ := sk.AsString(orderDirVal)

			// Materialize the iterable.
			iter := sk.Iterate(itemsVal)
			if iter == nil {
				return nil, fmt.Errorf("query_select: items must be iterable, got %s", itemsVal.Type())
			}
			var items []sk.Value
			var item sk.Value
			for iter.Next(&item) {
				items = append(items, item)
			}
			iter.Done()

			// Parse the filter triples.
			type clause struct {
				field string
				op    string
				value sk.Value
			}
			var clauses []clause
			if filterVal != sk.None {
				fl, ok := filterVal.(*sk.List)
				if !ok {
					return nil, fmt.Errorf("query_select: filter must be a list of [field, op, value], got %s", filterVal.Type())
				}
				for i := 0; i < fl.Len(); i++ {
					t, ok := fl.Index(i).(*sk.List)
					if !ok || t.Len() != 3 {
						return nil, fmt.Errorf("query_select: filter[%d] must be a [field, op, value] triple", i)
					}
					field, ok := sk.AsString(t.Index(0))
					if !ok {
						return nil, fmt.Errorf("query_select: filter[%d] field must be a string", i)
					}
					op, ok := sk.AsString(t.Index(1))
					if !ok {
						return nil, fmt.Errorf("query_select: filter[%d] op must be a string", i)
					}
					switch op {
					case "=", "!=", ">", ">=", "<", "<=", "contains", "startswith", "endswith", "in", "like":
					default:
						return nil, fmt.Errorf("query_select: filter[%d] unknown op %q", i, op)
					}
					clauses = append(clauses, clause{field: field, op: op, value: t.Index(2)})
				}
			}

			// Filter.
			out := items[:0]
			for _, it := range items {
				match := true
				for _, c := range clauses {
					ok, err := evalClause(it, c.field, c.op, c.value)
					if err != nil {
						return nil, err
					}
					if !ok {
						match = false
						break
					}
				}
				if match {
					out = append(out, it)
				}
			}
			items = out

			// Sort.
			if orderBy != "" {
				desc := strings.EqualFold(orderDir, "desc")
				var sortErr error
				sort.SliceStable(items, func(i, j int) bool {
					a, aok := fieldByPath(items[i], orderBy)
					b, bok := fieldByPath(items[j], orderBy)
					// Missing values sort last regardless of direction.
					if !aok || !bok {
						return aok && !bok
					}
					c, err := compareValues(a, b)
					if err != nil {
						sortErr = err
						return false
					}
					if desc {
						return c > 0
					}
					return c < 0
				})
				if sortErr != nil {
					return nil, fmt.Errorf("query_select: order_by %s: %w", orderBy, sortErr)
				}
			}

			// Offset/limit.
			start := 0
			if offsetVal != sk.None {
				off, ok := offsetVal.(sk.Int)
				if !ok {
					return nil, fmt.Errorf("query_select: offset must be an int, got %s", offsetVal.Type())
				}
				n, ok := off.Int64()
				if !ok {
					n = math.MaxInt64
				}
				start = int(n)
				if start < 0 {
					start = 0
				}
			}
			if start > len(items) {
				start = len(items)
			}
			end := len(items)
			if limitVal != sk.None {
				lim, ok := limitVal.(sk.Int)
				if !ok {
					return nil, fmt.Errorf("query_select: limit must be an int, got %s", limitVal.Type())
				}
				n, ok := lim.Int64()
				if !ok {
					n = math.MaxInt64
				}
				if n < 0 {
					n = 0
				}
				end = start + int(n)
				if end > len(items) {
					end = len(items)
				}
			}
			items = items[start:end]

			// Projection.
			if fieldsVal != sk.None {
				fl, ok := fieldsVal.(*sk.List)
				if !ok {
					return nil, fmt.Errorf("query_select: fields must be a list of key names, got %s", fieldsVal.Type())
				}
				names := make([]string, 0, fl.Len())
				for i := 0; i < fl.Len(); i++ {
					n, ok := sk.AsString(fl.Index(i))
					if !ok {
						return nil, fmt.Errorf("query_select: fields[%d] must be a string", i)
					}
					names = append(names, n)
				}
				projected := make([]sk.Value, len(items))
				for i, it := range items {
					d, ok := it.(*sk.Dict)
					if !ok {
						return nil, fmt.Errorf("query_select: fields projection requires dict items, got %s", it.Type())
					}
					nd := sk.NewDict(len(names))
					for _, n := range names {
						if v, found, _ := d.Get(sk.String(n)); found {
							_ = nd.SetKey(sk.String(n), v)
						}
					}
					projected[i] = nd
				}
				items = projected
			}

			return sk.NewList(items), nil
		}),
	}
}

// fieldByPath resolves a possibly-dotted path against a dict ("a.b" →
// d["a"]["b"]). The second return is false when any step is missing or not a
// dict.
func fieldByPath(v sk.Value, path string) (sk.Value, bool) {
	cur := v
	for _, seg := range strings.Split(path, ".") {
		d, ok := cur.(*sk.Dict)
		if !ok {
			return nil, false
		}
		nv, found, _ := d.Get(sk.String(seg))
		if !found {
			return nil, false
		}
		cur = nv
	}
	return cur, true
}

// evalClause applies one [field, op, value] triple to an item.
func evalClause(item sk.Value, field, op string, want sk.Value) (bool, error) {
	have, ok := fieldByPath(item, field)
	// Missing field: only != (and "in" over a list not containing None)
	// matches; everything else is false.
	if !ok {
		switch op {
		case "!=":
			return true, nil
		case "in":
			return listContains(want, sk.None), nil
		default:
			return false, nil
		}
	}

	switch op {
	case "=", "!=":
		eq, err := equals(have, want)
		if err != nil {
			return false, err
		}
		if op == "=" {
			return eq, nil
		}
		return !eq, nil
	case ">", ">=", "<", "<=":
		c, comparable := compareTyped(have, want)
		if !comparable {
			return false, nil
		}
		switch op {
		case ">":
			return c > 0, nil
		case ">=":
			return c >= 0, nil
		case "<":
			return c < 0, nil
		default:
			return c <= 0, nil
		}
	case "contains", "startswith", "endswith", "like":
		hs, ok1 := sk.AsString(have)
		ws, ok2 := sk.AsString(want)
		if !ok1 || !ok2 {
			return false, fmt.Errorf("query_select: op %q needs string values, got %s %s", op, have.Type(), want.Type())
		}
		switch op {
		case "contains":
			return strings.Contains(hs, ws), nil
		case "startswith":
			return strings.HasPrefix(hs, ws), nil
		case "endswith":
			return strings.HasSuffix(hs, ws), nil
		default:
			return likeMatch(hs, ws), nil
		}
	case "in":
		return listContains(want, have), nil
	}
	return false, fmt.Errorf("query_select: unknown op %q", op)
}

func listContains(list, want sk.Value) bool {
	l, ok := list.(*sk.List)
	if !ok {
		return false
	}
	for i := 0; i < l.Len(); i++ {
		if eq, err := equals(l.Index(i), want); err == nil && eq {
			return true
		}
	}
	return false
}

// equals compares two scalar values. Int vs Int compares exactly (big.Int —
// snowflake-scale ids must not collide through float64); int/float mix
// numerically; other equal types structurally; cross-type is false (a string
// query param never equals a numeric field — adapters convert).
func equals(a, b sk.Value) (bool, error) {
	if ai, aok := a.(sk.Int); aok {
		if bi, bok := b.(sk.Int); bok {
			return ai.BigInt().Cmp(bi.BigInt()) == 0, nil
		}
	}
	if an, aok := numeric(a); aok {
		if bn, bok := numeric(b); bok {
			return an == bn, nil
		}
		return false, nil
	}
	if a.Type() == b.Type() {
		eq, err := sk.Equal(a, b)
		if err != nil {
			return false, fmt.Errorf("query_select: compare %s: %w", a.Type(), err)
		}
		return eq, nil
	}
	return false, nil
}

// compareTyped is the filter-path comparison: strict about types so an
// ordering op can never fall through to string-form matching. A numeric field
// vs a numeric string compares numerically (query params arrive as strings);
// anything else mixed is incomparable (the clause is false).
func compareTyped(a, b sk.Value) (int, bool) {
	if ai, aok := a.(sk.Int); aok {
		if bi, bok := b.(sk.Int); bok {
			return ai.BigInt().Cmp(bi.BigInt()), true
		}
	}
	an, aNum := numericOrString(a)
	bn, bNum := numericOrString(b)
	if aNum && bNum {
		switch {
		case an < bn:
			return -1, true
		case an > bn:
			return 1, true
		default:
			return 0, true
		}
	}
	as, aok := sk.AsString(a)
	bs, bok := sk.AsString(b)
	if aok && bok {
		return strings.Compare(as, bs), true
	}
	return 0, false
}

// compareValues is the sort-path comparison: typed first, then a string-form
// fallback so sorts over mixed fields never fail (and stay deterministic).
func compareValues(a, b sk.Value) (int, error) {
	if c, ok := compareTyped(a, b); ok {
		return c, nil
	}
	return strings.Compare(a.String(), b.String()), nil
}

// numericOrString reports a numeric value for numbers and for strings that
// parse as a number; otherwise not numeric.
func numericOrString(v sk.Value) (float64, bool) {
	if f, ok := numeric(v); ok {
		return f, true
	}
	if s, ok := sk.AsString(v); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func numeric(v sk.Value) (float64, bool) {
	switch n := v.(type) {
	case sk.Int:
		i, ok := n.Int64()
		if !ok {
			f, _ := new(big.Float).SetInt(n.BigInt()).Float64()
			return f, true
		}
		return float64(i), true
	case sk.Float:
		return float64(n), true
	}
	return 0, false
}

// likeMatch implements SQL LIKE with % (any run) and _ (one char — one RUNE,
// not one byte). The whole pattern must match, case-sensitively.
func likeMatch(s, pattern string) bool {
	// Dynamic programming over (len(s)+1) x (len(pattern)+1).
	sr, pr := []rune(s), []rune(pattern)
	m, n := len(sr), len(pr)
	dp := make([][]bool, m+1)
	for i := range dp {
		dp[i] = make([]bool, n+1)
	}
	dp[0][0] = true
	for j := 1; j <= n && pr[j-1] == '%'; j++ {
		dp[0][j] = true
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			pc := pr[j-1]
			switch pc {
			case '%':
				dp[i][j] = dp[i-1][j] || dp[i][j-1]
			case '_':
				dp[i][j] = dp[i-1][j-1]
			default:
				dp[i][j] = dp[i-1][j-1] && sr[i-1] == pc
			}
		}
	}
	return dp[m][n]
}
