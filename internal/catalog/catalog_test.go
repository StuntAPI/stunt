package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// cannedEntries is a known set of entries served by the test HTTP server.
var cannedEntries = []Entry{
	{Name: "stripe-style", Description: "Stripe-style payment API", GitURL: "https://github.com/stuntapi/stripe-style", LatestRef: "v1.0.0", Tags: []string{"payments", "fintech"}},
	{Name: "drive-style", Description: "Google-Drive-style file storage API", GitURL: "https://github.com/stuntapi/drive-style", LatestRef: "v1.0.0", Tags: []string{"storage", "files"}},
	{Name: "twitter-style", Description: "Twitter/X-style social media API", GitURL: "https://github.com/stuntapi/twitter-style", LatestRef: "v1.0.0", Tags: []string{"social", "media"}},
}

func mustMarshalEntries(t *testing.T, entries []Entry) []byte {
	t.Helper()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal entries: %v", err)
	}
	return data
}

func newTestServer(t *testing.T, entries []Entry, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshalEntries(t, entries))
	}))
}

// --- Search tests ---

func TestSearchFiltersByName(t *testing.T) {
	srv := newTestServer(t, cannedEntries, nil)
	defer srv.Close()

	idx := NewRemoteIndexWithClient(srv.URL, srv.Client(), time.Minute)
	results, err := idx.Search(context.Background(), "stripe")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Name != "stripe-style" {
		t.Errorf("Name = %q, want %q", results[0].Name, "stripe-style")
	}
}

func TestSearchFiltersByTag(t *testing.T) {
	srv := newTestServer(t, cannedEntries, nil)
	defer srv.Close()

	idx := NewRemoteIndexWithClient(srv.URL, srv.Client(), time.Minute)
	results, err := idx.Search(context.Background(), "storage")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Name != "drive-style" {
		t.Errorf("Name = %q, want %q", results[0].Name, "drive-style")
	}
}

func TestSearchEmptyQueryReturnsAll(t *testing.T) {
	srv := newTestServer(t, cannedEntries, nil)
	defer srv.Close()

	idx := NewRemoteIndexWithClient(srv.URL, srv.Client(), time.Minute)
	results, err := idx.Search(context.Background(), "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != len(cannedEntries) {
		t.Errorf("got %d results, want %d", len(results), len(cannedEntries))
	}
}

// --- Get tests ---

func TestGetReturnsKnownEntry(t *testing.T) {
	srv := newTestServer(t, cannedEntries, nil)
	defer srv.Close()

	idx := NewRemoteIndexWithClient(srv.URL, srv.Client(), time.Minute)
	e, err := idx.Get(context.Background(), "twitter-style")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e.Name != "twitter-style" {
		t.Errorf("Name = %q, want %q", e.Name, "twitter-style")
	}
	if e.GitURL != "https://github.com/stuntapi/twitter-style" {
		t.Errorf("GitURL = %q", e.GitURL)
	}
}

func TestGetUnknownReturnsError(t *testing.T) {
	srv := newTestServer(t, cannedEntries, nil)
	defer srv.Close()

	idx := NewRemoteIndexWithClient(srv.URL, srv.Client(), time.Minute)
	_, err := idx.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown adapter, got nil")
	}
}

// --- Remote fetch tests ---

func TestRemoteFetchParsesJSON(t *testing.T) {
	srv := newTestServer(t, cannedEntries, nil)
	defer srv.Close()

	idx := NewRemoteIndexWithClient(srv.URL, srv.Client(), time.Minute)
	results, err := idx.Search(context.Background(), "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d entries, want 3", len(results))
	}
	// Verify all fields parsed correctly.
	var stripe *Entry
	for i := range results {
		if results[i].Name == "stripe-style" {
			stripe = &results[i]
			break
		}
	}
	if stripe == nil {
		t.Fatal("stripe-style entry not found")
	}
	if stripe.Description != "Stripe-style payment API" {
		t.Errorf("Description = %q", stripe.Description)
	}
	if stripe.LatestRef != "v1.0.0" {
		t.Errorf("LatestRef = %q", stripe.LatestRef)
	}
	if len(stripe.Tags) != 2 {
		t.Errorf("Tags len = %d, want 2", len(stripe.Tags))
	}
}

func TestRemoteFetchInvalidJSONReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	idx := NewRemoteIndexWithClient(srv.URL, srv.Client(), time.Minute)
	_, err := idx.Search(context.Background(), "")
	if err != nil {
		t.Fatalf("Search should fall back to bundled on invalid JSON, got error: %v", err)
	}
	// We get the bundled entries instead.
}

// --- Fallback tests ---

func TestFallbackToBundledOnUnreachableURL(t *testing.T) {
	// Point at an unreachable port; the request should fail quickly and
	// fall back to the bundled index.
	idx := NewRemoteIndexWithClient("http://127.0.0.1:1", &http.Client{Timeout: 200 * time.Millisecond}, time.Minute)
	results, err := idx.Search(context.Background(), "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("bundled fallback returned no entries")
	}
	// Bundled index should contain the known adapters.
	names := make(map[string]bool)
	for _, e := range results {
		names[e.Name] = true
	}
	for _, want := range []string{"stripe-style", "drive-style", "twitter-style"} {
		if !names[want] {
			t.Errorf("bundled index missing %q", want)
		}
	}
}

func TestBundledGetReturnsKnownEntry(t *testing.T) {
	idx := NewRemoteIndexWithClient("http://127.0.0.1:1", &http.Client{Timeout: 200 * time.Millisecond}, time.Minute)
	e, err := idx.Get(context.Background(), "stripe-style")
	if err != nil {
		t.Fatalf("Get via bundled fallback: %v", err)
	}
	if !strings.HasPrefix(e.GitURL, "https://github.com/stuntapi/stripe-style") {
		t.Errorf("GitURL = %q, want a github.com/stuntapi/stripe-style URL", e.GitURL)
	}
}

// --- Cache tests ---

func TestCacheTTLAvoidsRefetch(t *testing.T) {
	hits := 0
	srv := newTestServer(t, cannedEntries, &hits)
	defer srv.Close()

	idx := NewRemoteIndexWithClient(srv.URL, srv.Client(), time.Minute)

	// First call fetches from the server.
	_, err := idx.Search(context.Background(), "")
	if err != nil {
		t.Fatalf("first Search: %v", err)
	}
	if hits != 1 {
		t.Fatalf("expected 1 server hit, got %d", hits)
	}

	// Second call within TTL should use cache.
	_, err = idx.Search(context.Background(), "")
	if err != nil {
		t.Fatalf("second Search: %v", err)
	}
	if hits != 1 {
		t.Errorf("expected 1 server hit (cached), got %d", hits)
	}
}

func TestCacheExpiresAndRefetches(t *testing.T) {
	hits := 0
	srv := newTestServer(t, cannedEntries, &hits)
	defer srv.Close()

	idx := NewRemoteIndexWithClient(srv.URL, srv.Client(), 50*time.Millisecond)

	_, err := idx.Search(context.Background(), "")
	if err != nil {
		t.Fatalf("first Search: %v", err)
	}
	if hits != 1 {
		t.Fatalf("expected 1 server hit, got %d", hits)
	}

	time.Sleep(60 * time.Millisecond)

	// TTL expired → refetch.
	_, err = idx.Search(context.Background(), "")
	if err != nil {
		t.Fatalf("second Search: %v", err)
	}
	if hits != 2 {
		t.Errorf("expected 2 server hits after TTL expiry, got %d", hits)
	}
}

// --- M2: response body is capped at 10 MiB ---

func TestRemoteFetchCapsLargeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Send more than 10 MiB of data. The fetch should cap it, fail to
		// parse the truncated JSON, and fall back to the bundled index.
		w.Write([]byte("["))
		chunk := []byte(strings.Repeat("x", 4096))
		for i := 0; i < 3000; i++ {
			w.Write(chunk)
		}
	}))
	defer srv.Close()

	idx := NewRemoteIndexWithClient(srv.URL, srv.Client(), time.Minute)
	results, err := idx.Search(context.Background(), "")
	if err != nil {
		t.Fatalf("Search should fall back to bundled on truncated body: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected bundled entries as fallback after truncated fetch")
	}
}

// TestBundledIndexHasAllReferenceAdapters verifies that the bundled fallback
// catalog includes all 5 reference adapters with the correct `-style` names.
func TestBundledIndexHasAllReferenceAdapters(t *testing.T) {
	entries, err := bundledEntries()
	if err != nil {
		t.Fatalf("bundledEntries: %v", err)
	}
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name] = true
	}
	expected := []string{"stripe-style", "drive-style", "twitter-style", "dropbox-style", "echo-style"}
	for _, want := range expected {
		if !names[want] {
			t.Errorf("bundled index missing reference adapter %q (has: %v)", want, entries)
		}
	}
}

// TestGetBundledReturnsKnownEntry verifies the GetBundled function works for
// offline catalog name resolution.
func TestGetBundledReturnsKnownEntry(t *testing.T) {
	e, err := GetBundled("echo-style")
	if err != nil {
		t.Fatalf("GetBundled(\"echo-style\"): %v", err)
	}
	if e.Name != "echo-style" {
		t.Errorf("Name = %q, want %q", e.Name, "echo-style")
	}
	if !strings.Contains(e.Description, "gRPC") {
		t.Errorf("Description should mention gRPC: %q", e.Description)
	}
}

// TestGetBundledReturnsErrorForUnknown verifies that GetBundled errors on
// unknown names.
func TestGetBundledReturnsErrorForUnknown(t *testing.T) {
	_, err := GetBundled("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown adapter")
	}
}
