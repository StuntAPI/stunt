// Package catalog provides a registry of stunt adapters. The registry is an
// Index — a searchable list of Entry records, each describing one adapter
// (name, description, git URL, pinned ref, tags).
//
// A RemoteIndex fetches the registry from a configurable JSON URL and caches
// the result in-memory with a TTL. When the remote fetch fails (network
// error, invalid JSON, non-200), the index transparently falls back to a
// small bundled index compiled into the binary via go:embed. This guarantees
// the catalog always works offline.
package catalog

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed builtin.json
var builtinIndexData []byte

// DefaultIndexURL is the canonical stunt-project-hosted catalog index. It
// can be overridden at construction time (for tests) or via the STUNT_CATALOG_URL
// environment variable / --catalog-url flag in the CLI.
const DefaultIndexURL = "https://stunt-adapters.github.io/catalog/index.json"

// DefaultCacheTTL is how long a fetched index is considered fresh.
const DefaultCacheTTL = 10 * time.Minute

// Entry describes one adapter in the catalog.
type Entry struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	GitURL      string   `json:"git_url"`
	LatestRef   string   `json:"latest_ref"`
	Tags        []string `json:"tags"`
}

// Index is a searchable adapter registry.
type Index interface {
	// Search returns entries whose name, description, or tags contain the
	// query (case-insensitive). An empty query returns all entries.
	Search(ctx context.Context, query string) ([]Entry, error)
	// Get returns the entry with the given name (exact match).
	Get(ctx context.Context, name string) (Entry, error)
}

// RemoteIndex fetches the catalog from a JSON URL, caches it, and falls back
// to a bundled index on failure.
type RemoteIndex struct {
	url    string
	client *http.Client
	ttl    time.Duration

	mu       sync.Mutex
	cached   []Entry
	cachedAt time.Time
}

// NewRemoteIndex creates a RemoteIndex pointing at the default stunt catalog
// URL with standard settings.
func NewRemoteIndex() *RemoteIndex {
	return &RemoteIndex{
		url:    DefaultIndexURL,
		client: &http.Client{Timeout: 15 * time.Second},
		ttl:    DefaultCacheTTL,
	}
}

// NewRemoteIndexWithClient creates a RemoteIndex with an explicit URL, HTTP
// client, and cache TTL. Intended for testing.
func NewRemoteIndexWithClient(url string, client *http.Client, ttl time.Duration) *RemoteIndex {
	return &RemoteIndex{
		url:    url,
		client: client,
		ttl:    ttl,
	}
}

func (r *RemoteIndex) Search(ctx context.Context, query string) ([]Entry, error) {
	entries, err := r.entries(ctx)
	if err != nil {
		return nil, err
	}
	if query == "" {
		return entries, nil
	}
	q := strings.ToLower(query)
	var matched []Entry
	for _, e := range entries {
		if matches(e, q) {
			matched = append(matched, e)
		}
	}
	return matched, nil
}

func (r *RemoteIndex) Get(ctx context.Context, name string) (Entry, error) {
	entries, err := r.entries(ctx)
	if err != nil {
		return Entry{}, err
	}
	for _, e := range entries {
		if e.Name == name {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("catalog: adapter %q not found", name)
}

// entries returns the cached list if fresh, otherwise fetches from the
// remote URL. On fetch failure it falls back to the bundled index.
func (r *RemoteIndex) entries(ctx context.Context) ([]Entry, error) {
	r.mu.Lock()
	if r.cached != nil && time.Since(r.cachedAt) < r.ttl {
		entries := r.cached
		r.mu.Unlock()
		return entries, nil
	}
	r.mu.Unlock()

	fetched, err := r.fetch(ctx)
	if err != nil {
		// Fall back to the bundled index.
		return bundledEntries()
	}

	r.mu.Lock()
	r.cached = fetched
	r.cachedAt = time.Now()
	r.mu.Unlock()
	return fetched, nil
}

// fetch downloads and parses the JSON index from the configured URL.
func (r *RemoteIndex) fetch(ctx context.Context) ([]Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog: HTTP %d from %s", resp.StatusCode, r.url)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("catalog: parse index: %w", err)
	}
	return entries, nil
}

// bundledEntries parses the embedded builtin.json fallback index. This data
// is compiled into the binary so the catalog works even with no network.
var bundledErr error
var bundled []Entry

func init() {
	bundledErr = json.Unmarshal(builtinIndexData, &bundled)
}

func bundledEntries() ([]Entry, error) {
	if bundledErr != nil {
		return nil, fmt.Errorf("catalog: parse bundled index: %w", bundledErr)
	}
	return bundled, nil
}

// matches reports whether an entry matches a lowercased query in its name,
// description, or tags.
func matches(e Entry, q string) bool {
	if strings.Contains(strings.ToLower(e.Name), q) {
		return true
	}
	if strings.Contains(strings.ToLower(e.Description), q) {
		return true
	}
	for _, tag := range e.Tags {
		if strings.Contains(strings.ToLower(tag), q) {
			return true
		}
	}
	return false
}
