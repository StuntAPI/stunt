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
