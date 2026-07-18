package engine

import (
	"bufio"
	"fmt"
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
func (sr *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := sr.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return h.Hijack()
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
