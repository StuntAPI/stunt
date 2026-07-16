package primitives

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

// Open opens or creates a SQLite database at path and ensures it is usable.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// SQLite performs better with a single connection; also avoids lock contention.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Store wraps a *sql.DB for collection-based document storage.
type Store struct {
	db *sql.DB
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// Collection returns a Collection backed by table <name>, creating it if needed.
func (s *Store) Collection(name string) (*Collection, error) {
	_, err := s.db.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (id TEXT PRIMARY KEY, data TEXT)`,
		quoteIdent(name),
	))
	if err != nil {
		return nil, fmt.Errorf("create collection %s: %w", name, err)
	}
	return &Collection{store: s, name: quoteIdent(name)}, nil
}

// Collection is a simple document store backed by a SQLite table.
// Each document is a JSON object stored as TEXT in a single "data" column.
type Collection struct {
	store *Store
	name  string // quoted table identifier
}

// Insert adds doc to the collection. If doc has an "id" key it is used;
// otherwise a random hex id is generated. The id is stamped into the stored
// document and returned.
func (c *Collection) Insert(doc map[string]any) (string, error) {
	id, _ := doc["id"].(string)
	if id == "" {
		id = randomID()
	}
	doc["id"] = id

	data, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal doc: %w", err)
	}

	_, err = c.store.db.Exec(
		fmt.Sprintf(`INSERT INTO %s (id, data) VALUES (?, ?)`, c.name),
		id, string(data),
	)
	if err != nil {
		return "", fmt.Errorf("insert into %s: %w", c.name, err)
	}
	return id, nil
}

// Get retrieves a single document by id.
// Returns sql.ErrNoRows when the document does not exist.
func (c *Collection) Get(id string) (map[string]any, error) {
	var data string
	err := c.store.db.QueryRow(
		fmt.Sprintf(`SELECT data FROM %s WHERE id = ?`, c.name),
		id,
	).Scan(&data)
	if err != nil {
		return nil, err
	}
	return unmarshalDoc(data)
}

// List returns all documents in the collection.
func (c *Collection) List() ([]map[string]any, error) {
	rows, err := c.store.db.Query(
		fmt.Sprintf(`SELECT data FROM %s`, c.name),
	)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", c.name, err)
	}
	defer rows.Close()

	var docs []map[string]any
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		doc, err := unmarshalDoc(data)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

// Update replaces the document at id with doc. The id is preserved.
func (c *Collection) Update(id string, doc map[string]any) error {
	doc["id"] = id
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal doc: %w", err)
	}

	res, err := c.store.db.Exec(
		fmt.Sprintf(`UPDATE %s SET data = ? WHERE id = ?`, c.name),
		string(data), id,
	)
	if err != nil {
		return fmt.Errorf("update %s: %w", c.name, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Delete removes the document at id. No-op (nil error) if it doesn't exist.
func (c *Collection) Delete(id string) error {
	_, err := c.store.db.Exec(
		fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, c.name),
		id,
	)
	if err != nil {
		return fmt.Errorf("delete from %s: %w", c.name, err)
	}
	return nil
}

// Seed reads a JSONL file (one JSON object per line) and inserts each object.
func (c *Collection) Seed(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open seed file %s: %w", path, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	for dec.More() {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			return fmt.Errorf("decode jsonl line: %w", err)
		}
		if _, err := c.Insert(doc); err != nil {
			return err
		}
	}
	return nil
}

// --- helpers ---

func unmarshalDoc(data string) (map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		return nil, fmt.Errorf("unmarshal doc: %w", err)
	}
	return doc, nil
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// quoteIdent wraps a SQL identifier in double quotes for safe interpolation.
func quoteIdent(name string) string {
	return `"` + name + `"`
}
