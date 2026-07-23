// Package dashboard serves the localhost admin UI + JSON API for a running engine.
//
// It is bound to a requestlog.Store and exposes:
//   - GET /api/requests — filterable JSON history
//   - GET /            — minimal server-rendered history page
//
// Every request must pass two guards: a DNS-rebinding Host-header check
// (loopback hosts only) and an auth-token check (X-Stunt-Token header).
package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"

	"stuntapi.com/stunt/internal/engine/dashboard/assets"
	"stuntapi.com/stunt/internal/engine/requestlog"
)

// allowedHosts are the loopback hostnames permitted by the Host guard. The
// port is stripped from r.Host before lookup, so "127.0.0.1:41257" matches.
var allowedHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"[::1]":     true,
}

// Dashboard is the admin server bound to a request log store.
type Dashboard struct {
	store *requestlog.Store
	token string
	tmpl  *template.Template
}

// New builds a dashboard over store and generates a fresh auth token.
func New(store *requestlog.Store) *Dashboard {
	tmpl := template.Must(template.ParseFS(assets.Templates, "dashboard.tmpl"))
	return &Dashboard{store: store, token: newToken(), tmpl: tmpl}
}

// Token returns the session auth token (printed to the terminal by the CLI).
func (d *Dashboard) Token() string { return d.token }

// SetTokenForTest sets the token (test seam).
func (d *Dashboard) SetTokenForTest(t string) { d.token = t }

// Handler returns the HTTP handler with the auth + Host guards applied.
func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/requests", d.handleRequests)
	mux.HandleFunc("/", d.handleIndex)
	return d.guard(mux)
}

// guard enforces a loopback Host header (DNS-rebinding defense) and the auth
// token on every request before delegating to next.
func (d *Dashboard) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if i := strings.IndexByte(host, ':'); i >= 0 {
			host = host[:i] // strip :port
		}
		if !allowedHosts[host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Header.Get("X-Stunt-Token") != d.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (d *Dashboard) handleRequests(w http.ResponseWriter, r *http.Request) {
	q := requestlog.Query{
		Service: r.URL.Query().Get("service"),
		Method:  r.URL.Query().Get("method"),
		Path:    r.URL.Query().Get("path"),
		Q:       r.URL.Query().Get("q"),
		Limit:   atoiDefault(r.URL.Query().Get("limit"), 100),
	}
	entries, err := d.store.List(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

func (d *Dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	entries, _ := d.store.List(requestlog.Query{Limit: 200})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.tmpl.Execute(w, entries)
}

// newToken returns a random 32-char hex token.
func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// atoiDefault parses s as a positive int, returning def when s is empty,
// non-numeric, or non-positive.
func atoiDefault(s string, def int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return def
	}
	return n
}
