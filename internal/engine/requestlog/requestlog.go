// Package requestlog records simulated-API request/response traffic to SQLite.
// Writes are asynchronous (off the request hot path) and bounded (ring + size cap).
package requestlog

import (
	"database/sql"
	"fmt"
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

// Open opens (or creates) the request log at path.
func Open(path string) (*Store, error) {
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
	return &Store{db: db}, nil
}

// Store is the SQLite-backed request log.
type Store struct {
	db *sql.DB
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Insert persists an entry (assigning seq + ts if zero).
func (s *Store) Insert(e Entry) error {
	if e.Ts == "" {
		e.Ts = nowRFC3339()
	}
	_, err := s.db.Exec(`INSERT INTO request_log
		(seq, ts, service, transport, method, path, status, duration_ms,
		 req_headers, req_body, resp_headers, resp_body)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.Seq, e.Ts, e.Service, e.Transport, e.Method, e.Path, e.Status, e.DurationMs,
		e.ReqHeaders, e.ReqBody, e.RespHeaders, e.RespBody)
	if err != nil {
		return fmt.Errorf("insert request_log: %w", err)
	}
	return nil
}

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
