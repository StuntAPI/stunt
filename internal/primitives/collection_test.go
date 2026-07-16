package primitives

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCollectionInsertGet(t *testing.T) {
	s := newTestStore(t)
	c, err := s.Collection("users")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	id, err := c.Insert(map[string]any{"name": "Alice", "age": 30})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id == "" {
		t.Fatal("Insert returned empty id")
	}

	got, err := c.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got["name"] != "Alice" {
		t.Fatalf("name = %v, want Alice", got["name"])
	}
	if got["age"] != float64(30) {
		t.Fatalf("age = %v (%T), want float64(30)", got["age"], got["age"])
	}
	if got["id"] != id {
		t.Fatalf("id in doc = %v, want %s", got["id"], id)
	}
}

func TestCollectionInsertWithProvidedID(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.Collection("items")

	id, err := c.Insert(map[string]any{"id": "custom-123", "label": "widget"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id != "custom-123" {
		t.Fatalf("id = %q, want custom-123", id)
	}
}

func TestCollectionList(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.Collection("products")

	for _, name := range []string{"p1", "p2", "p3"} {
		c.Insert(map[string]any{"name": name})
	}

	docs, err := c.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("len(docs) = %d, want 3", len(docs))
	}
}

func TestCollectionUpdate(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.Collection("users")

	id, _ := c.Insert(map[string]any{"name": "Bob", "age": 25, "city": "NYC"})

	// Update replaces the entire document (except id is preserved).
	err := c.Update(id, map[string]any{"name": "Bobby", "age": 26})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, _ := c.Get(id)
	if got["name"] != "Bobby" {
		t.Fatalf("name = %v, want Bobby", got["name"])
	}
	if got["age"] != float64(26) {
		t.Fatalf("age = %v, want 26", got["age"])
	}
	if _, ok := got["city"]; ok {
		t.Fatal("city should be gone after replace update")
	}
	if got["id"] != id {
		t.Fatalf("id = %v, want %s", got["id"], id)
	}
}

func TestCollectionDelete(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.Collection("users")

	id, _ := c.Insert(map[string]any{"name": "Carol"})

	if err := c.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := c.Get(id)
	if err != sql.ErrNoRows {
		t.Fatalf("Get after delete: err = %v, want sql.ErrNoRows", err)
	}

	docs, _ := c.List()
	if len(docs) != 0 {
		t.Fatalf("len(docs) = %d, want 0 after delete", len(docs))
	}
}

func TestCollectionGetNotFound(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.Collection("things")

	_, err := c.Get("nonexistent")
	if err != sql.ErrNoRows {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestCollectionDeleteNotFound(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.Collection("things")

	if err := c.Delete("nonexistent"); err != nil {
		t.Fatalf("Delete nonexistent should be nil, got %v", err)
	}
}

func TestCollectionSeed(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.Collection("accounts")

	dir := t.TempDir()
	path := filepath.Join(dir, "seed.jsonl")
	data := `{"name":"Alice","balance":100}
{"name":"Bob","balance":200}
{"name":"Carol","balance":300}
`
	if err := writeFile(path, data); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	if err := c.Seed(path); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	docs, _ := c.List()
	if len(docs) != 3 {
		t.Fatalf("len(docs) = %d, want 3", len(docs))
	}

	// Verify each account was inserted with data + generated id.
	for _, d := range docs {
		if d["id"] == nil || d["id"] == "" {
			t.Fatal("seeded doc missing id")
		}
		if d["name"] == nil {
			t.Fatal("seeded doc missing name")
		}
	}
}

func TestCollectionPersistsAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")

	s1, _ := Open(dbPath)
	c1, _ := s1.Collection("widgets")
	c1.Insert(map[string]any{"name": "w1"})
	s1.Close()

	s2, _ := Open(dbPath)
	c2, _ := s2.Collection("widgets")
	docs, _ := c2.List()
	if len(docs) != 1 {
		t.Fatalf("len(docs) = %d, want 1 (persisted)", len(docs))
	}
	s2.Close()
}

func TestCollectionReuseSameCollection(t *testing.T) {
	s := newTestStore(t)

	c1, _ := s.Collection("orders")
	c2, _ := s.Collection("orders")

	c1.Insert(map[string]any{"total": 42})
	docs, err := c2.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("len(docs) = %d, want 1", len(docs))
	}
}
