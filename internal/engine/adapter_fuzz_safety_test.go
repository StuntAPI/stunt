package engine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestAdapterInputSafety drives adversarial — but fully deterministic —
// requests at EVERY reference adapter's handler-backed routes and asserts
// the one invariant a mock must uphold: client input NEVER produces a
// 5xx. Starlark has no try/except, so any builtin error on attacker-shaped
// input surfaces as a 500 ("handler error"); real APIs answer bad input
// with 4xx. Each route is hit with its declared method, path params filled
// from a garbage pool, a garbage body (JSON nulls, malformed JSON, batch
// arrays, bracket-form), garbage auth, and a garbage query string.
//
// This is the seed corpus; FuzzAdapterRequests extends it with coverage-
// guided mutation over a curated adapter set.
func TestAdapterInputSafety(t *testing.T) {
	adaptersDir := repoAdaptersDir(t)
	if adaptersDir == "" {
		t.Skip("adapters/ directory not found — skipping reference-adapter safety sweep")
	}

	entries, err := os.ReadDir(adaptersDir)
	if err != nil {
		t.Skipf("cannot read adapters dir %s: %v", adaptersDir, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), "-style") {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	if len(dirs) == 0 {
		t.Skip("no *-style adapter directories found")
	}

	// Bound live engines: each subtest boots its own engine + SQLite.
	sem := make(chan struct{}, 6)
	for _, name := range dirs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sem <- struct{}{}
			defer func() { <-sem }()
			runAdapterSafetySweep(t, filepath.Join(adaptersDir, name))
		})
	}
}

// garbageParams fills {param} segments with values handlers must survive:
// negative numbers, huge numbers, unicode, empty-ish shapes, percent
// residues, assignment syntax. Slash-free so routes still MATCH (a slash
// would just 404 and test nothing).
var garbageParams = []string{
	"1", "0", "-1", "null", "abc", "🦀", "0x1F",
	"9999999999999999999999", "x=y", "%20", "undefined",
	"dddddddddddddddddddddddddddddddd", "{}", "[]", "id",
}

// garbageBodies cycles per endpoint: JSON null in required spots, nested
// nulls, a JSON-RPC batch array (the engine wraps it as _batch),
// malformed JSON (handler sees an empty body dict), bracket-form
// urlencoded, wrong-typed fields, unicode keys.
var garbageBodies = []struct {
	ct   string
	body string
}{
	{"application/json", `{"x": null}`},
	{"application/json", `{"a": {"b": null}, "id": null}`},
	{"application/json", `[1,2,3]`},
	{"application/json", `{"a":`},
	{"application/x-www-form-urlencoded", `a=1&b[c][]=2&d[e]=3&f=`},
	{"application/json", `{"id": 123, "amount": "1e3", "n": -0.5, "limit": -1}`},
	{"application/json", `{"🦀": "🦀"}`},
}

var garbageAuth = []string{
	"Bearer garbage-token",
	"Basic garbage",
	"",
}

// garbageQuery poisons every plausible cursor/limit/query param name at
// once (URL-encoded): adapters feed their own param name into paginate or
// a decoder, and a tampered cursor must answer 4xx, never a builtin raise.
const garbageQuery = "?cursor=%21%21%21bogus&pageToken=%21%21%21bogus" +
	"&starting_after=%21%21%21bogus&after=%21%21%21bogus&next=%21%21%21bogus" +
	"&offset=%21%21%21bogus&marker=%21%21%21bogus&position=%21%21%21bogus" +
	"&continuation=%21%21%21bogus&bookmark=%21%21%21bogus" +
	"&limit=99999999999999999999999&maxResults=99999999999999999999999" +
	"&maxresults=99999999999999999999999&per_page=99999999999999999999999" +
	"&pageSize=99999999999999999999999&q=%F0%9F%A6%80"

func runAdapterSafetySweep(t *testing.T, adapterDir string) {
	t.Helper()

	tmp := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(tmp, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{BasePort: 0},
		Services: map[string]manifest.Service{
			"svc": {Adapter: adapterDir},
		},
	}
	e, err := newEngine(m, t.TempDir())
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addrs, stop, err := e.ServeForTest(ctx)
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer stop()
	base := addrs["svc"]

	st, ok := e.states["svc"]
	if !ok {
		t.Fatalf("no service state for svc (adapter load error?)")
	}

	client := &http.Client{Timeout: 15 * time.Second}
	i := 0
	for _, ep := range st.adapter.Endpoints {
		if ep.Handler == "" {
			continue // rules-only endpoint: static responses, no handler to crash
		}
		method := ep.Method
		if method == "" {
			method = "GET"
		}
		path := fillRouteParams(ep.Route, i) + garbageQuery

		// Body-bearing verbs get EVERY garbage body (not a rotating
		// one); read verbs get one probe. Auth rotates per endpoint.
		bodiesForRoute := []struct{ ct, body string }{}
		if method == "POST" || method == "PUT" || method == "PATCH" {
			bodiesForRoute = append(bodiesForRoute, garbageBodies...)
		}
		if len(bodiesForRoute) == 0 {
			bodiesForRoute = append(bodiesForRoute, struct{ ct, body string }{"", ""})
		}

		for bi, gb := range bodiesForRoute {
			var body *bytes.Reader
			if gb.body != "" {
				body = bytes.NewReader([]byte(gb.body))
			} else {
				body = bytes.NewReader(nil)
			}
			req, err := http.NewRequest(method, base+path, body)
			if err != nil {
				// A garbage param can produce an unparseable URL; that
				// exercises the CLIENT, not the server. Skip.
				break
			}
			if auth := garbageAuth[(i+bi)%len(garbageAuth)]; auth != "" {
				req.Header.Set("Authorization", auth)
			}
			if gb.body != "" {
				req.Header.Set("Content-Type", gb.ct)
			}

			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("%s %s: request failed: %v", method, ep.Route, err)
				continue
			}
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			if resp.StatusCode >= 500 {
				t.Errorf("%s %s (filled %q, body #%d) -> %d: client input must never 5xx; body: %.400s",
					method, ep.Route, path, bi, resp.StatusCode, respBody)
			}
		}
		i++
	}
}

// fillRouteParams replaces every {name} in a route template with a
// deterministic garbage value (cycling by position + seed so different
// params on the same route get different poison).
func fillRouteParams(route string, seed int) string {
	var b strings.Builder
	n := 0
	for {
		open := strings.IndexByte(route, '{')
		if open < 0 {
			b.WriteString(route)
			return b.String()
		}
		end := strings.IndexByte(route[open:], '}')
		if end < 0 {
			b.WriteString(route)
			return b.String()
		}
		b.WriteString(route[:open])
		b.WriteString(garbageParams[(seed+n)%len(garbageParams)])
		route = route[open+end+1:]
		n++
	}
}
