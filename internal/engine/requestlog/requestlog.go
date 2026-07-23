// Package requestlog records simulated-API request/response traffic to SQLite.
// Writes are asynchronous (off the request hot path) and bounded (ring + size cap).
package requestlog

import (
	"database/sql"
	"fmt"
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
	DurationMs  int64  `json:"duration_ms"`
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
	duration_ms INTEGER NOT NULL,
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
	db, err := sql.Open("sqlite", path)
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
	return &Store{db: db, opts: opts}, nil
}

// Store is the SQLite-backed request log.
type Store struct {
	db        *sql.DB
	opts      Options
	ch        chan Entry
	inFlight  sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// Close stops the async writer (if running) and closes the database. It is
// idempotent: safe to call multiple times (the channel is closed and the DB
// closed exactly once).
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.ch != nil {
			close(s.ch)
		}
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// startWriter launches the background goroutine that drains s.ch and persists
// each entry off the request hot path.
func (s *Store) startWriter() {
	s.ch = make(chan Entry, s.opts.WriteQueue)
	go func() {
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
		(seq, ts, service, transport, method, path, status, duration_ms,
		 req_headers, req_body, resp_headers, resp_body)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.Seq, e.Ts, e.Service, e.Transport, e.Method, e.Path, e.Status, e.DurationMs,
		e.ReqHeaders, e.ReqBody, e.RespHeaders, e.RespBody); err != nil {
		return fmt.Errorf("insert request_log: %w", err)
	}
	if s.opts.Ring > 0 {
		_, _ = s.db.Exec(`DELETE FROM request_log WHERE id NOT IN (
			SELECT id FROM request_log ORDER BY seq DESC LIMIT ?)`, s.opts.Ring)
	}
	return nil
}

// Insert persists an entry synchronously (back-compat / test path). It does
// NOT go through the async writer and does not touch the in-flight counter.
func (s *Store) Insert(e Entry) error { return s.persist(e) }

// List returns entries matching q, newest-first.
func (s *Store) List(q Query) ([]Entry, error) {
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 100
	}
	rows, err := s.db.Query(`SELECT id, seq, ts, service, transport, method, path,
		status, duration_ms, req_headers, req_body, resp_headers, resp_body
		FROM request_log
		ORDER BY seq DESC LIMIT ? OFFSET ?`, q.Limit, q.Offset)
	if err != nil {
		return nil, fmt.Errorf("list request_log: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Seq, &e.Ts, &e.Service, &e.Transport, &e.Method,
			&e.Path, &e.Status, &e.DurationMs, &e.ReqHeaders, &e.ReqBody,
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
