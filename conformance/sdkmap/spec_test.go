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
