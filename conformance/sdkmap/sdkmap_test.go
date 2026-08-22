package sdkmap

import (
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		// every param spelling canonicalizes to * on BOTH sides, so
		// adapter {userId} matches spec {userId} and vice versa
		{"/gmail/v1/users/{userId}/messages", "/gmail/v1/users/*/messages"},
		// JS template interpolation -> param wildcard
		{"/v1/charges/${encodeURIComponent(id)}", "/v1/charges/*"},
		{"/Accounts/${accountSid}/Messages/${sid}.json", "/Accounts/*/Messages/*.json"},
		// multi-segment discovery wildcard expands, then collapses
		{"/v1/{+name}/roles", "/v1/*/roles"},
		// discovery flatPath {x}/{x1}/{x2} expansions collapse to one *
		{"/v1/projects/{projectsId}/policies/{policiesId}/{policiesId1}", "/v1/projects/*/policies/*"},
		// sprintf-style
		{"/orders/%v", "/orders/*"},
		// literal suffix on a param survives
		{"/tickets/{id}.json", "/tickets/*.json"},
		// literals and :verb suffixes stay verbatim
		{"/calendar/v3/users/me/calendarList", "/calendar/v3/users/me/calendarList"},
		{"/v1beta/properties/{property}:runReport", "/v1beta/properties/*:runReport"},
		// query strings are not part of a route
		{"/upload/gmail/v1/users/{userId}/messages/send?alt=media", "/upload/gmail/v1/users/*/messages/send"},
		// no leading slash tolerated
		{"gmail/v1/users/{userId}/labels", "/gmail/v1/users/*/labels"},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDiff(t *testing.T) {
	spec := []Route{
		{Method: "GET", Path: "/a"},
		{Method: "POST", Path: "/a"},
		{Method: "POST", Path: "/b/{id}.json"},
		{Method: "DELETE", Path: "/c"},
	}
	adapter := []AdapterRoute{
		{Method: "GET", Path: "/a"},
		{Method: "", Path: "/b/{ticket}.json"}, // any-method covers POST
		{Method: "PUT", Path: "/c"},            // wrong method does not
	}
	res := Diff(spec, adapter)
	if res.Provider != 4 || res.Covered != 2 {
		t.Fatalf("Provider=%d Covered=%d, want 4/2", res.Provider, res.Covered)
	}
	// spec param+suffix is covered by a bare-param adapter route: engine
	// params capture dot-bearing segments.
	spec = append(spec, Route{Method: "GET", Path: "/d/{id}.json"})
	adapter = append(adapter, AdapterRoute{Method: "GET", Path: "/d/{did}"})
	res2 := Diff(spec, adapter)
	if res2.Covered != 3 {
		t.Errorf("suffix-covered: Covered=%d, want 3", res2.Covered)
	}
	want := map[string]bool{"POST /a": true, "DELETE /c": true}
	if len(res.Missing) != len(want) {
		t.Fatalf("missing = %+v", res.Missing)
	}
	for _, m := range res.Missing {
		k := m.Method + " " + m.Path
		if !want[k] {
			t.Errorf("unexpected missing %s", k)
		}
		delete(want, k)
	}
}

func TestDisplayPath(t *testing.T) {
	if got := displayPath("/v1/charges/${encodeURIComponent(id)}"); got != "/v1/charges/{id}" {
		t.Errorf("displayPath = %q", got)
	}
	if got := displayPath("/2010-04-01/${sid}.json"); got != "/2010-04-01/{sid}.json" {
		t.Errorf("displayPath = %q", got)
	}
	if got := displayPath("/rest/api/3/issue/${parameters.issueIdOrKey}"); got != "/rest/api/3/issue/{issueIdOrKey}" {
		t.Errorf("displayPath = %q", got)
	}
}
