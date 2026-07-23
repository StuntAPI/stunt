package requestlog

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"proxy-authorization": true,
}

const maxBody = 64 << 10 // 64 KB

// Recorder wraps a handler to capture req/resp into the store.
type Recorder struct {
	store   *Store
	service string
	seq     *atomic.Int64 // shared global sequence (engine-wide) → unique across services
}

// NewRecorder builds a recorder bound to a service name. seq is a shared,
// engine-wide monotonic counter so entry Seq values are globally unique
// (needed for the live-feed gap-free ordering in Plan 2 and unique row labels).
func NewRecorder(st *Store, service string, seq *atomic.Int64) *Recorder {
	return &Recorder{store: st, service: service, seq: seq}
}

// Wrap returns a handler that records then delegates to next.
func (r *Recorder) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// WebSocket upgrades are long-lived hijacked connections that do not fit
		// the request/response capture model (and reading req.Body / wrapping
		// the ResponseWriter would break the hijack). Delegate directly.
		if isWebSocketUpgrade(req) {
			next.ServeHTTP(w, req)
			return
		}
		start := time.Now()
		var reqBody []byte
		if req.Body != nil {
			reqBody, _ = io.ReadAll(io.LimitReader(req.Body, maxBody+1))
			req.Body = io.NopCloser(bytes.NewReader(reqBody))
		}
		rw := &capturingWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, req)

		dur := time.Since(start)
		e := Entry{
			Seq:         r.seq.Add(1),
			Service:     r.service,
			Transport:   "http",
			Method:      req.Method,
			Path:        req.URL.Path,
			Status:      rw.status,
			DurationUs:  dur.Microseconds(),
			ReqHeaders:  redactHeaders(req.Header),
			ReqBody:     capBody(reqBody),
			RespHeaders: headerJSON(rw.Header()),
			RespBody:    capBody(rw.buf.Bytes()),
		}
		if r.store != nil {
			r.store.Enqueue(e)
		}
	})
}

func capBody(b []byte) string {
	if len(b) > maxBody {
		return string(b[:maxBody]) + "\n…[truncated]"
	}
	return string(b)
}

// isWebSocketUpgrade reports whether the request is a WebSocket upgrade
// (RFC 6455 §4.1). Such requests hijack the connection and must bypass
// capture (mirrors engine.isWebSocketUpgrade, kept local to avoid a cycle).
func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, part := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
			return true
		}
	}
	return false
}

func redactHeaders(h http.Header) string {
	cp := h.Clone()
	for k := range cp {
		if sensitiveHeaders[strings.ToLower(k)] {
			cp[k] = []string{"[REDACTED]"}
		}
	}
	return headerJSON(cp)
}

func headerJSON(h http.Header) string {
	b, _ := json.Marshal(h)
	return string(b)
}

// capturingWriter records the status code and response body.
type capturingWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (c *capturingWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the underlying ResponseWriter so http.ResponseController can
// reach Hijack/Flush/etc. on the original writer through this wrapper.
func (c *capturingWriter) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}

func (c *capturingWriter) Write(b []byte) (int, error) {
	c.buf.Write(b)
	return c.ResponseWriter.Write(b)
}
