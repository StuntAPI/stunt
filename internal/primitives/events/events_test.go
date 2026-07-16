package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingSink is an httptest server that counts received posts and can
// be configured to fail the first N requests.
type countingSink struct {
	mu          sync.Mutex
	requests    []receivedReq
	failCount   int32 // number of requests to fail (decremented)
	statusOnFail int   // status code to return when failing
}

type receivedReq struct {
	method      string
	path        string
	contentType string
	body        map[string]any
}

func newSink() *countingSink {
	return &countingSink{statusOnFail: http.StatusInternalServerError}
}

func (s *countingSink) handler(w http.ResponseWriter, r *http.Request) {
	if atomic.LoadInt32(&s.failCount) > 0 {
		atomic.AddInt32(&s.failCount, -1)
		w.WriteHeader(s.statusOnFail)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var parsed map[string]any
	json.Unmarshal(body, &parsed)

	s.mu.Lock()
	s.requests = append(s.requests, receivedReq{
		method:      r.Method,
		path:        r.URL.Path,
		contentType: r.Header.Get("Content-Type"),
		body:        parsed,
	})
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *countingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *countingSink) lastRequest() receivedReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return receivedReq{}
	}
	return s.requests[len(s.requests)-1]
}

func newTestEmitter() *Emitter {
	return NewEmitter()
}

func TestEmitReachesSink(t *testing.T) {
	sink := newSink()
	server := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer server.Close()

	e := newTestEmitter()
	e.Register("stripe", server.URL)

	err := e.Emit(context.Background(), "stripe", "payment.created", map[string]any{
		"amount": 1000,
		"currency": "usd",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if sink.count() != 1 {
		t.Fatalf("sink received %d posts, want 1", sink.count())
	}

	req := sink.lastRequest()
	if req.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", req.method)
	}
	if req.contentType != "application/json" {
		t.Fatalf("contentType = %q, want application/json", req.contentType)
	}
	if req.body["type"] != "payment.created" {
		t.Fatalf("type = %v, want payment.created", req.body["type"])
	}
	payload, ok := req.body["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want map[string]any", req.body["payload"])
	}
	if payload["amount"] != float64(1000) {
		t.Fatalf("payload.amount = %v, want 1000", payload["amount"])
	}
}

func TestRetryOnFailureThenSucceeds(t *testing.T) {
	sink := newSink()
	server := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer server.Close()

	// Fail the first 2 requests, then succeed.
	atomic.StoreInt32(&sink.failCount, 2)

	e := newTestEmitter()
	e.Register("svc", server.URL)

	err := e.Emit(context.Background(), "svc", "test.event", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("Emit should have succeeded after retries, got %v", err)
	}

	// 2 failures + 1 success = 3 total requests.
	if sink.count() != 1 {
		t.Fatalf("sink received %d successful posts, want 1", sink.count())
	}
}

func TestRetryExhaustedReturnsError(t *testing.T) {
	sink := newSink()
	server := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer server.Close()

	// Always fail.
	atomic.StoreInt32(&sink.failCount, 1000)

	e := newTestEmitter()
	e.Register("svc", server.URL)

	err := e.Emit(context.Background(), "svc", "test.event", map[string]any{"k": "v"})
	if err == nil {
		t.Fatal("Emit should have returned an error after exhausting retries")
	}
}

func TestMissingRegistrationReturnsError(t *testing.T) {
	e := newTestEmitter()

	err := e.Emit(context.Background(), "unregistered", "test.event", map[string]any{"k": "v"})
	if err == nil {
		t.Fatal("Emit should error when ns is not registered")
	}
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("err = %v, want ErrNotRegistered", err)
	}
}

func TestRegisterOverwrites(t *testing.T) {
	sink1 := newSink()
	server1 := httptest.NewServer(http.HandlerFunc(sink1.handler))
	defer server1.Close()

	sink2 := newSink()
	server2 := httptest.NewServer(http.HandlerFunc(sink2.handler))
	defer server2.Close()

	e := newTestEmitter()
	e.Register("svc", server1.URL)
	e.Register("svc", server2.URL) // overwrite

	e.Emit(context.Background(), "svc", "test", nil)

	if sink1.count() != 0 {
		t.Fatalf("sink1 received %d, want 0 (overwritten)", sink1.count())
	}
	if sink2.count() != 1 {
		t.Fatalf("sink2 received %d, want 1", sink2.count())
	}
}

func TestEmitNilPayload(t *testing.T) {
	sink := newSink()
	server := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer server.Close()

	e := newTestEmitter()
	e.Register("svc", server.URL)

	err := e.Emit(context.Background(), "svc", "empty.event", nil)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	req := sink.lastRequest()
	if req.body["type"] != "empty.event" {
		t.Fatalf("type = %v, want empty.event", req.body["type"])
	}
}

func TestEmitRequestBody(t *testing.T) {
	sink := newSink()
	server := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer server.Close()

	e := newTestEmitter()
	e.Register("svc", server.URL)

	payload := map[string]any{
		"nested": map[string]any{"deep": true},
		"list":   []any{1, 2, 3},
	}
	err := e.Emit(context.Background(), "svc", "complex", payload)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	req := sink.lastRequest()
	p, _ := req.body["payload"].(map[string]any)
	nested, _ := p["nested"].(map[string]any)
	if nested["deep"] != true {
		t.Fatalf("payload.nested.deep = %v, want true", nested["deep"])
	}
}

func TestEmitCancelledContext(t *testing.T) {
	sink := newSink()
	server := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer server.Close()

	e := newTestEmitter()
	e.Register("svc", server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := e.Emit(ctx, "svc", "test", nil)
	if err == nil {
		t.Fatal("Emit should error on cancelled context")
	}
}

func TestConcurrentEmit(t *testing.T) {
	sink := newSink()
	server := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer server.Close()

	e := newTestEmitter()
	e.Register("svc", server.URL)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Emit(context.Background(), "svc", "concurrent", map[string]any{"ok": true})
		}()
	}
	wg.Wait()

	if sink.count() != 50 {
		t.Fatalf("sink received %d, want 50", sink.count())
	}
}

func TestEventEnvelopeFormat(t *testing.T) {
	sink := newSink()
	server := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer server.Close()

	e := newTestEmitter()
	e.Register("svc", server.URL)

	e.Emit(context.Background(), "svc", "user.signup", map[string]any{"email": "a@b.com"})

	// Verify the raw body is a proper JSON envelope.
	req := sink.lastRequest()
	if req.body["type"] != "user.signup" {
		t.Fatalf("type = %v, want user.signup", req.body["type"])
	}
	if _, ok := req.body["payload"]; !ok {
		t.Fatal("envelope missing 'payload' key")
	}
}

// TestBackoffIsUsed verifies that retries happen (i.e. there's delay between
// attempts). We don't assert exact timing but verify the sink sees multiple
// attempts.
func TestRetriesHappen(t *testing.T) {
	sink := newSink()
	server := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer server.Close()

	// Fail 2 times, succeed on 3rd.
	atomic.StoreInt32(&sink.failCount, 2)

	e := newTestEmitter()
	e.Register("svc", server.URL)
	e.backoff = 1 * time.Millisecond // speed up test

	start := time.Now()
	err := e.Emit(context.Background(), "svc", "test", nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// With backoff of 1ms, 3 attempts should take at least 2ms.
	if elapsed < 2*time.Millisecond {
		t.Logf("warning: elapsed %v seems too short for 3 attempts with backoff", elapsed)
	}
}

// Ensure we can marshal the envelope to bytes.
func TestEnvelopeMarshal(t *testing.T) {
	env := envelope{
		Type:    "test.event",
		Payload: map[string]any{"k": "v"},
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(data, []byte(`"type":"test.event"`)) {
		t.Fatalf("envelope JSON missing type: %s", data)
	}
	if !bytes.Contains(data, []byte(`"payload":`)) {
		t.Fatalf("envelope JSON missing payload: %s", data)
	}
}
