package sdkmap

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSurface(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSurfaceGuards(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"zero routes", `{"source":"spec x @ 1","url":"https://x","fetched":"2026-08-23","routes":[]}`},
		{"no provenance", `{"routes":[{"method":"GET","route":"/a"}]}`},
		{"relative path", `{"source":"s","url":"u","fetched":"f","routes":[{"method":"GET","route":"a"}]}`},
		{"bad verb", `{"source":"s","url":"u","fetched":"f","routes":[{"method":"FETCH","route":"/a"}]}`},
	}
	for _, c := range cases {
		if _, err := LoadSurface(writeSurface(t, c.body)); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	ok := `{"source":"spec x @ 1","url":"https://x","fetched":"2026-08-23","routes":[{"method":"get","route":"/a"},{"method":"POST","route":"/b"}]}`
	f, err := LoadSurface(writeSurface(t, ok))
	if err != nil {
		t.Fatalf("valid surface rejected: %v", err)
	}
	if f.Routes[0].Method != "get" || len(f.Routes) != 2 {
		t.Errorf("round-trip: %+v", f.Routes)
	}
}

// Every committed artifact must stay loadable — the registry contract
// genmatrix relies on, checked where the files live.
func TestCommittedSurfacesLoad(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "surfaces", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no vendored surfaces in this checkout")
	}
	for _, f := range files {
		if _, err := LoadSurface(f); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
}

// The IDL-derived artifacts (Stone, lexicon) must keep their parsers
// honest: flagship routes of each flavor — versioned stone name, plain
// stone namespace, lexicon procedure, lexicon query — must be present.
func TestCommittedIDLSurfaces(t *testing.T) {
	cases := []struct{ file, route string }{
		{"dropbox-style.json", "POST /2/files/upload"},
		{"dropbox-style.json", "POST /2/users/get_current_account"},
		{"bluesky-style.json", "POST /xrpc/com.atproto.server.createSession"},
		{"bluesky-style.json", "GET /xrpc/app.bsky.feed.searchPosts"},
	}
	for _, c := range cases {
		f, err := LoadSurface(filepath.Join("..", "surfaces", c.file))
		if err != nil {
			if os.IsNotExist(err) {
				t.Skipf("%s not vendored in this checkout", c.file)
			}
			t.Fatal(err)
		}
		have := map[string]bool{}
		for _, r := range f.Routes {
			have[r.Method+" "+r.Path] = true
		}
		if !have[c.route] {
			t.Errorf("%s: missing known route %q", c.file, c.route)
		}
	}
}

// Committed surface artifacts must not shrink without a deliberate edit here.
//
// These files are vendored provider route tables, refreshed only by
// `just surfaces-fetch`, and genmatrix publishes `provider_routes`,
// `covered_routes` and a coverage percentage derived from them. Nothing
// validated their size: trimming adyen-style.json from 39 routes to 2 left the
// artifact loadable and republished coverage as 23% → 50%, while 30 real routes
// silently reclassified from "documented not-implemented" to "not part of the
// API". The denominator is the number the whole coverage claim rests on, and it
// was the easiest quantity in the repository to move.
//
// These are FLOORS, not exact counts. A re-fetch that adds routes — the normal
// outcome of a spec refresh — needs no edit here. Only a shrink does, which is
// the point: it should take a deliberate act to make coverage look better by
// making the denominator smaller.
var committedRouteCountFloors = map[string]int{
	"adyen-style.json":                 39,
	"apple-appstoreconnect-style.json": 1263,
	"auth0-style.json":                 469,
	"azure-devops-style.json":          305,
	"azure-storage-style.json":         69,
	"bluesky-style.json":               202,
	"cloudflare-style.json":            3334,
	"discord-style.json":               242,
	"dropbox-style.json":               72,
	"entra-id-style.json":              17531,
	"fattureincloud-style.json":        123,
	"microsoft-graph-style.json":       17531,
	"onfido-style.json":                84,
	"opensea-style.json":               139,
	"paypal-style.json":                17,
	"persona-style.json":               213,
	"printful-style.json":              55,
	"printify-style.json":              40,
	"revenuecat-style.json":            15,
	"sendgrid-style.json":              26,
	"twitter-style.json":               178,
	"x-articles-style.json":            178,
	"xero-style.json":                  235,
}

func TestCommittedSurfacesDoNotShrink(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "surfaces", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no vendored surfaces in this checkout")
	}
	seen := map[string]bool{}
	total := 0
	for _, f := range files {
		base := filepath.Base(f)
		floor, known := committedRouteCountFloors[base]
		if !known {
			t.Errorf("%s: committed surface has no route-count floor — add one", base)
			continue
		}
		seen[base] = true
		s, err := LoadSurface(f)
		if err != nil {
			t.Errorf("%s: %v", base, err)
			continue
		}
		total += len(s.Routes)
		if len(s.Routes) < floor {
			t.Errorf("%s: %d routes, floor is %d — the provider denominator shrank; if that is correct, lower the floor deliberately",
				base, len(s.Routes), floor)
		}
	}
	for base := range committedRouteCountFloors {
		if !seen[base] {
			t.Errorf("%s: floor declared but no such artifact is committed", base)
		}
	}
	if total < 42360 {
		t.Errorf("total committed routes = %d, floor is 42360 — one artifact cannot be inflated to offset another's shrink", total)
	}
}
