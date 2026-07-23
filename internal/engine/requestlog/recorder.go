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
	seq     atomic.Int64
}

// NewRecorder builds a recorder bound to a service name.
func NewRecorder(st *Store, service string) *Recorder {
	return &Recorder{store: st, service: service}
}

// Wrap returns a handler that records then delegates to next.
func (r *Recorder) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
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
			DurationMs:  dur.Milliseconds(),
			ReqHeaders:  redactHeaders(req.Header),
			ReqBody:     capBody(reqBody),
			RespHeaders: headerJSON(rw.Header()),
			RespBody:    capBody(rw.buf.Bytes()),
		}
		r.store.Enqueue(e)
	})
}

func capBody(b []byte) string {
	if len(b) > maxBody {
		return string(b[:maxBody]) + "\n…[truncated]"
	}
	return string(b)
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

func (c *capturingWriter) Write(b []byte) (int, error) {
	c.buf.Write(b)
	return c.ResponseWriter.Write(b)
}
