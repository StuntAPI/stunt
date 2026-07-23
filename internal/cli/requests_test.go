package cli

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/engine/dashboard"
	"stuntapi.com/stunt/internal/engine/requestlog"
)

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
	if err := runRequests(&out, srv.URL, d.Token(), true /*json*/, 5); err != nil {
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
	if err := runRequests(&out, srv.URL, d.Token(), false /*table*/, 5); err != nil {
		t.Fatalf("runRequests: %v", err)
	}
	if !strings.Contains(out.String(), "GET") || !strings.Contains(out.String(), "/x") {
		t.Fatalf("expected GET and /x in table output:\n%s", out.String())
	}
}

func TestRequestsCommandNoURL(t *testing.T) {
	var out bytes.Buffer
	err := runRequests(&out, "", "tok", false, 5)
	if err == nil {
		t.Fatalf("expected error for empty URL")
	}
}
