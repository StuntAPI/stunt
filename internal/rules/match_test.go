package rules

import "testing"

func TestMatchesMethodPathHeaders(t *testing.T) {
	cases := []struct {
		name string
		m    Match
		req  Request
		want bool
	}{
		{"exact path", Match{Method: "GET", Path: "/hello"}, Request{Method: "GET", Path: "/hello"}, true},
		{"wrong method", Match{Method: "GET", Path: "/hello"}, Request{Method: "POST", Path: "/hello"}, false},
		{"single segment wildcard", Match{Method: "GET", Path: "/users/*"}, Request{Method: "GET", Path: "/users/42"}, true},
		{"wildcard no span", Match{Method: "GET", Path: "/users/*"}, Request{Method: "GET", Path: "/users/42/posts"}, false},
		{"multi segment wildcard", Match{Method: "GET", Path: "/files/**"}, Request{Method: "GET", Path: "/files/a/b/c"}, true},
		{"empty match matches all", Match{}, Request{Method: "DELETE", Path: "/x"}, true},
		{"header mismatch", Match{Headers: map[string]string{"x-api": "k"}}, Request{Path: "/x", Headers: map[string]string{"x-api": "other"}}, false},
		{"header match", Match{Headers: map[string]string{"x-api": "k"}}, Request{Path: "/x", Headers: map[string]string{"x-api": "k", "extra": "1"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.m.Matches(c.req); got != c.want {
				t.Fatalf("Matches = %v, want %v", got, c.want)
			}
		})
	}
}
