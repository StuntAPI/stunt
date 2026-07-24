package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestSnapshotSaveLoadCLI(t *testing.T) {
	// Minimal httptest server emulating the snapshot/restore endpoints.
	mux := http.NewServeMux()
	snapshotBytes := []byte{0x1f, 0x8b, 0x08, 0x00, 'F', 'A', 'K', 'E'} // gzip-ish
	var uploaded []byte
	mux.HandleFunc("/api/state/snapshot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(snapshotBytes)
	})
	mux.HandleFunc("/api/state/restore", func(w http.ResponseWriter, r *http.Request) {
		uploaded, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"restored":true}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// save
	outPath := filepath.Join(t.TempDir(), "snap.tar.gz")
	var out bytes.Buffer
	if err := runSnapshotSave(&out, srv.URL, "tok", outPath, true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(outPath)
	if !bytes.Equal(got, snapshotBytes) {
		t.Fatalf("saved archive = %v, want %v", got, snapshotBytes)
	}

	// load
	out.Reset()
	if err := runSnapshotLoad(&out, srv.URL, "tok", outPath, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(uploaded, snapshotBytes) {
		t.Fatalf("uploaded archive = %v, want %v", uploaded, snapshotBytes)
	}
}
