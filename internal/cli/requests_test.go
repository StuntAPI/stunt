package cli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/engine/dashboard"
	"stuntapi.com/stunt/internal/engine/requestlog"
)

// safeBuffer is a concurrency-safe bytes.Buffer for tests that write from one
// goroutine (runFollow) and read from another (the test's poll loop).
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestRequestsCommandJSON(t *testing.T) {
	st, err := requestlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.Enqueue(requestlog.Entry{Seq: 1, Service: "api", Transport: "http",
		Method: "GET", Path: "/x", Status: 200})
	st.Flush()
	d := dashboard.New(st)
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	if err := runRequests(&out, srv.URL, d.Token(), "", true /*json*/, 5); err != nil {
		t.Fatalf("runRequests: %v", err)
	}
	if !strings.Contains(out.String(), `"/x"`) {
		t.Fatalf("expected /x in json output:\n%s", out.String())
	}
}

func TestRequestsCommandTable(t *testing.T) {
	st, err := requestlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.Enqueue(requestlog.Entry{Seq: 1, Service: "api", Transport: "http",
		Method: "GET", Path: "/x", Status: 200})
	st.Flush()
	d := dashboard.New(st)
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	if err := runRequests(&out, srv.URL, d.Token(), "", false /*table*/, 5); err != nil {
		t.Fatalf("runRequests: %v", err)
	}
	if !strings.Contains(out.String(), "GET") || !strings.Contains(out.String(), "/x") {
		t.Fatalf("expected GET and /x in table output:\n%s", out.String())
	}
}

// TestRequestsCommandNoServer asserts that when --url is empty and no runtime
// file exists for the manifest dir, runRequests returns a friendly error
// pointing the user at `stunt up`.
func TestRequestsCommandNoServer(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	var out bytes.Buffer
	err := runRequests(&out, "", "", manifestPath, false, 5)
	if err == nil {
		t.Fatalf("expected error for no running server")
	}
	if !strings.Contains(err.Error(), "stunt up") {
		t.Fatalf("expected friendly error mentioning `stunt up`, got: %v", err)
	}
}

// TestRequestsCommandAutoDiscover asserts that when --url/--token are empty,
// runRequests resolves the dashboard URL+token from the manifest dir's runtime
// file (written by `stunt up`) and hits that server.
func TestRequestsCommandAutoDiscover(t *testing.T) {
	st, err := requestlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.Enqueue(requestlog.Entry{Seq: 1, Service: "api", Transport: "http",
		Method: "GET", Path: "/auto-discovered", Status: 200})
	st.Flush()
	d := dashboard.New(st)
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	if err := writeRuntimeFile(manifestDir(manifestPath), RuntimeFile{
		Manifest:       manifestPath,
		Mode:           "port",
		DashboardURL:   srv.URL,
		DashboardToken: d.Token(),
	}); err != nil {
		t.Fatalf("writeRuntimeFile: %v", err)
	}

	var out bytes.Buffer
	if err := runRequests(&out, "", "", manifestPath, false, 5); err != nil {
		t.Fatalf("runRequests auto-discover: %v", err)
	}
	if !strings.Contains(out.String(), "/auto-discovered") {
		t.Fatalf("expected auto-discovered server to be hit, output:\n%s", out.String())
	}
}

// TestRequestsFollow drives runFollow against a live dashboard: it dials the
// stream, we publish one entry, and the entry's path must appear in the
// captured output within a short window. The context-bound deadline keeps the
// test from hanging; cancelling the context must stop runFollow cleanly.
func TestRequestsFollow(t *testing.T) {
	st, err := requestlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := dashboard.New(st)
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	// Short timeout so a broken feature fails fast instead of hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var out safeBuffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runFollow(ctx, &out, srv.URL, d.Token(), false /*table*/)
	}()

	// The dashboard subscribes to the bus only AFTER websocket.Accept returns,
	// so an entry published immediately after dial can race the subscription
	// and be dropped. Retry-publish until runFollow surfaces the entry.
	const want = "/followed"
	for seq := int64(1); !strings.Contains(out.String(), want) && ctx.Err() == nil; seq++ {
		st.Enqueue(requestlog.Entry{Seq: seq, Service: "api", Transport: "http",
			Method: "GET", Path: want, Status: 200})
		st.Flush()
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out.String(), want) {
		t.Fatalf("expected %q in follow output within 2s:\n%s", want, out.String())
	}

	cancel() // Ctrl-C equivalent: must stop the loop.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runFollow did not stop after context cancel")
	}
}

// TestRequestsFollowJSON asserts that with --json each event is printed as raw
// JSON on its own line.
func TestRequestsFollowJSON(t *testing.T) {
	st, err := requestlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := dashboard.New(st)
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var out safeBuffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runFollow(ctx, &out, srv.URL, d.Token(), true /*json*/)
	}()

	const want = "/json-event"
	for seq := int64(1); !strings.Contains(out.String(), want) && ctx.Err() == nil; seq++ {
		st.Enqueue(requestlog.Entry{Seq: seq, Service: "api", Transport: "http",
			Method: "POST", Path: want, Status: 201})
		st.Flush()
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out.String(), want) {
		t.Fatalf("expected %q in json follow output within 2s:\n%s", want, out.String())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runFollow did not stop after context cancel")
	}
}
