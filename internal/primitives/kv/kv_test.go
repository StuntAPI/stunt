package kv

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func newTestKV(t *testing.T) *KV {
	t.Helper()
	dir := t.TempDir()
	k, err := Open(filepath.Join(dir, "kvtest.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { k.Close() })
	return k
}

func TestKVSetGet(t *testing.T) {
	k := newTestKV(t)

	if err := k.Set("config", "port", "8080"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := k.Get("config", "port")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "8080" {
		t.Fatalf("Get = %q, want 8080", got)
	}
}

func TestKVSetOverwrite(t *testing.T) {
	k := newTestKV(t)

	k.Set("config", "port", "8080")
	k.Set("config", "port", "9090")

	got, _ := k.Get("config", "port")
	if got != "9090" {
		t.Fatalf("Get = %q, want 9090 after overwrite", got)
	}
}

func TestKVNamespacing(t *testing.T) {
	k := newTestKV(t)

	k.Set("ns1", "key", "value1")
	k.Set("ns2", "key", "value2")

	got1, _ := k.Get("ns1", "key")
	got2, _ := k.Get("ns2", "key")

	if got1 != "value1" {
		t.Fatalf("ns1/key = %q, want value1", got1)
	}
	if got2 != "value2" {
		t.Fatalf("ns2/key = %q, want value2", got2)
	}
}

func TestKVDelete(t *testing.T) {
	k := newTestKV(t)

	k.Set("config", "host", "localhost")
	if err := k.Delete("config", "host"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := k.Get("config", "host")
	if err != sql.ErrNoRows {
		t.Fatalf("Get after delete: err = %v, want sql.ErrNoRows", err)
	}
}

func TestKVGetNotFound(t *testing.T) {
	k := newTestKV(t)

	_, err := k.Get("missing", "key")
	if err != sql.ErrNoRows {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestKVDeleteNotFound(t *testing.T) {
	k := newTestKV(t)

	if err := k.Delete("nope", "nope"); err != nil {
		t.Fatalf("Delete nonexistent should be nil, got %v", err)
	}
}

func TestKVPersistsAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")

	k1, _ := Open(dbPath)
	k1.Set("state", "counter", "42")
	k1.Close()

	k2, _ := Open(dbPath)
	got, err := k2.Get("state", "counter")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "42" {
		t.Fatalf("Get = %q, want 42 (persisted)", got)
	}
	k2.Close()
}
