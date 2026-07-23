package dashboard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
