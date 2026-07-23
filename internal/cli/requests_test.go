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
