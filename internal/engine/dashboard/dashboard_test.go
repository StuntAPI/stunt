package dashboard_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"stuntapi.com/stunt/internal/engine/dashboard"
	"stuntapi.com/stunt/internal/engine/requestlog"
)

func TestRequestsAPIRequiresToken(t *testing.T) {
	d := dashboard.New(dummyStore(t))
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/api/requests")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", res.StatusCode)
	}
}

func TestRequestsAPIReturnsJSON(t *testing.T) {
	d := dashboard.New(dummyStore(t))
	d.SetTokenForTest("tok")
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/requests?limit=5", nil)
	req.Header.Set("X-Stunt-Token", "tok")
	req.Host = "localhost" // satisfy Host guard
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", res.StatusCode)
	}
	var out []requestlog.Entry
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("expected entries, got 0")
	}
}

func TestHostGuardRejectsForeignHost(t *testing.T) {
	d := dashboard.New(dummyStore(t))
	d.SetTokenForTest("tok")
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/requests", nil)
	req.Header.Set("X-Stunt-Token", "tok")
	req.Host = "evil.example.com"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 for foreign Host, got %d", res.StatusCode)
	}
}

func TestStreamLiveFeed(t *testing.T) {
	st, err := requestlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("requestlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := dashboard.New(st)
	d.SetTokenForTest("tok")
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	// ws dial (coder/websocket) with the token header. httptest.NewServer
	// listens on 127.0.0.1, which passes the Host guard.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/requests/stream"
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Stunt-Token": {"tok"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// Capture one request after subscribing: the live feed must deliver it.
	st.Enqueue(requestlog.Entry{Seq: 42, Service: "s", Method: "GET", Path: "/live", Status: 200})
	st.Flush()

	_, msg, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(msg), `"/live"`) {
		t.Fatalf("expected /live in %s", msg)
	}
}

func TestReplay(t *testing.T) {
	st, err := requestlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("requestlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.Enqueue(requestlog.Entry{Seq: 1, Service: "s", Method: "GET", Path: "/x", Status: 200})
	st.Flush()
	orig, err := st.List(requestlog.Query{Limit: 1})
	if err != nil || len(orig) == 0 {
		t.Fatalf("List: err=%v len=%d", err, len(orig))
	}

	d := dashboard.New(st)
	d.SetTokenForTest("tok")
	d.SetReplayFunc(func(e requestlog.Entry) (int, string) { return 201, `{"replayed":true}` })
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/requests/"+strconv.FormatInt(orig[0].ID, 10)+"/replay", nil)
	req.Header.Set("X-Stunt-Token", "tok")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("want 200, got %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `"replayed":true`) {
		t.Fatalf("unexpected body %s", body)
	}
}

// TestIndexRendersLiveInspector asserts the server-rendered dashboard page:
//   - returns 200 when authed via the bootstrap cookie,
//   - contains the live ws endpoint URL (so the JS client is present),
//   - still renders the requests table (no-JS / first-paint fallback),
//   - NEVER echoes the auth token or cookie name into the HTML.
func TestIndexRendersLiveInspector(t *testing.T) {
	d := dashboard.New(dummyStore(t))
	d.SetTokenForTest("live-inspector-token")
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	// Bootstrap the cookie (browser-style) so the follow-up GET / is authed.
	client := noRedirectClient()
	res, err := client.Get(srv.URL + "/?token=live-inspector-token")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	res.Body.Close()

	// GET / carrying the cookie (no header) — the browser path.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	for _, c := range res.Cookies() {
		req.AddCookie(c)
	}
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", res2.StatusCode)
	}
	body, _ := io.ReadAll(res2.Body)

	// The live ws endpoint must be wired into the inline client JS.
	if !strings.Contains(string(body), "/api/requests/stream") {
		t.Fatalf("rendered page must reference the live ws endpoint")
	}
	// The server-rendered requests table must still be present (first paint).
	if !strings.Contains(string(body), "class=\"reqs\"") {
		t.Fatalf("rendered page must contain the requests table")
	}
	// The auth token must NEVER be echoed into the HTML body.
	if strings.Contains(string(body), "live-inspector-token") {
		t.Fatalf("auth token must not be echoed in the HTML")
	}
	if strings.Contains(string(body), "stunt_token") {
		t.Fatalf("cookie name must not be echoed in the HTML")
	}
}

// dummyStore opens a requestlog store in a temp dir, inserts one entry, and
// returns it. The returned store's Close is registered with t.Cleanup.
func dummyStore(t *testing.T) *requestlog.Store {
	t.Helper()
	st, err := requestlog.Open(filepath.Join(t.TempDir(), "requests.db"))
	if err != nil {
		t.Fatalf("requestlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.Enqueue(requestlog.Entry{
		Seq: 1, Service: "api", Transport: "http", Method: "GET", Path: "/v1/widgets",
		Status: 200, DurationUs: 7000,
	})
	st.Flush()
	return st
}

func TestStateEndpoints(t *testing.T) {
	st, _ := requestlog.Open(filepath.Join(t.TempDir(), "r.db"))
	t.Cleanup(func() { _ = st.Close() })
	d := dashboard.New(st); d.SetTokenForTest("tok")
	d.SetServices([]string{"api"})
	d.SetState(
		func(svc string) (dashboard.ServiceState, bool) {
			if svc != "api" { return dashboard.ServiceState{}, false }
			return dashboard.ServiceState{
				Collections: []dashboard.CollectionInfo{{Name: "orders", Count: 2}},
				KVNames:     []string{"cfg"},
				BlobNames:   []string{"uploads"},
			}, true
		},
		func(svc, name string) ([]map[string]any, error) { return []map[string]any{{"id": "o1"}}, nil },
		func(svc, ns string) ([][2]string, error) { return [][2]string{{"k", "v"}}, nil },
		func(svc, ns string) ([]dashboard.BlobInfo, error) { return []dashboard.BlobInfo{{Name: "f.txt", Size: 2}}, nil },
		func(svc string) error { return nil },
	)
	srv := httptest.NewServer(d.Handler()); t.Cleanup(srv.Close)
	get := func(p string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+p, nil)
		req.Header.Set("X-Stunt-Token", "tok")
		res, _ := http.DefaultClient.Do(req)
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	for _, c := range []struct{ path, want string }{
		{"/api/state", "services"},
		{"/api/state/api", "orders"},
		{"/api/state/api/collections", "orders"},
		{"/api/state/api/collections/orders", "o1"},
		{"/api/state/api/kv", "cfg"},
		{"/api/state/api/kv/cfg", "\"k\""},
		{"/api/state/api/blobs", "f.txt"},
	} {
		code, body := get(c.path)
		if code != 200 || !strings.Contains(body, c.want) {
			t.Errorf("%s: got %d %q (want 200 containing %q)", c.path, code, body, c.want)
		}
	}
	// reset (POST)
	req, _ := http.NewRequest("POST", srv.URL+"/api/state/reset", nil)
	req.Header.Set("X-Stunt-Token", "tok")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 200 { t.Errorf("reset all: %d", res.StatusCode) }
}
