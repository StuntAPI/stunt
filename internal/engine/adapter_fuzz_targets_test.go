package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// fuzzAdapters is the curated deep-fuzz set — one process boots one engine
// per adapter (fuzz workers are separate processes), so the set trades
// breadth for a rich mutation substrate: the largest REST surface
// (stripe-style), SQL text (cloudflare-style D1), SOQL (salesforce-style),
// OData (powerplatform-style), RFC 7807 (emailoctopus-style), JSON-RPC
// batches (eth-jsonrpc-style), bracket-form bodies (shopify-style).
var fuzzAdapters = []string{
	"stripe-style",
	"cloudflare-style",
	"salesforce-style",
	"powerplatform-style",
	"emailoctopus-style",
	"eth-jsonrpc-style",
	"shopify-style",
}

type fuzzServer struct {
	base   string
	client *http.Client
}

var (
	fuzzSrvMu  sync.Mutex
	fuzzSrvs   = map[string]*fuzzServer{}
	fuzzSrvDir string
)

// fuzzServerFor lazily boots the adapter's engine once per process (per
// fuzz worker). State accumulates garbage across inputs — that is the
// point; the dirs live under os.TempDir for the process lifetime because
// t.TempDir would be reaped after every input.
func fuzzServerFor(t *testing.T, name string) *fuzzServer {
	t.Helper()
	fuzzSrvMu.Lock()
	defer fuzzSrvMu.Unlock()
	if srv, ok := fuzzSrvs[name]; ok {
		return srv
	}

	if fuzzSrvDir == "" {
		var err error
		fuzzSrvDir, err = os.MkdirTemp("", "stunt-fuzz-*")
		if err != nil {
			t.Fatalf("tmp dir: %v", err)
		}
	}
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(adapterDir); err != nil {
		t.Skipf("adapter %s not present", name)
	}

	stateDir, err := os.MkdirTemp(fuzzSrvDir, "state-*")
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{BasePort: 0},
		Services: map[string]manifest.Service{
			"svc": {Adapter: adapterDir},
		},
	}
	e, err := newEngine(m, stateDir)
	if err != nil {
		t.Fatalf("engine.New(%s): %v", name, err)
	}
	addrs, stop, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest(%s): %v", name, err)
	}
	// Engine + listener live for the process; nothing to stop.
	_ = stop

	srv := &fuzzServer{base: addrs["svc"], client: &http.Client{Timeout: 15 * time.Second}}
	fuzzSrvs[name] = srv
	return srv
}

// fuzzSafePath percent-encodes any byte outside RFC 3986's path pchar set
// (keeping '/' structure) so every mutated path is still a valid request —
// the fuzzer exercises the router and handlers, not the HTTP client.
func fuzzSafePath(p string) string {
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '/' || c == '-' || c == '.' || c == '_' || c == '~' ||
			c == ':' || c == '@' || c == '!' || c == '$' || c == '&' || c == '\'' ||
			c == '(' || c == ')' || c == '*' || c == '+' || c == ',' || c == ';' || c == '=' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// FuzzAdapterRequests drives coverage-guided mutated requests through the
// full HTTP dispatch path (routing, body classification, Starlark handler,
// response marshal) of the curated adapter set. Invariant: client input
// never yields a 5xx — the same contract as TestAdapterInputSafety, but
// with the fuzzer inventing the inputs.
func FuzzAdapterRequests(f *testing.F) {
	for _, s := range []struct {
		idx    int
		method string
		path   string
		body   string
	}{
		{0, "GET", "/v1/charges", ""},
		{0, "POST", "/v1/charges", `{"amount": 1000, "currency": "usd"}`},
		{0, "POST", "/v1/payment_intents", "amount=1000&currency=usd"},
		{1, "GET", "/zones", ""},
		{1, "POST", "/client/v4/accounts/acc/d1/database/db/query", `{"sql": "SELECT * FROM t"}`},
		{2, "GET", "/services/data/v60.0/query", ""},
		{2, "POST", "/services/oauth2/token", "grant_type=password&username=u&password=p"},
		{3, "GET", "/accounts", ""},
		{4, "GET", "/lists", ""},
		{4, "POST", "/lists", `{"name": "x"}`},
		{5, "POST", "/", `{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`},
		{5, "POST", "/", `[{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}]`},
		{6, "POST", "/admin/api/2024-10/orders.json", `{"order":{"line_items":[{"title":"x"}]}}`},
		{6, "GET", "/admin/api/2024-10/orders.json", ""},
	} {
		f.Add(s.idx, s.method, s.path, []byte(s.body))
	}
	f.Fuzz(func(t *testing.T, adapterIdx int, method, path string, body []byte) {
		name := fuzzAdapters[abs(adapterIdx)%len(fuzzAdapters)]
		srv := fuzzServerFor(t, name)

		req, err := http.NewRequest(method, srv.base+fuzzSafePath(path), bytes.NewReader(body))
		if err != nil {
			return // unparseable method line — the HTTP client rejects it
		}
		if len(body) > 0 {
			if body[0] == '{' || body[0] == '[' {
				req.Header.Set("Content-Type", "application/json")
			} else {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
		}
		resp, err := srv.client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			t.Fatalf("%s %s %s (adapter %s) -> %d: client input must never 5xx; body: %.400s",
				method, path, truncate(body, 200), name, resp.StatusCode, respBody)
		}
	})
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
