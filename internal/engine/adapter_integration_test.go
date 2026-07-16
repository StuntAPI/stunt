package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/rules"
)

// writeFile lays out a file in dir, creating parent directories.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

const chargesAdapterYAML = `
id: charges
name: Charges
endpoints:
  - route: /charges
    method: POST
    handler: scripts/charges.star#on_create
  - route: /charges/{id}
    method: GET
    handler: scripts/charges.star#on_get
resources:
  - name: charges
    kind: collection
`

const chargesStar = `
def on_create(req):
    c = store_collection("charges")
    id = c.insert(req["body"])
    return respond(201, {"id": id})

def on_get(req):
    id = req["params"]["id"]
    c = store_collection("charges")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": "not found"})
    return respond(200, doc)
`

// TestAdapterStatefulRoundTrip proves that an adapter-backed service can
// insert a document (POST /charges) and read it back (GET /charges/{id}) —
// state persists across requests in the per-service store.
func TestAdapterStatefulRoundTrip(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", chargesAdapterYAML)
	writeFile(t, adapterDir, "scripts/charges.star", chargesStar)

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"charges": {Adapter: adapterDir},
			// Regression: an inline-rules-only service must still work.
			"hello": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/ok"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"msg": "hi"}}}},
			}},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(30 * time.Millisecond)

	chargesURL := addrs["charges"]
	helloURL := addrs["hello"]

	// --- POST /charges {amount:5000} → 201 + {id} ---
	body, status := postJSON(t, chargesURL+"/charges", map[string]any{"amount": 5000})
	if status != 201 {
		t.Fatalf("POST /charges -> status %d, want 201; body %s", status, body)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("unmarshal POST response: %v (body %s)", err, body)
	}
	id, ok := created["id"].(string)
	if !ok || id == "" {
		t.Fatalf("POST response has no id: %v", created)
	}

	// --- GET /charges/{id} → 200 + stored doc ---
	body, status = get2(t, chargesURL+"/charges/"+id)
	if status != 200 {
		t.Fatalf("GET /charges/%s -> status %d, want 200; body %s", id, status, body)
	}
	var fetched map[string]any
	if err := json.Unmarshal([]byte(body), &fetched); err != nil {
		t.Fatalf("unmarshal GET response: %v (body %s)", err, body)
	}
	if amount, ok := fetched["amount"].(float64); !ok || amount != 5000 {
		t.Fatalf("fetched amount = %v, want 5000 (body %s)", fetched["amount"], body)
	}
	if fetched["id"] != id {
		t.Fatalf("fetched id = %v, want %s", fetched["id"], id)
	}

	// --- GET /charges/nonexistent → 404 ---
	_, status = get2(t, chargesURL+"/charges/nope")
	if status != 404 {
		t.Fatalf("GET /charges/nope -> status %d, want 404", status)
	}

	// --- Regression: inline-rules service still works ---
	body, status = get2(t, helloURL+"/ok")
	if status != 200 || body != `{"msg":"hi"}` {
		t.Fatalf("GET /ok (hello service) -> status %d body %q, want 200 {\"msg\":\"hi\"}", status, body)
	}
}

// --- error handling test ---

const errorAdapterYAML = `
id: error-svc
name: Error Service
endpoints:
  - route: /boom
    method: GET
    handler: scripts/boom.star#on_get
`

const errorStar = `
def on_get(req):
    fail("kaboom")
`

// TestAdapterHandlerError verifies that a panicking/erroring Starlark handler
// returns 500 with a JSON error body rather than crashing the server.
func TestAdapterHandlerError(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", errorAdapterYAML)
	writeFile(t, adapterDir, "scripts/boom.star", errorStar)

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"err": {Adapter: adapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(30 * time.Millisecond)

	body, status := get2(t, addrs["err"]+"/boom")
	if status != 500 {
		t.Fatalf("GET /boom -> status %d, want 500; body %s", status, body)
	}
	if !strings.Contains(body, "error") {
		t.Fatalf("expected JSON error body, got %s", body)
	}
}

// --- rules overlay test ---

const overlayAdapterYAML = `
id: overlay-svc
name: Overlay Service
endpoints:
  - route: /items
    method: GET
    rules:
      - name: items-ok
        match: { method: GET, path: /items }
        respond: { status: 200, body: { inline: { source: adapter } } }
rules:
  - name: catchall
    match: { path: /** }
    respond: { status: 404, body: { inline: { error: not_found } } }
`

// TestAdapterRulesOverlay verifies that an adapter with rules-only endpoints
// (no Starlark handler) dispatches through the rules engine, including
// adapter top-level rules as a catch-all.
func TestAdapterRulesOverlay(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", overlayAdapterYAML)

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"ovl": {Adapter: adapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(30 * time.Millisecond)

	// Matched endpoint rule
	body, status := get2(t, addrs["ovl"]+"/items")
	if status != 200 || !strings.Contains(body, `"source":"adapter"`) {
		t.Fatalf("GET /items -> status %d body %q, want 200 with source:adapter", status, body)
	}

	// Adapter top-level catch-all rule
	body, status = get2(t, addrs["ovl"]+"/nope")
	if status != 404 || !strings.Contains(body, "not_found") {
		t.Fatalf("GET /nope -> status %d body %q, want 404 not_found", status, body)
	}
}

const seededAdapterYAML = `
id: seeded
name: Seeded
endpoints:
  - route: /items
    method: GET
    handler: scripts/items.star#on_list
resources:
  - name: items
    kind: collection
    seed: fixtures/items.jsonl
`

const seededStar = `
def on_list(req):
    c = store_collection("items")
    docs = c.list()
    return respond(200, {"count": len(docs)})
`

const seedItemsJSONL = `{"id":"i1","name":"widget"}` + "\n" + `{"id":"i2","name":"gadget"}` + "\n"

// TestSeedNoDuplicateOnRestart proves that re-opening the same state dir
// does not duplicate seed rows (C3).
func TestSeedNoDuplicateOnRestart(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", seededAdapterYAML)
	writeFile(t, adapterDir, "scripts/items.star", seededStar)
	writeFile(t, adapterDir, "fixtures/items.jsonl", seedItemsJSONL)

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	makeManifest := func() *manifest.Manifest {
		return &manifest.Manifest{
			Path:    manifestPath,
			Version: 1,
			Network: manifest.Network{Mode: "port", BasePort: 0},
			Services: map[string]manifest.Service{
				"seeded": {Adapter: adapterDir},
			},
		}
	}

	// First engine open: seed 2 rows.
	e1, err := New(makeManifest())
	if err != nil {
		t.Fatalf("engine.New (first): %v", err)
	}
	addrs1, cancel1, err := e1.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest (first): %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	body, status := get2(t, addrs1["seeded"]+"/items")
	if status != 200 {
		t.Fatalf("GET /items (first) -> status %d, body %s", status, body)
	}
	if !strings.Contains(body, `"count":2`) {
		t.Fatalf("first open: body = %s, want count 2", body)
	}
	cancel1()
	e1.Close()

	// Second engine open (restart with same state dir): no duplicates.
	e2, err := New(makeManifest())
	if err != nil {
		t.Fatalf("engine.New (restart): %v", err)
	}
	defer e2.Close()
	addrs2, cancel2, err := e2.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest (restart): %v", err)
	}
	defer cancel2()
	time.Sleep(30 * time.Millisecond)

	body, status = get2(t, addrs2["seeded"]+"/items")
	if status != 200 {
		t.Fatalf("GET /items (restart) -> status %d, body %s", status, body)
	}
	if !strings.Contains(body, `"count":2`) {
		t.Fatalf("restart: body = %s, want count 2 (no duplicates)", body)
	}
}

// --- I2: concurrent requests with shared rng/faker must be race-free ---

const chanceAdapterYAML = `
id: chance-svc
name: Chance Service
endpoints:
  - route: /maybe
    method: GET
    rules:
      - name: sometimes-fail
        match: { method: GET, path: /maybe }
        when: { chance: 50 }
        respond: { status: 200, body: { inline: { result: hit } } }
      - name: default
        match: { method: GET, path: /maybe }
        respond: { status: 200, body: { inline: { result: miss } } }
`

func TestConcurrentRulesRaceFree(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", chanceAdapterYAML)

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"chance": {Adapter: adapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()
	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(30 * time.Millisecond)

	url := addrs["chance"] + "/maybe"
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(url)
			if err != nil {
				t.Errorf("GET: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
}

// --- HTTP helpers for this test file ---

func postJSON(t *testing.T, url string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func get2(t *testing.T, url string) (string, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
