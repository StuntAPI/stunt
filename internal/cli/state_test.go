package cli

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/engine/dashboard"
	"stuntapi.com/stunt/internal/engine/requestlog"
)

func newDashboardServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	st, _ := requestlog.Open(t.TempDir() + "/r.db")
	t.Cleanup(func() { _ = st.Close() })
	d := dashboard.New(st)
	d.SetTokenForTest("tok")
	d.SetServices([]string{"api"})
	d.SetState(
		func(svc string) (dashboard.ServiceState, bool) {
			return dashboard.ServiceState{Collections: []dashboard.CollectionInfo{{Name: "orders", Count: 3}}, KVNames: []string{"cfg"}}, true
		},
		func(svc, name string) ([]map[string]any, error) { return []map[string]any{{"id": "o1"}}, nil },
		func(svc, ns string) ([][2]string, error) { return [][2]string{{"k", "v"}}, nil },
		func(svc, ns string) ([]dashboard.BlobInfo, error) { return nil, nil },
		func(svc string) error { return nil },
	)
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	return srv, "tok"
}

func TestStateCollectionsCLI(t *testing.T) {
	srv, tok := newDashboardServer(t)
	var out bytes.Buffer
	if err := runStateCollections(&out, srv.URL, tok, "api", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "orders") {
		t.Fatalf("expected 'orders' in: %s", out.String())
	}
}

func TestResetCLI(t *testing.T) {
	srv, tok := newDashboardServer(t)
	var out bytes.Buffer
	if err := runReset(&out, srv.URL, tok, "api", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "reset") {
		t.Fatalf("expected reset confirmation: %s", out.String())
	}
}
