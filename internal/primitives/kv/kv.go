package kv

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Open opens or creates a SQLite database at path with the KV schema.
func Open(path string) (*KV, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS kv (
		namespace TEXT NOT NULL,
		key       TEXT NOT NULL,
		value     TEXT,
		PRIMARY KEY (namespace, key)
	)`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create kv table: %w", err)
	}

	return &KV{db: db}, nil
}

// KV is a simple namespaced key-value store backed by SQLite.
type KV struct {
	db *sql.DB
}

// Close closes the underlying database connection.
func (k *KV) Close() error { return k.db.Close() }

// Get retrieves the value for ns/key. Returns sql.ErrNoRows if not found.
func (k *KV) Get(ns, key string) (string, error) {
	var value string
	err := k.db.QueryRow(
		`SELECT value FROM kv WHERE namespace = ? AND key = ?`,
		ns, key,
	).Scan(&value)
	if err != nil {
		return "", err
	}
	return value, nil
}

// Set stores value for ns/key, overwriting any existing value.
func (k *KV) Set(ns, key, value string) error {
	_, err := k.db.Exec(
		`INSERT INTO kv (namespace, key, value) VALUES (?, ?, ?)
		 ON CONFLICT(namespace, key) DO UPDATE SET value = excluded.value`,
		ns, key, value,
	)
	if err != nil {
		return fmt.Errorf("kv set %s/%s: %w", ns, key, err)
	}
	return nil
}

// Incr atomically increments the integer stored at ns/key (treating it as a
// base-10 int; a missing/non-numeric value is treated as 0) and returns the
// NEW value after incrementing. It is a single SQL statement (upsert +
// RETURNING), so it is safe under concurrent callers — use this instead of a
// Get-then-Set sequence for monotonic id/counters.
func (k *KV) Incr(ns, key string) (int, error) {
	var next int
	err := k.db.QueryRow(
		`INSERT INTO kv (namespace, key, value) VALUES (?, ?, '1')
		 ON CONFLICT(namespace, key) DO UPDATE SET value = CAST(COALESCE(CAST(value AS INTEGER), 0) + 1 AS TEXT)
		 RETURNING CAST(value AS INTEGER)`,
		ns, key,
	).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("kv incr %s/%s: %w", ns, key, err)
	}
	return next, nil
}

// Delete removes the entry for ns/key. No-op (nil error) if it doesn't exist.
func (k *KV) Delete(ns, key string) error {
	_, err := k.db.Exec(
		`DELETE FROM kv WHERE namespace = ? AND key = ?`,
		ns, key,
	)
	if err != nil {
		return fmt.Errorf("kv delete %s/%s: %w", ns, key, err)
	}
	return nil
}

// List returns all key/value pairs in namespace ns, ordered by key. Used by the
// dashboard's kv browser.
func (k *KV) List(ns string) ([][2]string, error) {
	rows, err := k.db.Query(`SELECT key, value FROM kv WHERE namespace = ? ORDER BY key`, ns)
	if err != nil {
		return nil, fmt.Errorf("kv list %s: %w", ns, err)
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out = append(out, [2]string{key, value})
	}
	return out, rows.Err()
}

// Namespaces returns the distinct namespaces that currently hold at least one
// key, ordered by name. Used by the dashboard's kv browser.
func (k *KV) Namespaces() ([]string, error) {
	rows, err := k.db.Query(`SELECT DISTINCT namespace FROM kv ORDER BY namespace`)
	if err != nil {
		return nil, fmt.Errorf("kv namespaces: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, err
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

// Clear removes every key in namespace ns (no-op if ns has none). Used by reset.
func (k *KV) Clear(ns string) error {
	if _, err := k.db.Exec(`DELETE FROM kv WHERE namespace = ?`, ns); err != nil {
		return fmt.Errorf("kv clear %s: %w", ns, err)
	}
	return nil
}

// ClearAll removes every key across all namespaces. Used by reset.
func (k *KV) ClearAll() error {
	if _, err := k.db.Exec(`DELETE FROM kv`); err != nil {
		return fmt.Errorf("kv clear all: %w", err)
	}
	return nil
}
