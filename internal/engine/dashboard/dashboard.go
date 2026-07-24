// Package dashboard serves the localhost admin UI + JSON API for a running engine.
//
// It is bound to a requestlog.Store and exposes:
//   - GET /api/requests — filterable JSON history
//   - GET /            — server-rendered history page (stunt "Rehearsal Grid" aesthetic)
//
// Every request must pass two guards: a DNS-rebinding Host-header check
// (loopback hosts only) and an auth-token check (X-Stunt-Token header).
package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

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
	store  *requestlog.Store
	token  string
	tmpl   *template.Template
	replay ReplayFunc    // engine-backed re-issuer; nil = replay unavailable
	seq    *atomic.Int64 // shared engine sequence counter for replay entries

	// state browsing + reset (engine-backed; nil = unavailable). Wired by up.go.
	stateOverview StateProvider
	colDocs       CollectionDocsProvider
	kvList        KVListProvider
	blobList      BlobListProvider
	reset         ResetSvcFunc
	snapshot      SnapshotProvider // nil = unavailable
	restore       RestoreProvider
	services      []string // manifest service names (set by up.go) for the picker
}

// ServiceState is a serializable snapshot of one service's stores, returned by
// StateProvider for the data browser.
type ServiceState struct {
	Collections []CollectionInfo `json:"collections"`
	KVNames     []string         `json:"kv_namespaces"`
	BlobNames   []string         `json:"blob_namespaces"`
}

// CollectionInfo is a collection name + its document count.
type CollectionInfo struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// StateProvider returns a serializable snapshot of a service's stores, or
// ok=false if the service has no adapter/state. Wired by up.go to the engine.
type StateProvider func(service string) (ServiceState, bool)

// CollectionDocsProvider returns the documents in a service's named collection.
type CollectionDocsProvider func(service, collection string) ([]map[string]any, error)

// KVListProvider returns the key/value pairs in a service's kv namespace.
type KVListProvider func(service, ns string) ([][2]string, error)

// BlobListProvider returns the blobs in a service's blob namespace.
type BlobListProvider func(service, ns string) ([]BlobInfo, error)

// BlobInfo is a serializable blob metadata entry.
type BlobInfo struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type,omitempty"`
	Modified    string `json:"modified"`
}

// ResetSvcFunc wipes one service's state (if service != "") or all services +
// the request log (if service == ""). Wired by up.go to the engine.
type ResetSvcFunc func(service string) error

// SnapshotProvider writes a full snapshot archive (gzip-tar) to w. Wired by
// up.go to engine.Snapshot.
type SnapshotProvider func(w io.Writer) error

// RestoreProvider reads a snapshot archive from r and restores state. Wired
// by up.go to engine.Restore.
type RestoreProvider func(r io.Reader) error

// SetSnapshot wires the engine-backed snapshot/restore providers (up.go).
func (d *Dashboard) SetSnapshot(s SnapshotProvider, r RestoreProvider) {
	d.snapshot, d.restore = s, r
}

// SetServices records the manifest's service names (for the browser picker).
func (d *Dashboard) SetServices(names []string) { d.services = names }

// SetState wires the engine-backed state providers (called by up.go).
func (d *Dashboard) SetState(
	overview StateProvider,
	docs CollectionDocsProvider,
	kv KVListProvider,
	blobs BlobListProvider,
	reset ResetSvcFunc,
) {
	d.stateOverview, d.colDocs, d.kvList, d.blobList, d.reset = overview, docs, kv, blobs, reset
}

// New builds a dashboard over store and generates a fresh auth token.
func New(store *requestlog.Store) *Dashboard {
	tmpl := template.Must(template.New("").Funcs(tmplFuncs).ParseFS(assets.Templates, "dashboard.tmpl"))
	return &Dashboard{store: store, token: newToken(), tmpl: tmpl}
}

// Token returns the session auth token (printed to the terminal by the CLI).
func (d *Dashboard) Token() string { return d.token }

// SetTokenForTest sets the token (test seam).
func (d *Dashboard) SetTokenForTest(t string) { d.token = t }

// ReplayFunc re-issues a captured entry against the simulator, returning the new
// status + response body. Wired by up.go to the engine. Nil = replay unavailable.
type ReplayFunc func(e requestlog.Entry) (status int, respBody string)

// SetReplayFunc wires the engine-backed replay (called by up.go).
func (d *Dashboard) SetReplayFunc(f ReplayFunc) { d.replay = f }

// SetSeq shares the engine's global sequence counter so replay entries get a
// proper, globally-unique Seq (replay bypasses the recorder).
func (d *Dashboard) SetSeq(seq *atomic.Int64) { d.seq = seq }

// Handler returns the HTTP handler with the auth + Host guards applied.
func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/requests/stream", d.handleStream)
	mux.HandleFunc("/api/requests", d.handleRequests)
	mux.HandleFunc("/api/requests/{id}", d.handleDetail)
	mux.HandleFunc("/api/requests/{id}/replay", d.handleReplay)
	// data browsers + reset (Plan 3)
	mux.HandleFunc("/api/state", d.handleStateServices)
	mux.HandleFunc("/api/state/reset", d.handleStateReset) // all-services reset (service=="")
	mux.HandleFunc("/api/state/snapshot", d.handleStateSnapshot)
	mux.HandleFunc("/api/state/restore", d.handleStateRestore)
	mux.HandleFunc("/api/state/{service}/reset", d.handleStateReset)
	mux.HandleFunc("/api/state/{service}/collections/{name}", d.handleStateCollection)
	mux.HandleFunc("/api/state/{service}/collections", d.handleStateCollections)
	mux.HandleFunc("/api/state/{service}/kv/{ns}", d.handleStateKV)
	mux.HandleFunc("/api/state/{service}/kv", d.handleStateKVList)
	mux.HandleFunc("/api/state/{service}/blobs", d.handleStateBlobs)
	mux.HandleFunc("/api/state/{service}", d.handleStateService)
	mux.HandleFunc("/", d.handleIndex)
	return d.guard(mux)
}

// cookieName is the bootstrap cookie that carries the auth token so a
// browser (which cannot send a custom header on a plain navigation) can talk
// to the dashboard after the initial `stunt ui` open. Its value IS the token;
// that's acceptable for a localhost-only developer tool. Set HttpOnly +
// SameSite=Strict + Path=/; no Secure because the dashboard serves plain
// http on loopback (a Secure cookie would be dropped over http).
const cookieName = "stunt_token"

// guard enforces a loopback Host header (DNS-rebinding defense) and the auth
// token on every request before delegating to next.
//
// A request is authorized if ANY of:
//   - the X-Stunt-Token header == token (CLI clients like `stunt requests`), OR
//   - a stunt_token cookie == token (browsers after bootstrap).
//
// To bootstrap the cookie, a request carrying ?token=<token> that matches AND
// has no valid cookie gets a stunt_token cookie set and a 302 redirect to the
// same path with the token query stripped. Subsequent browser requests (page
// nav + the ws client) then carry the cookie automatically. The token is never
// echoed in a response body.
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

		// Header path (CLI clients).
		if r.Header.Get("X-Stunt-Token") == d.token {
			next.ServeHTTP(w, r)
			return
		}
		// Cookie path (browsers after bootstrap).
		if c, err := r.Cookie(cookieName); err == nil && c.Value == d.token {
			next.ServeHTTP(w, r)
			return
		}

		// Bootstrap: a ?token=<token> query that matches AND no valid cookie
		// → set the cookie and redirect to the same path minus the query so the
		// token doesn't linger in the address bar.
		if r.URL.Query().Get("token") == d.token {
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    d.token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			loc := stripTokenQuery(r.URL)
			http.Redirect(w, r, loc, http.StatusFound)
			return
		}

		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// stripTokenQuery returns the string form of u with the token query parameter
// removed (all other query params preserved). It never echoes the token.
func stripTokenQuery(u *url.URL) string {
	q := u.Query()
	q.Del("token")
	u.RawQuery = q.Encode()
	return u.String()
}

// apiEntry is the JSON shape exposed by /api/requests. Unlike requestlog.Entry
// (which stores headers as JSON TEXT for SQLite), apiEntry decodes the header
// blobs into real objects so consumers don't have to double-decode.
type apiEntry struct {
	ID          int64               `json:"id"`
	Seq         int64               `json:"seq"`
	Ts          string              `json:"ts"`
	Service     string              `json:"service"`
	Transport   string              `json:"transport"`
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Status      int                 `json:"status"`
	DurationUs  int64               `json:"duration_us"`
	ReqHeaders  map[string][]string `json:"req_headers"`
	ReqBody     string              `json:"req_body"`
	RespHeaders map[string][]string `json:"resp_headers"`
	RespBody    string              `json:"resp_body"`
}

// toAPI converts a stored entry to its API shape (headers decoded to objects).
func toAPI(e requestlog.Entry) apiEntry {
	return apiEntry{
		ID:          e.ID,
		Seq:         e.Seq,
		Ts:          e.Ts,
		Service:     e.Service,
		Transport:   e.Transport,
		Method:      e.Method,
		Path:        e.Path,
		Status:      e.Status,
		DurationUs:  e.DurationUs,
		ReqHeaders:  decodeHeaders(e.ReqHeaders),
		ReqBody:     e.ReqBody,
		RespHeaders: decodeHeaders(e.RespHeaders),
		RespBody:    e.RespBody,
	}
}

// decodeHeaders parses a stored JSON header blob into a map. A blank or
// unparseable blob yields nil (renders as JSON null, not a broken string).
func decodeHeaders(s string) map[string][]string {
	if s == "" {
		return nil
	}
	var h map[string][]string
	if err := json.Unmarshal([]byte(s), &h); err != nil {
		return nil
	}
	return h
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
	out := make([]apiEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, toAPI(e))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (d *Dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	entries, _ := d.store.List(requestlog.Query{Limit: 200})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.tmpl.ExecuteTemplate(w, "dashboard.tmpl", entries)
}

// handleDetail returns the full stored entry (objects decoded) by DB id.
func (d *Dashboard) handleDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	e, err := d.store.Get(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toAPI(e))
}

// handleReplay re-issues a captured entry via the wired ReplayFunc and logs a
// fresh entry (tagged onto the original's service/method/path). POST-only.
func (d *Dashboard) handleReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	e, err := d.store.Get(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if d.replay == nil {
		http.Error(w, "replay not available", http.StatusServiceUnavailable)
		return
	}
	status, body := d.replay(e)

	// Log a fresh entry: replay bypasses the recorder, so pull the next
	// globally-unique seq from the shared engine counter (if wired).
	var seq int64
	if d.seq != nil {
		seq = d.seq.Add(1)
	}
	d.store.Enqueue(requestlog.Entry{
		Seq: seq, Service: e.Service, Transport: "http", Method: e.Method, Path: e.Path,
		Status: status, ReqHeaders: e.ReqHeaders, ReqBody: e.ReqBody, RespBody: body,
	})
	d.store.Flush()

	w.Header().Set("Content-Type", "application/json")
	// If the replayed body is itself JSON, embed it raw so clients get a
	// structured value rather than a doubly-escaped string.
	if raw := json.RawMessage(body); json.Valid(raw) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "body": raw})
	} else {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "body": body})
	}
}

// handleStream upgrades to a WebSocket and streams captured requests live,
// gap-free: any entries newer than the client's last seen seq (since_seq) are
// backfilled oldest-first, then live entries fan out as they are persisted.
// The endpoint lives under the same guard as the REST API (token + Host), so
// Accept is reached only for authenticated loopback clients.
func (d *Dashboard) handleStream(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close(websocket.StatusInternalError, "closing")

	ctx := r.Context()
	since, _ := strconv.ParseInt(r.URL.Query().Get("since_seq"), 10, 64)

	// Subscribe BEFORE backfilling so nothing published in the gap is lost.
	ch, cancel := d.store.Bus().Subscribe()
	defer cancel()

	// Gap-free backfill: send entries with seq > since, oldest-first. List
	// returns newest-first, so iterate in reverse. On first connect (since<=0)
	// the client fetches its own initial fill via /api/requests; here we only
	// stream what arrives live.
	if since > 0 {
		recent, _ := d.store.List(requestlog.Query{Since: since, Limit: 1000})
		for i := len(recent) - 1; i >= 0; i-- {
			if err := wsWriteJSON(ctx, c, toAPI(recent[i])); err != nil {
				return
			}
		}
	}

	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return
			}
			if e.Seq <= since {
				continue // already backfilled; skip
			}
			if err := wsWriteJSON(ctx, c, toAPI(e)); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// wsWriteJSON marshals v and writes it as a single WebSocket text frame. A
// write error (client gone / context cancelled) is returned to the caller,
// which closes the connection via the deferred Close.
func wsWriteJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

// newToken returns a random 32-char hex token.
// ── data browsers + reset (Plan 3) ──

// stateUnavailable reports 503 when no engine-backed state provider is wired.
func (d *Dashboard) stateUnavailable(w http.ResponseWriter) bool {
	if d.stateOverview == nil {
		http.Error(w, "state browsing not available", http.StatusServiceUnavailable)
		return true
	}
	return false
}

// handleStateServices lists the manifest's service names (the picker).
func (d *Dashboard) handleStateServices(w http.ResponseWriter, r *http.Request) {
	if d.stateUnavailable(w) {
		return
	}
	writeJSON(w, map[string]any{"services": d.services})
}

// handleStateService returns the overview (collections + counts, kv/blob ns).
func (d *Dashboard) handleStateService(w http.ResponseWriter, r *http.Request) {
	if d.stateUnavailable(w) {
		return
	}
	svc := r.PathValue("service")
	st, ok := d.stateOverview(svc)
	if !ok {
		http.Error(w, "no state for service "+svc, http.StatusNotFound)
		return
	}
	writeJSON(w, st)
}

// handleStateCollections lists a service's collections + counts.
func (d *Dashboard) handleStateCollections(w http.ResponseWriter, r *http.Request) {
	if d.stateUnavailable(w) {
		return
	}
	svc := r.PathValue("service")
	st, ok := d.stateOverview(svc)
	if !ok {
		http.Error(w, "no state for service "+svc, http.StatusNotFound)
		return
	}
	writeJSON(w, st.Collections)
}

// handleStateCollection returns the documents in one collection.
func (d *Dashboard) handleStateCollection(w http.ResponseWriter, r *http.Request) {
	if d.stateUnavailable(w) || d.colDocs == nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}
	docs, err := d.colDocs(r.PathValue("service"), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, docs)
}

// handleStateKVList lists a service's kv namespaces.
func (d *Dashboard) handleStateKVList(w http.ResponseWriter, r *http.Request) {
	if d.stateUnavailable(w) {
		return
	}
	st, ok := d.stateOverview(r.PathValue("service"))
	if !ok {
		http.Error(w, "no state", http.StatusNotFound)
		return
	}
	writeJSON(w, st.KVNames)
}

// handleStateKV returns the key/value pairs in one kv namespace.
func (d *Dashboard) handleStateKV(w http.ResponseWriter, r *http.Request) {
	if d.stateUnavailable(w) || d.kvList == nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}
	pairs, err := d.kvList(r.PathValue("service"), r.PathValue("ns"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, pairs)
}

// handleStateBlobs lists a service's blobs (across namespaces or ?ns=).
func (d *Dashboard) handleStateBlobs(w http.ResponseWriter, r *http.Request) {
	if d.stateUnavailable(w) || d.blobList == nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}
	ns := r.URL.Query().Get("ns")
	infos, err := d.blobList(r.PathValue("service"), ns)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, infos)
}

// handleStateReset wipes one service (POST) or all (POST /api/state/reset).
func (d *Dashboard) handleStateReset(w http.ResponseWriter, r *http.Request) {
	if d.reset == nil {
		http.Error(w, "reset not available", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	svc := r.PathValue("service") // "" when called at /api/state/reset
	if err := d.reset(svc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"reset": true, "service": svc})
}

// writeJSON encodes v as JSON.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleStateSnapshot streams a snapshot archive (gzip-tar) back as an
// attachment. GET or POST; both trigger a download of the current state.
func (d *Dashboard) handleStateSnapshot(w http.ResponseWriter, r *http.Request) {
	if d.snapshot == nil {
		http.Error(w, "snapshot not available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="stunt-snapshot.tar.gz"`)
	if err := d.snapshot(w); err != nil {
		// Headers already sent (200 + streaming); best we can do is log.
		fmt.Fprintf(w, "\n[snapshot error: %v]", err)
	}
}

// handleStateRestore reads a snapshot archive from the request body and
// restores it. POST only. Expects a raw gzip-tar body (not multipart).
func (d *Dashboard) handleStateRestore(w http.ResponseWriter, r *http.Request) {
	if d.restore == nil {
		http.Error(w, "restore not available", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := d.restore(r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"restored": true})
}

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

// tmplFuncs are template helpers.
var tmplFuncs = template.FuncMap{
	// humanize a microsecond duration: "<1ms", "1.2ms", "350µs", "42ms"
	"humanDur": func(us int64) string {
		d := time.Duration(us) * time.Microsecond
		switch {
		case d < 1*time.Microsecond:
			return "<1µs"
		case d < time.Millisecond:
			return fmt.Sprintf("%dµs", d.Microseconds())
		case d < time.Second:
			return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", float64(d.Microseconds())/1000.0), "0"), ".") + "ms"
		default:
			return fmt.Sprintf("%.2fs", d.Seconds())
		}
	},
	// statusClass returns a CSS class for a status code (ok / redirect / client-err / server-err).
	"statusClass": func(status int) string {
		switch {
		case status >= 200 && status < 300:
			return "st-ok"
		case status >= 300 && status < 400:
			return "st-redir"
		case status >= 400 && status < 500:
			return "st-4xx"
		default:
			return "st-5xx"
		}
	},
	// lower lowercases a string (for CSS class derivation in the template).
	"lower": strings.ToLower,
}
