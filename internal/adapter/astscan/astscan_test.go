package astscan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/adapter"
)

func loadFixture(t *testing.T, name string) *adapter.Adapter {
	t.Helper()
	a, err := adapter.Load(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("Load(%s): %v", name, err)
	}
	return a
}

func tagsOf(t *testing.T, ets []EndpointTags, route, method string) []Tag {
	t.Helper()
	for _, et := range ets {
		if et.Route == route && et.Method == method {
			return et.Tags
		}
	}
	t.Fatalf("endpoint %s %s not scanned", method, route)
	return nil
}

func has(tags []Tag, want Tag) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// The auth tag must cross the lib.star boundary: the handler itself never
// mentions 401 — the preloaded helper does.
func TestScanAuthViaLibHelper(t *testing.T) {
	ets, err := Scan(loadFixture(t, "fixture-style"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	tags := tagsOf(t, ets, "/items", "POST")
	for _, want := range []Tag{TagAuth, TagBody, TagStateful} {
		if !has(tags, want) {
			t.Errorf("POST /items: want tag %q, got %v", want, TagsString(tags))
		}
	}
	if has(tags, TagErrors) {
		t.Errorf("POST /items: 201-only responder must not carry errors, got %v", TagsString(tags))
	}
}

// Lib attribution composes: _list_page (lib) contributes query+paginate,
// and the same lib chain carries the auth gate.
func TestScanLibChainAndBuiltins(t *testing.T) {
	ets, err := Scan(loadFixture(t, "fixture-style"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	tags := tagsOf(t, ets, "/items", "GET")
	for _, want := range []Tag{TagAuth, TagQuery, TagPaginate, TagFilter, TagStateful} {
		if !has(tags, want) {
			t.Errorf("GET /items: want tag %q, got %v", want, TagsString(tags))
		}
	}
}

// Path params via nested subscripts and the errors tag via a 404 literal.
func TestScanParamsAndErrors(t *testing.T) {
	ets, err := Scan(loadFixture(t, "fixture-style"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	tags := tagsOf(t, ets, "/items/{id}", "GET")
	if !has(tags, TagParams) || !has(tags, TagErrors) {
		t.Errorf("GET /items/{id}: want params+errors, got %v", TagsString(tags))
	}
}

// Mode-0 parsing accepts while-loops the resolver would reject.
func TestScanWhileLoopScript(t *testing.T) {
	ets, err := Scan(loadFixture(t, "fixture-style"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if tags := tagsOf(t, ets, "/loop", "GET"); !has(tags, TagQuery) {
		t.Errorf("GET /loop: want query, got %v", TagsString(tags))
	}
}

// A handler fn the manifest names but no script defines is a hard error.
func TestScanMissingHandlerFn(t *testing.T) {
	_, err := Scan(loadFixture(t, "fixture-broken-style"))
	if err == nil || !strings.Contains(err.Error(), "on_missing") {
		t.Fatalf("want missing-handler error naming on_missing, got %v", err)
	}
}

// Zero tags across a whole adapter means the scan went wrong, not that
// the adapter is trivial — also a hard error.
func TestScanZeroTagsAdapter(t *testing.T) {
	_, err := Scan(loadFixture(t, "fixture-empty-style"))
	if err == nil || !strings.Contains(err.Error(), "no tags derived") {
		t.Fatalf("want zero-tags error, got %v", err)
	}
}

// Every reference adapter scans clean and yields at least one tag — the
// same sweep shape as the QC boot guard.
func TestScanAllReferenceAdapters(t *testing.T) {
	root := filepath.Join("..", "..", "..", "adapters")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "adapter.yaml")); err != nil {
			continue
		}
		a, err := adapter.Load(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatalf("Load(%s): %v", e.Name(), err)
		}
		if _, err := Scan(a); err != nil {
			t.Fatalf("Scan(%s): %v", e.Name(), err)
		}
		count++
	}
	if count < 90 {
		t.Fatalf("expected to sweep ~98 reference adapters, got %d", count)
	}
}
