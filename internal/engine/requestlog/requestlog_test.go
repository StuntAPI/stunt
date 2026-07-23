package requestlog_test

import (
	"path/filepath"
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

func TestStoreInsertAndList(t *testing.T) {
	dir := t.TempDir()
	st, err := requestlog.Open(filepath.Join(dir, "requests.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	in := requestlog.Entry{
		Seq: 1, Service: "api", Transport: "http", Method: "POST", Path: "/v1/charge",
		Status: 200, DurationMs: 12,
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
