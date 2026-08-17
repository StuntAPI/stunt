package engine

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestParseFormBodyBracketNotation pins the Rails/PHP bracket expansion used
// by provider SDKs (stripe-node POSTs line_items[0][price_data][currency]=…
// urlencoded). Handlers must see real nested dicts/lists in req["body"].
func TestParseFormBodyBracketNotation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // JSON of the expected map
	}{
		{"flat", "a=1&b=two", `{"a":"1","b":"two"}`},
		{"nested dict", "a[b]=1", `{"a":{"b":"1"}}`},
		{"deep dict", "a[b][c]=1", `{"a":{"b":{"c":"1"}}}`},
		{"bare append list", "a[]=1&a[]=2", `{"a":["1","2"]}`},
		// NOTE: repeated a[] values arrive under ONE ParseQuery key so their
		// order is preserved; a[][k] pairs are separate keys whose relative
		// map-iteration order is unspecified — asserted order-insensitively
		// in TestParseFormBodyBareAppendDictsUnordered below.
		{"numeric indexed merge", "a[0][b]=1&a[0][c]=2&a[1][b]=3", `{"a":[{"b":"1","c":"2"},{"b":"3"}]}`},
		// Terminal numeric index — the Rails array-literal form. Found by
		// fuzzing: an empty tail segs panicked the recursion (index out
		// of range).
		{"terminal numeric index", "a[0]=v", `{"a":["v"]}`},
		{"terminal numeric indexes", "a[0]=1&a[1]=2", `{"a":["1","2"]}`},
		{"terminal numeric, numeric key, empty value", "0[0]=", `{"0":[""]}`},
		// Found by fuzzing: a huge bracket index must be skipped, not
		// materialized as a hundred-million-element sparse slice (57s
		// in the parser alone before the cap).
		{"pathological index skipped", "a[222222220][b]=v&ok=1", `{"ok":"1"}`},
		{"flat + brackets coexist", "mode=payment&line_items[0][qty]=2", `{"mode":"payment","line_items":[{"qty":"2"}]}`},
		{"stripe-shaped", "line_items[0][price_data][currency]=usd&line_items[0][price_data][unit_amount]=1000&line_items[0][quantity]=1&success_url=https%3A%2F%2Fx.test%2Fs",
			`{"line_items":[{"price_data":{"currency":"usd","unit_amount":"1000"},"quantity":"1"}],"success_url":"https://x.test/s"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseFormBody(tc.raw)
			if got == nil {
				t.Fatalf("parseFormBody(%q) = nil", tc.raw)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatalf("bad want JSON: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				g, _ := json.Marshal(got)
				w, _ := json.Marshal(want)
				t.Fatalf("parseFormBody(%q)\n got %s\nwant %s", tc.raw, g, w)
			}
		})
	}
}

// TestParseFormBodyBareAppendDictsUnordered proves a[][k] pairs append one
// dict each; their relative order follows ParseQuery's map iteration and is
// unspecified — only membership is asserted.
func TestParseFormBodyBareAppendDictsUnordered(t *testing.T) {
	got := parseFormBody("a[][b]=1&a[][c]=2")
	l, ok := got["a"].([]any)
	if !ok || len(l) != 2 {
		t.Fatalf("a = %v, want a 2-element list of dicts", got["a"])
	}
	seen := map[string]string{}
	for _, e := range l {
		m, ok := e.(map[string]any)
		if !ok || len(m) != 1 {
			t.Fatalf("element %v, want single-key dict", e)
		}
		for k, v := range m {
			seen[k] = v.(string)
		}
	}
	if seen["b"] != "1" || seen["c"] != "2" {
		t.Fatalf("appended dicts = %v, want {b:1, c:2}", seen)
	}
}

// TestParseFormBodyMalformedBracketsFallsBackFlat proves unbalanced bracket
// garbage degrades to the historical flat-key behavior instead of erroring.
func TestParseFormBodyMalformedBracketsFallsBackFlat(t *testing.T) {
	got := parseFormBody("a[=1&b]=2")
	if got == nil {
		t.Fatal("parseFormBody returned nil for malformed brackets")
	}
	if got["a["] != "1" {
		t.Errorf("a[ = %v, want 1 (flat fallback)", got["a["])
	}
	if got["b]"] != "2" {
		t.Errorf("b] = %v, want 2 (flat fallback)", got["b]"])
	}
}

// TestParseFormBodyShapeConflictSkipped proves a scalar/structure collision
// at the same path never crashes the handler and never mixes shapes — exactly
// one of the two survives, regardless of ParseQuery's map iteration order.
func TestParseFormBodyShapeConflictSkipped(t *testing.T) {
	for _, raw := range []string{"a=1&a[b]=2", "a[b]=1&a=2", "a[0]=2&a[0][b]=1", "a[0][b]=1&a[0]=2"} {
		got := parseFormBody(raw)
		if got == nil {
			t.Fatalf("parseFormBody(%q) = nil", raw)
		}
		switch a := got["a"].(type) {
		case string:
			if a != "1" && a != "2" {
				t.Errorf("parseFormBody(%q): a = %q, want the scalar", raw, a)
			}
		case []any:
			// Numeric-index variant: exactly one element, either the
			// scalar or the dict — never mixed shapes.
			if len(a) != 1 {
				t.Errorf("parseFormBody(%q): a = %v, want one element", raw, a)
			}
			switch e := a[0].(type) {
			case string:
				if e != "1" && e != "2" {
					t.Errorf("parseFormBody(%q): a[0] = %q, want the scalar", raw, e)
				}
			case map[string]any:
				if e["b"] != "1" {
					t.Errorf("parseFormBody(%q): a[0][b] = %v, want 1", raw, e["b"])
				}
			default:
				t.Errorf("parseFormBody(%q): a[0] = %v (%T), want scalar or dict", raw, a[0], a[0])
			}
		case map[string]any:
			if a["b"] != "1" && a["b"] != "2" {
				t.Errorf("parseFormBody(%q): a[b] = %v, want the dict value", raw, a["b"])
			}
		default:
			t.Errorf("parseFormBody(%q): a = %v (%T), want scalar, list, or dict", raw, got["a"], got["a"])
		}
	}
}
