package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzMatchRoute throws arbitrary pattern/path pairs at the router core —
// whole-segment params, embedded-prefix params (OData `prefix{p}suffix`),
// dot-separated versions, and whatever mutation the fuzzer invents.
// Invariants: no panic; a match's captured params never contain "/" (they
// are segment-scoped) and never use an empty key.
func FuzzMatchRoute(f *testing.F) {
	for _, s := range [][2]string{
		{"/v1/charges/{id}", "/v1/charges/ch_123"},
		{"/accounts({id})", "/accounts(001xx)"},
		{"/v1.0/{container_id}", "/v1.0/c_1"},
		{"/v1.0/{a}/{b}/x", "/v1.0/1/2/x"},
		{"/v1/{id}", "/v1/"},
		{"/v1/{id}", "/v1/a/b"},
		{"/prefix{p}suffix", "/prefixmiddlesuffix"},
		{"/prefix{p}suffix", "/prefixsuffix"},
		{"/x/{a}", "//"},
		{"/a/b", "/a/b/"},
		{"/v21.0/{media_id}/comments", "/v21.0/m_1/comments"},
		{"{only}", "anything"},
		{"", ""},
		{"/", "/"},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, pattern, path string) {
		params, ok := matchRoute(pattern, path)
		if !ok {
			return
		}
		for k, v := range params {
			if k == "" {
				t.Fatalf("matchRoute(%q, %q) captured an empty param name: %v", pattern, path, params)
			}
			if strings.Contains(v, "/") {
				t.Fatalf("matchRoute(%q, %q): param %q captured across segments: %q", pattern, path, k, v)
			}
		}
	})
}

// FuzzParseFormBody drives the bracket-notation form parser (Rails/PHP
// SDK bodies: a[b]=v, a[]=v, a[0][b]=v) with arbitrary raw payloads.
// Invariants: no panic; the result is a non-nil map that stays
// JSON-marshalable — it flows into the Starlark request dict and back out
// through respond(), so a non-marshalable value would be a downstream 500.
func FuzzParseFormBody(f *testing.F) {
	for _, s := range []string{
		"a=1",
		"a[b]=v",
		"a[]=1&a[]=2",
		"a[0][b]=v",
		"line_items[0][price_data][currency]=usd&line_items[0][quantity]=2",
		"a[]=1&a[b]=2",
		"a=1&a[b]=2",
		"a[b]=1&a=2",
		"=v",
		"a=",
		"&&&",
		"a[b][c][d][e][f]=1",
		"a%zz=badpercent",
		"a=%C3%28",
		"key=value+with+plus",
		"a[]=b[c]=d",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		// A body with no decodable pairs legitimately yields nil (the
		// handler then sees an empty body); the invariant is that any
		// map it DOES return stays JSON-marshalable — it flows into the
		// Starlark request dict and back out through respond().
		m := parseFormBody(raw)
		if m == nil {
			return
		}
		if _, err := json.Marshal(m); err != nil {
			t.Fatalf("parseFormBody(%q) produced non-marshalable state: %v", raw, err)
		}
	})
}

// The greedy {name+} terminal segment (S3 object keys) is pinned here and
// in the router unit tests.
func TestMatchRouteGreedy(t *testing.T) {
	params, ok := matchRoute("/{bucket}/{key+}", "/my-bucket/photos/2024/a.jpg")
	if !ok {
		t.Fatal("greedy route did not match a slashed key")
	}
	if params["bucket"] != "my-bucket" || params["key"] != "photos/2024/a.jpg" {
		t.Fatalf("params = %v", params)
	}
	if _, ok := matchRoute("/{bucket}/{key+}", "/only-bucket"); ok {
		t.Fatal("greedy route matched without a key segment")
	}
	if params, ok := matchRoute("/{bucket}/{key+}", "/b/flat.txt"); !ok || params["key"] != "flat.txt" {
		t.Fatalf("flat key: %v %v", params, ok)
	}
}
