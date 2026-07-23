package requestlog_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"stuntapi.com/stunt/internal/engine/requestlog"
)

func TestAsyncWriteAndRingEviction(t *testing.T) {
	dir := t.TempDir()
	st, err := requestlog.Open(filepath.Join(dir, "requests.db"),
		requestlog.WithRing(3))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for i := 1; i <= 5; i++ { // write 5, ring keeps 3
		st.Enqueue(requestlog.Entry{Seq: int64(i), Service: "s", Transport: "http",
			Method: "GET", Path: "/x", Status: 200})
	}
	st.Flush() // wait for the writer to drain

	got, err := st.List(requestlog.Query{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 || got[0].Seq != 5 || got[2].Seq != 3 {
		t.Fatalf("ring eviction wrong: %+v", got)
	}
}

func TestRecorderCapturesAndRedacts(t *testing.T) {
	dir := t.TempDir()
	st, _ := requestlog.Open(filepath.Join(dir, "requests.db"))
	t.Cleanup(func() { _ = st.Close() })

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"ch_1"}`))
	})
	rec := requestlog.NewRecorder(st, "api", new(atomic.Int64))
	h := rec.Wrap(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/charge",
		strings.NewReader(`{"amount":100}`))
	req.Header.Set("Authorization", "Bearer sk_test_abc")
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	st.Flush()
	got, _ := st.List(requestlog.Query{Limit: 5})
	if len(got) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got))
	}
	e := got[0]
	if e.Method != "POST" || e.Path != "/v1/charge" || e.Status != 201 {
		t.Fatalf("capture wrong: %+v", e)
	}
	if strings.Contains(e.ReqHeaders, "sk_test_abc") {
		t.Fatalf("Authorization not redacted: %s", e.ReqHeaders)
	}
	if !strings.Contains(e.ReqHeaders, "[REDACTED]") {
		t.Fatalf("expected [REDACTED]: %s", e.ReqHeaders)
	}
}

func TestStoreInsertAndList(t *testing.T) {
	dir := t.TempDir()
	st, err := requestlog.Open(filepath.Join(dir, "requests.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	in := requestlog.Entry{
		Seq: 1, Service: "api", Transport: "http", Method: "POST", Path: "/v1/charge",
		Status: 200, DurationUs: 12000,
		ReqHeaders:  `{"Authorization":"[REDACTED]","Content-Type":"application/json"}`,
		ReqBody:     `{"amount":100}`,
		RespHeaders: `{"Content-Type":"application/json"}`,
		RespBody:    `{"id":"ch_1"}`,
	}
	if err := st.Insert(in); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := st.List(requestlog.Query{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Path != "/v1/charge" || got[0].Status != 200 {
		t.Fatalf("unexpected: %+v", got)
	}
}
