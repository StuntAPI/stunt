package engine

import (
	"bufio"
	"log"
	"net"
	"net/http"
	"time"
)

// statusRecorder wraps http.ResponseWriter to capture the status code for
// request logging. It proxies the Hijacker interface so WebSocket upgrades
// work transparently through the logging middleware.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// Hijack proxies the underlying ResponseWriter's Hijack method so WebSocket
// upgrades and connection hijacking work through the logging middleware.
// A ResponseController walks the inner writers' Unwrap chain — asserting
// the immediate inner writer directly fails when a middleware between us
// and the real writer (e.g. the request-log capture wrapper) implements
// only Unwrap, which silently degraded behavior:timeout into a clean 504.
func (sr *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(sr.ResponseWriter).Hijack()
}

// requestLogger returns middleware that logs each request to the given
// logger in the format:  <service> GET /path 200 1.2ms
func requestLogger(serviceName string, lg *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sr := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(sr, r)
			lg.Printf("%s %s %s %d %s", serviceName, r.Method, r.URL.Path, sr.status, time.Since(start).Round(time.Millisecond))
		})
	}
}
