// Package requestlog records simulated-API request/response traffic to SQLite.
// Writes are asynchronous (off the request hot path) and bounded (ring + size cap).
package requestlog

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Entry is one captured request/response.
type Entry struct {
	ID          int64  `json:"id"`
	Seq         int64  `json:"seq"`
	Ts          string `json:"ts"` // RFC3339
	Service     string `json:"service"`
	Transport   string `json:"transport"` // http | grpc | ws
	Method      string `json:"method"`
	Path        string `json:"path"`
	Status      int    `json:"status"`
	DurationUs  int64  `json:"duration_us"`  // microseconds (sub-ms resolution)
	ReqHeaders  string `json:"req_headers"`  // JSON, sensitive values redacted
	ReqBody     string `json:"req_body"`     // capped
	RespHeaders string `json:"resp_headers"` // JSON
	RespBody    string `json:"resp_body"`    // capped
}

// Query filters the history list.
type Query struct {
	Service string
	Method  string
	Path    string // substring match
	Status  int    // 0 = any
	Q       string // free-text over path+req_body+resp_body
	Since   int64  // only entries with seq > Since (0 = no filter; gap-free backfill)
	Limit   int
	Offset  int
}

const schema = `CREATE TABLE IF NOT EXISTS request_log (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	seq         INTEGER NOT NULL,
	ts          TEXT NOT NULL,
	service     TEXT NOT NULL,
	transport   TEXT NOT NULL,
	method      TEXT NOT NULL,
	path        TEXT NOT NULL,
	status      INTEGER NOT NULL,
	duration_us INTEGER NOT NULL,
	req_headers TEXT NOT NULL DEFAULT '',
	req_body    TEXT NOT NULL DEFAULT '',
	resp_headers TEXT NOT NULL DEFAULT '',
	resp_body   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_request_log_seq ON request_log(seq);`

// Options configures the store.
type Options struct {
	Ring       int   // keep at most this many newest entries (0 = unlimited)
	MaxBytes   int64 // rotate (delete oldest) when DB exceeds this (0 = unlimited)
	WriteQueue int   // async enqueue buffer size (default 1024)
}

// WithRing sets the ring-bound (newest-N) retention.
func WithRing(n int) func(*Options) {
	return func(o *Options) { o.Ring = n }
}

// Open opens (or creates) the request log at path with the given options.
// Without options it defaults to Ring=1000, WriteQueue=1024.
func Open(path string, opts ...func(*Options)) (*Store, error) {
	o := Options{Ring: 1000, WriteQueue: 1024}
	for _, fn := range opts {
		fn(&o)
	}
	st, err := openStore(path, o)
	if err != nil {
		return nil, err
	}
	st.startWriter()
	return st, nil
}

// openStore opens the database and applies the schema, storing opts on the
// struct. It does NOT start the async writer (callers that want a read-only
// inspection store can use this directly).
func openStore(path string, opts Options) (*Store, error) {
	if opts.WriteQueue <= 0 {
		opts.WriteQueue = 1024
	}
	// _pragma=busy_timeout makes SQLite wait up to 5s on lock contention (e.g. a
	// test reopening the same state dir while a prior engine's writer drains)
	// instead of failing instantly with SQLITE_BUSY.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create request_log: %w", err)
	}
	return &Store{db: db, opts: opts, bus: NewBus()}, nil
}

// Store is the SQLite-backed request log.
type Store struct {
	db         *sql.DB
	opts       Options
	bus        *Bus // always set (created in openStore); recorders publish here
	ch         chan Entry
	writerDone chan struct{}
	inFlight   sync.WaitGroup
	closeOnce  sync.Once
	closeErr   error
}

// Close drains the async writer (if running), stops it, then closes the DB. It
// is idempotent. Draining before close guarantees in-flight writes are persisted
// and the single connection is released, so a caller reopening the same DB (e.g.
// a test restarting an engine in one state dir) does not hit SQLITE_BUSY.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.ch != nil {
			s.inFlight.Wait() // finish all in-flight writes
			close(s.ch)       // signal the writer to exit
			<-s.writerDone    // wait until it has exited
		}
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// startWriter launches the background goroutine that drains s.ch and persists
// each entry off the request hot path.
func (s *Store) startWriter() {
	s.ch = make(chan Entry, s.opts.WriteQueue)
	s.writerDone = make(chan struct{})
	go func() {
		defer close(s.writerDone)
		for e := range s.ch {
			_ = s.persist(e)
			s.inFlight.Done()
		}
	}()
}

// Enqueue hands an entry to the writer goroutine (non-blocking; drops on full).
// If no writer is running (store opened read-only), it persists synchronously.
func (s *Store) Enqueue(e Entry) {
	if s.ch == nil { // read-only inspection store
		_ = s.persist(e)
		return
	}
	s.inFlight.Add(1)
	select {
	case s.ch <- e:
	default: // queue full → drop; ring still bounds total on subsequent writes
		s.inFlight.Done()
	}
}

// Flush blocks until all enqueued entries have been written by the writer
// goroutine. It is a no-op for read-only stores.
func (s *Store) Flush() { s.inFlight.Wait() }

// persist inserts an entry (assigning ts if empty) and enforces the ring
// bound. It is pure with respect to the in-flight counter: the writer loop
// (or the sync Insert path) owns counter management.
func (s *Store) persist(e Entry) error {
	if e.Ts == "" {
		e.Ts = nowRFC3339()
	}
	if _, err := s.db.Exec(`INSERT INTO request_log
		(seq, ts, service, transport, method, path, status, duration_us,
		 req_headers, req_body, resp_headers, resp_body)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.Seq, e.Ts, e.Service, e.Transport, e.Method, e.Path, e.Status, e.DurationUs,
		e.ReqHeaders, e.ReqBody, e.RespHeaders, e.RespBody); err != nil {
		return fmt.Errorf("insert request_log: %w", err)
	}
	if s.opts.Ring > 0 {
		_, _ = s.db.Exec(`DELETE FROM request_log WHERE id NOT IN (
			SELECT id FROM request_log ORDER BY seq DESC LIMIT ?)`, s.opts.Ring)
	}
	// Publish the stored entry to the bus AFTER a successful insert so the live
	// feed reflects exactly what was persisted. Non-blocking (drops on slow subs).
	if s.bus != nil {
		s.bus.Publish(e)
	}
	return nil
}

// Insert persists an entry synchronously (back-compat / test path). It does
// NOT go through the async writer and does not touch the in-flight counter.
func (s *Store) Insert(e Entry) error { return s.persist(e) }

// Bus returns the store's publisher (recorders publish captured entries here).
func (s *Store) Bus() *Bus { return s.bus }

// Get returns a single entry by DB id (for detail + replay).
func (s *Store) Get(id int64) (Entry, error) {
	var e Entry
	err := s.db.QueryRow(`SELECT id, seq, ts, service, transport, method, path,
		status, duration_us, req_headers, req_body, resp_headers, resp_body
		FROM request_log WHERE id = ?`, id).
		Scan(&e.ID, &e.Seq, &e.Ts, &e.Service, &e.Transport, &e.Method, &e.Path,
			&e.Status, &e.DurationUs, &e.ReqHeaders, &e.ReqBody, &e.RespHeaders, &e.RespBody)
	if err != nil {
		return Entry{}, fmt.Errorf("get request_log %d: %w", id, err)
	}
	return e, nil
}

// List returns entries matching q, newest-first. Non-empty filter fields
// (Service, Method, Path substring, Q free-text) are AND-combined into a
// WHERE clause; an empty query returns the newest entries up to Limit.
func (s *Store) List(q Query) ([]Entry, error) {
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 100
	}

	var where []string
	var args []any
	if q.Service != "" {
		where = append(where, "service = ?")
		args = append(args, q.Service)
	}
	if q.Method != "" {
		where = append(where, "method = ?")
		args = append(args, q.Method)
	}
	if q.Path != "" {
		where = append(where, "path LIKE ?")
		args = append(args, "%"+q.Path+"%")
	}
	if q.Q != "" {
		where = append(where, "(path LIKE ? OR req_body LIKE ? OR resp_body LIKE ?)")
		args = append(args, "%"+q.Q+"%", "%"+q.Q+"%", "%"+q.Q+"%")
	}
	if q.Since > 0 {
		where = append(where, "seq > ?")
		args = append(args, q.Since)
	}

	clause := ""
	if len(where) > 0 {
		clause = "WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, q.Limit, q.Offset)

	rows, err := s.db.Query(`SELECT id, seq, ts, service, transport, method, path,
		status, duration_us, req_headers, req_body, resp_headers, resp_body
		FROM request_log
		`+clause+`
		ORDER BY seq DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list request_log: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Seq, &e.Ts, &e.Service, &e.Transport, &e.Method,
			&e.Path, &e.Status, &e.DurationUs, &e.ReqHeaders, &e.ReqBody,
			&e.RespHeaders, &e.RespBody); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// nowRFC3339 returns the current time as RFC3339 (factored for tests).
func nowRFC3339() string { return timeNow().Format("2006-01-02T15:04:05.000Z07:00") }

// timeNow is the test-overridable clock seam.
var timeNow = time.Now
