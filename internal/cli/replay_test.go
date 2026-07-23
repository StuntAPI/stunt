package cli

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/engine/dashboard"
	"stuntapi.com/stunt/internal/engine/requestlog"
)

// TestReplayCommandJSON POSTs a replay against an httptest dashboard with a
// stubbed ReplayFunc and asserts the replayed body is printed (--json mode).
func TestReplayCommandJSON(t *testing.T) {
	st, err := requestlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.Enqueue(requestlog.Entry{Seq: 1, Service: "api", Transport: "http",
		Method: "POST", Path: "/charge", Status: 200})
	st.Flush()
	orig, err := st.List(requestlog.Query{Limit: 1})
	if err != nil || len(orig) == 0 {
		t.Fatalf("List: err=%v len=%d", err, len(orig))
	}

	d := dashboard.New(st)
	d.SetReplayFunc(func(e requestlog.Entry) (int, string) { return 201, `{"replayed":true}` })
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	id := strconv.FormatInt(orig[0].ID, 10)
	var out bytes.Buffer
	if err := runReplay(&out, srv.URL, d.Token(), id, true /*json*/); err != nil {
		t.Fatalf("runReplay: %v", err)
	}
	if !strings.Contains(out.String(), `"replayed"`) {
		t.Fatalf("expected replayed body in json output:\n%s", out.String())
	}
}

// TestReplayCommandHuman asserts the human-readable summary includes the
// replayed body so a user can see what came back.
func TestReplayCommandHuman(t *testing.T) {
	st, err := requestlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.Enqueue(requestlog.Entry{Seq: 1, Service: "api", Transport: "http",
		Method: "POST", Path: "/charge", Status: 200})
	st.Flush()
	orig, err := st.List(requestlog.Query{Limit: 1})
	if err != nil || len(orig) == 0 {
		t.Fatalf("List: err=%v len=%d", err, len(orig))
	}

	d := dashboard.New(st)
	d.SetReplayFunc(func(e requestlog.Entry) (int, string) { return 201, `{"replayed":true}` })
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	id := strconv.FormatInt(orig[0].ID, 10)
	var out bytes.Buffer
	if err := runReplay(&out, srv.URL, d.Token(), id, false /*human*/); err != nil {
		t.Fatalf("runReplay: %v", err)
	}
	if !strings.Contains(out.String(), "replayed") || !strings.Contains(out.String(), "201") {
		t.Fatalf("expected human summary with status 201, output:\\n%s", out.String())
	}
}

// TestReplayCommandNoServer asserts the friendly error surfaces when --url is
// empty and no runtime file exists.
func TestReplayCommandNoServer(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	var out bytes.Buffer
	err := runReplayAuto(&out, "", "", manifestPath, "1", false)
	if err == nil {
		t.Fatalf("expected error for no running server")
	}
	if !strings.Contains(err.Error(), "stunt up") {
		t.Fatalf("expected friendly error mentioning `stunt up`, got: %v", err)
	}
}
