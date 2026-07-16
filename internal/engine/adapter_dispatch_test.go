package engine

import (
	"testing"
)

func TestMatchRoute(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
		ok      bool
		params  map[string]string
	}{
		{"exact match", "/charges", "/charges", true, nil},
		{"exact mismatch", "/charges", "/refunds", false, nil},
		{"param match", "/charges/{id}", "/charges/abc123", true, map[string]string{"id": "abc123"}},
		{"param mismatch extra seg", "/charges/{id}", "/charges/abc/def", false, nil},
		{"multi segment with param", "/charges/{id}/refund", "/charges/abc/refund", true, map[string]string{"id": "abc"}},
		{"param at root", "/{id}", "/xyz", true, map[string]string{"id": "xyz"}},
		{"trailing slash ignored", "/charges/", "/charges", true, nil},
		{"root match", "/", "/", true, nil},
		{"different lengths", "/charges", "/charges/abc", false, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			params, ok := matchRoute(c.pattern, c.path)
			if ok != c.ok {
				t.Fatalf("matchRoute(%q, %q) ok = %v, want %v", c.pattern, c.path, ok, c.ok)
			}
			if ok && c.params != nil {
				for k, v := range c.params {
					if params[k] != v {
						t.Fatalf("params[%q] = %q, want %q", k, params[k], v)
					}
				}
			}
		})
	}
}

func TestMethodMatches(t *testing.T) {
	cases := []struct {
		epMethod  string
		reqMethod string
		want      bool
	}{
		{"GET", "GET", true},
		{"GET", "get", true},
		{"POST", "POST", true},
		{"GET", "POST", false},
		{"", "GET", true},  // empty endpoint method matches anything
		{"", "DELETE", true},
	}
	for _, c := range cases {
		got := methodMatches(c.epMethod, c.reqMethod)
		if got != c.want {
			t.Fatalf("methodMatches(%q, %q) = %v, want %v", c.epMethod, c.reqMethod, got, c.want)
		}
	}
}

func TestSplitHandlerRef(t *testing.T) {
	path, fn := splitHandlerRef("/abs/scripts/x.star#on_post")
	if path != "/abs/scripts/x.star" || fn != "on_post" {
		t.Fatalf("splitHandlerRef = (%q, %q)", path, fn)
	}
	// No fragment
	path, fn = splitHandlerRef("/abs/scripts/x.star")
	if path != "/abs/scripts/x.star" || fn != "" {
		t.Fatalf("splitHandlerRef (no fn) = (%q, %q)", path, fn)
	}
}
