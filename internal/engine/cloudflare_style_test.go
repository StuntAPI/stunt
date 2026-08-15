package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestCloudflareStyleAdapter exercises the Cloudflare-style adapter:
//
//   - List zones → seeded zone appears
//   - Create zone → appears in list
//   - Get single zone
//   - List DNS records
//   - Deploy worker → appears in list (STATEFUL)
//   - List R2 buckets → create bucket → appears (STATEFUL)
//   - Create D1 database → query returns rows (STATEFUL)
//   - Without auth → 401
//   - X-Auth-Email + X-Auth-Key also works
func TestCloudflareStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "cloudflare-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"cf": {Adapter: absAdapterDir},
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
	time.Sleep(50 * time.Millisecond)

	base := addrs["cf"]

	const bearer = "Bearer stunt-api-token-123"
	const accountID = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"

	// ===== List zones → seeded zone appears =====

	body, status := cfGet(t, base+"/zones", bearer)
	if status != 200 {
		t.Fatalf("list zones -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "stunt.dev") {
		t.Fatalf("list zones: missing seeded stunt.dev; body %s", body)
	}
	if !strings.Contains(body, "result_info") {
		t.Fatalf("list zones: missing result_info; body %s", body)
	}
	if !strings.Contains(body, "name_servers") {
		t.Fatalf("list zones: missing name_servers; body %s", body)
	}

	// ===== Create zone =====

	zoneBody, _ := json.Marshal(map[string]string{"name": "example.org"})
	body, status = cfPost(t, base+"/zones", bearer, zoneBody)
	if status != 200 {
		t.Fatalf("create zone -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "example.org") {
		t.Fatalf("create zone: missing zone name; body %s", body)
	}

	// Verify it appears in list
	body, status = cfGet(t, base+"/zones", bearer)
	if !strings.Contains(body, "example.org") {
		t.Fatalf("create zone: new zone not in list; body %s", body)
	}

	// ===== Get single zone =====

	body, status = cfGet(t, base+"/zones/023e105f4ecef8ad9ca31a8372d0c353", bearer)
	if status != 200 {
		t.Fatalf("get zone -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "stunt.dev") {
		t.Fatalf("get zone: missing zone name; body %s", body)
	}

	// ===== List DNS records =====

	body, status = cfGet(t, base+"/zones/023e105f4ecef8ad9ca31a8372d0c353/dns_records", bearer)
	if status != 200 {
		t.Fatalf("list dns_records -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "\"type\":\"A\"") {
		t.Fatalf("dns records: missing type A; body %s", body)
	}
	if !strings.Contains(body, "CNAME") {
		t.Fatalf("dns records: missing CNAME; body %s", body)
	}

	// ===== Deploy worker → appears in list (STATEFUL) =====

	body, status = cfPut(t, base+"/accounts/"+accountID+"/workers/scripts/my-worker", bearer, []byte(`{"main_module":"addEventListener"}`))
	if status != 200 {
		t.Fatalf("deploy worker -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "my-worker") {
		t.Fatalf("deploy worker: missing script name; body %s", body)
	}

	// Verify it appears in list
	body, status = cfGet(t, base+"/accounts/"+accountID+"/workers/scripts", bearer)
	if status != 200 {
		t.Fatalf("list workers -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "my-worker") {
		t.Fatalf("deploy worker: script not in list; body %s", body)
	}

	// ===== R2 bucket create → appears in list (STATEFUL) =====

	bucketBody, _ := json.Marshal(map[string]string{"name": "test-bucket"})
	body, status = cfPost(t, base+"/accounts/"+accountID+"/r2/buckets", bearer, bucketBody)
	if status != 200 {
		t.Fatalf("create bucket -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "test-bucket") {
		t.Fatalf("create bucket: missing bucket name; body %s", body)
	}

	body, status = cfGet(t, base+"/accounts/"+accountID+"/r2/buckets", bearer)
	if status != 200 {
		t.Fatalf("list buckets -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "test-bucket") {
		t.Fatalf("list buckets: created bucket not found; body %s", body)
	}

	// ===== D1 database create → query returns rows (STATEFUL) =====

	dbBody, _ := json.Marshal(map[string]string{"name": "test-db"})
	body, status = cfPost(t, base+"/accounts/"+accountID+"/d1/database", bearer, dbBody)
	if status != 200 {
		t.Fatalf("create database -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "test-db") {
		t.Fatalf("create database: missing db name; body %s", body)
	}

	// Extract the database UUID from the create response
	var createResp map[string]any
	json.Unmarshal([]byte(body), &createResp)
	result, _ := createResp["result"].(map[string]any)
	dbUUID, _ := result["uuid"].(string)
	if dbUUID == "" {
		t.Fatalf("create database: missing uuid in response; body %s", body)
	}

	// Query the database: the D1 engine requires a real table model, so
	// a SELECT against a missing table is a 400 D1_ERROR.
	queryBody, _ := json.Marshal(map[string]string{"sql": "SELECT * FROM users"})
	body, status = cfPost(t, base+"/accounts/"+accountID+"/d1/database/"+dbUUID+"/query", bearer, queryBody)
	if status != 400 {
		t.Fatalf("query missing table -> status %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "no such table: users") || !strings.Contains(body, "7501") {
		t.Fatalf("query missing table: wrong error envelope; body %s", body)
	}

	// ===== Firewall rules + page rules =====

	body, status = cfGet(t, base+"/zones/023e105f4ecef8ad9ca31a8372d0c353/firewall/rules", bearer)
	if status != 200 {
		t.Fatalf("firewall rules -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "block") {
		t.Fatalf("firewall rules: missing action; body %s", body)
	}

	body, status = cfGet(t, base+"/zones/023e105f4ecef8ad9ca31a8372d0c353/page_rules", bearer)
	if status != 200 {
		t.Fatalf("page rules -> status %d, want 200; body %s", status, body)
	}

	// ===== Purge cache =====

	purgeBody, _ := json.Marshal(map[string]string{"purge_everything": "true"})
	body, status = cfPost(t, base+"/zones/023e105f4ecef8ad9ca31a8372d0c353/purge_cache", bearer, purgeBody)
	if status != 200 {
		t.Fatalf("purge cache -> status %d, want 200; body %s", status, body)
	}

	// ===== Without auth → 401 =====

	body, status = cfGetNoAuth(t, base+"/zones")
	if status != 401 {
		t.Fatalf("list zones without auth -> status %d, want 401; body %s", status, body)
	}
	if !strings.Contains(body, "Authentication error") {
		t.Fatalf("no auth: missing Authentication error; body %s", body)
	}

	// ===== X-Auth-Email + X-Auth-Key also works =====

	body, status = cfGetWithKey(t, base+"/zones", "user@stunt.dev", "global-api-key-abc")
	if status != 200 {
		t.Fatalf("list zones with global key -> status %d, want 200; body %s", status, body)
	}

	// ===== Duplicate bucket → 409 =====

	body, status = cfPost(t, base+"/accounts/"+accountID+"/r2/buckets", bearer, bucketBody)
	if status != 409 {
		t.Fatalf("duplicate bucket -> status %d, want 409; body %s", status, body)
	}

	// ===== Nonexistent zone → 404 =====

	body, status = cfGet(t, base+"/zones/00000000000000000000000000000000", bearer)
	if status != 404 {
		t.Fatalf("get nonexistent zone -> status %d, want 404; body %s", status, body)
	}
}

// === Cloudflare test helpers ===

func cfGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func cfGetNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func cfGetWithKey(t *testing.T, rawurl, email, key string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Auth-Email", email)
	req.Header.Set("X-Auth-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func cfPost(t *testing.T, rawurl, auth string, body []byte) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func cfPut(t *testing.T, rawurl, auth string, body []byte) (string, int) {
	t.Helper()
	req, err := http.NewRequest("PUT", rawurl, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func cfPatch(t *testing.T, rawurl, auth string, body []byte) (string, int) {
	t.Helper()
	req, err := http.NewRequest("PATCH", rawurl, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func cfDelete(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// cfServe spins up the cloudflare-style adapter and returns its base URL.
func cfServe(t *testing.T) string {
	t.Helper()
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "cloudflare-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"cf": {Adapter: adapterDir},
		},
	}
	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	t.Cleanup(cancel)
	time.Sleep(50 * time.Millisecond)
	return addrs["cf"]
}

// cfJSON decodes a Cloudflare envelope and returns result.
func cfResult(t *testing.T, body string) any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("bad JSON: %v; body %s", err, body)
	}
	return resp["result"]
}

const cfZoneID = "023e105f4ecef8ad9ca31a8372d0c353"

// TestCloudflareStyleDNSRecordLifecycle exercises the full DNS record
// lifecycle: create -> list/get -> PUT -> PATCH -> DELETE, plus the
// validation failure paths (unknown type, out-of-range ttl).
func TestCloudflareStyleDNSRecordLifecycle(t *testing.T) {
	base := cfServe(t)
	const bearer = "Bearer stunt-api-token-123"
	zone := base + "/zones/" + cfZoneID + "/dns_records"

	// ===== Failure: unknown record type -> 400 =====
	bad, _ := json.Marshal(map[string]any{"type": "BOGUS", "name": "stunt.dev", "content": "192.0.2.9"})
	body, status := cfPost(t, zone, bearer, bad)
	if status != 400 {
		t.Fatalf("create bogus type -> status %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "unknown record type") {
		t.Fatalf("create bogus type: missing message; body %s", body)
	}

	// ===== Failure: ttl below 60 (and not 1) -> 400 =====
	badTTL, _ := json.Marshal(map[string]any{"type": "A", "name": "stunt.dev", "content": "192.0.2.9", "ttl": 30})
	body, status = cfPost(t, zone, bearer, badTTL)
	if status != 400 {
		t.Fatalf("create bad ttl -> status %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "'ttl' must be 1 (auto)") {
		t.Fatalf("create bad ttl: missing message; body %s", body)
	}

	// ===== Create -> appears in list =====
	good, _ := json.Marshal(map[string]any{"type": "TXT", "name": "_verify.stunt.dev", "content": "stunt-token", "ttl": 3600})
	body, status = cfPost(t, zone, bearer, good)
	if status != 200 {
		t.Fatalf("create record -> status %d, want 200; body %s", status, body)
	}
	res, _ := cfResult(t, body).(map[string]any)
	recordID, _ := res["id"].(string)
	if recordID == "" || res["type"] != "TXT" || res["ttl"].(float64) != 3600 {
		t.Fatalf("create record: wrong envelope; body %s", body)
	}
	if _, ok := res["meta"].(map[string]any); !ok {
		t.Fatalf("create record: missing meta object; body %s", body)
	}

	body, status = cfGet(t, zone+"?type=TXT", bearer)
	if status != 200 || !strings.Contains(body, "_verify.stunt.dev") {
		t.Fatalf("list filtered by type: record missing; status %d body %s", status, body)
	}

	// ===== Get =====
	body, status = cfGet(t, zone+"/"+recordID, bearer)
	if status != 200 || !strings.Contains(body, "stunt-token") {
		t.Fatalf("get record: status %d body %s", status, body)
	}

	// ===== PATCH content =====
	patch, _ := json.Marshal(map[string]any{"content": "stunt-token-2"})
	body, status = cfPatch(t, zone+"/"+recordID, bearer, patch)
	if status != 200 || !strings.Contains(body, "stunt-token-2") {
		t.Fatalf("patch record: status %d body %s", status, body)
	}

	// ===== PUT full replace (proxied forces ttl 1) =====
	put, _ := json.Marshal(map[string]any{"type": "A", "name": "api.stunt.dev", "content": "192.0.2.9", "proxied": true, "ttl": 3600})
	body, status = cfPut(t, zone+"/"+recordID, bearer, put)
	if status != 200 {
		t.Fatalf("put record: status %d body %s", status, body)
	}
	res, _ = cfResult(t, body).(map[string]any)
	if res["ttl"].(float64) != 1 || res["proxied"] != true {
		t.Fatalf("put record: proxied must force ttl 1; body %s", body)
	}

	// ===== DELETE -> gone =====
	body, status = cfDelete(t, zone+"/"+recordID, bearer)
	if status != 200 || !strings.Contains(body, recordID) {
		t.Fatalf("delete record: status %d body %s", status, body)
	}
	body, status = cfGet(t, zone+"/"+recordID, bearer)
	if status != 404 {
		t.Fatalf("get deleted record -> status %d, want 404; body %s", status, body)
	}
}

// TestCloudflareStyleFirewallAndPageRules exercises the store-backed CRUD
// lifecycle for zone firewall rules and page rules.
func TestCloudflareStyleFirewallAndPageRules(t *testing.T) {
	base := cfServe(t)
	const bearer = "Bearer stunt-api-token-123"

	// ===== Firewall rules =====
	fw := base + "/zones/" + cfZoneID + "/firewall/rules"

	// Seeded canned rule still lists.
	body, status := cfGet(t, fw, bearer)
	if status != 200 || !strings.Contains(body, "block") {
		t.Fatalf("list firewall rules: status %d body %s", status, body)
	}

	// Failure: unknown action -> 400.
	bad, _ := json.Marshal(map[string]any{"action": "zap", "filter": map[string]string{"expression": "(ip.src eq 192.0.2.0/24)"}})
	body, status = cfPost(t, fw, bearer, bad)
	if status != 400 || !strings.Contains(body, "unknown action") {
		t.Fatalf("create bad firewall rule: status %d body %s", status, body)
	}

	// Create (single-object form of the real array body).
	good, _ := json.Marshal(map[string]any{
		"action":      "managed_challenge",
		"description": "challenge bad bots",
		"filter":      map[string]string{"expression": "(http.user_agent contains \"bot\")"},
	})
	body, status = cfPost(t, fw, bearer, good)
	if status != 200 {
		t.Fatalf("create firewall rule: status %d body %s", status, body)
	}
	created, _ := cfResult(t, body).([]any)
	if len(created) != 1 {
		t.Fatalf("create firewall rule: want array result; body %s", body)
	}
	rule := created[0].(map[string]any)
	ruleID, _ := rule["id"].(string)
	if ruleID == "" || rule["action"] != "managed_challenge" {
		t.Fatalf("create firewall rule: wrong envelope; body %s", body)
	}

	// Get / PATCH / DELETE.
	body, status = cfGet(t, fw+"/"+ruleID, bearer)
	if status != 200 || !strings.Contains(body, "challenge bad bots") {
		t.Fatalf("get firewall rule: status %d body %s", status, body)
	}
	patch, _ := json.Marshal(map[string]any{"description": "challenge worse bots"})
	body, status = cfPatch(t, fw+"/"+ruleID, bearer, patch)
	if status != 200 || !strings.Contains(body, "challenge worse bots") {
		t.Fatalf("patch firewall rule: status %d body %s", status, body)
	}
	body, status = cfDelete(t, fw+"/"+ruleID, bearer)
	if status != 200 {
		t.Fatalf("delete firewall rule: status %d body %s", status, body)
	}
	body, status = cfGet(t, fw+"/"+ruleID, bearer)
	if status != 404 {
		t.Fatalf("get deleted firewall rule -> status %d, want 404; body %s", status, body)
	}

	// ===== Page rules =====
	pr := base + "/zones/" + cfZoneID + "/page_rules"

	body, status = cfGet(t, pr+"?status=active", bearer)
	if status != 200 || !strings.Contains(body, "browser_cache_ttl") {
		t.Fatalf("list page rules: status %d body %s", status, body)
	}

	prBody, _ := json.Marshal(map[string]any{
		"targets": []any{map[string]any{"target": "url", "constraint": map[string]any{"operator": "matches", "value": "stunt.dev/cache/*"}}},
		"actions": []any{map[string]any{"id": "cache_level", "value": "bypass"}},
		"status":  "active",
	})
	body, status = cfPost(t, pr, bearer, prBody)
	if status != 200 {
		t.Fatalf("create page rule: status %d body %s", status, body)
	}
	pRes, _ := cfResult(t, body).(map[string]any)
	prID, _ := pRes["id"].(string)
	if prID == "" || pRes["status"] != "active" || pRes["priority"].(float64) != 2 {
		t.Fatalf("create page rule: wrong envelope; body %s", body)
	}

	patch, _ = json.Marshal(map[string]any{"status": "paused"})
	body, status = cfPatch(t, pr+"/"+prID, bearer, patch)
	if status != 200 || !strings.Contains(body, "paused") {
		t.Fatalf("patch page rule: status %d body %s", status, body)
	}
	body, status = cfDelete(t, pr+"/"+prID, bearer)
	if status != 200 {
		t.Fatalf("delete page rule: status %d body %s", status, body)
	}
	body, status = cfGet(t, pr+"/"+prID, bearer)
	if status != 404 {
		t.Fatalf("get deleted page rule -> status %d, want 404; body %s", status, body)
	}
}

// TestCloudflareStyleD1QueryEngine proves the stored table model: CREATE
// TABLE registers the schema, INSERT/UPDATE with bound parameters mutate
// persisted rows, SELECT maps WHERE/ORDER BY/LIMIT onto query_select, and
// unsupported SQL returns 400 with the Cloudflare error envelope.
func TestCloudflareStyleD1QueryEngine(t *testing.T) {
	base := cfServe(t)
	const bearer = "Bearer stunt-api-token-123"
	const accountID = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"

	dbBody, _ := json.Marshal(map[string]string{"name": "sqldb"})
	body, status := cfPost(t, base+"/accounts/"+accountID+"/d1/database", bearer, dbBody)
	if status != 200 {
		t.Fatalf("create database -> %d; body %s", status, body)
	}
	dbUUID, _ := cfResult(t, body).(map[string]any)["uuid"].(string)
	q := base + "/accounts/" + accountID + "/d1/database/" + dbUUID + "/query"

	exec := func(sql string, params []any) (string, int) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"sql": sql, "params": params})
		return cfPost(t, q, bearer, b)
	}

	// CREATE TABLE (IF NOT EXISTS registers the schema; a second plain
	// CREATE errors like real D1).
	body, status = exec("CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY, email TEXT, name TEXT, age INTEGER)", nil)
	if status != 200 || !strings.Contains(body, "\"success\":true") {
		t.Fatalf("create table: status %d body %s", status, body)
	}
	body, status = exec("CREATE TABLE users (id INTEGER)", nil)
	if status != 400 || !strings.Contains(body, "already exists") {
		t.Fatalf("recreate table: status %d body %s", status, body)
	}

	// INSERT with bound parameters (three rows).
	for i, name := range []string{"alice", "bob", "carol"} {
		body, status = exec("INSERT INTO users (id, email, name, age) VALUES (?, ?, ?, ?)", []any{i + 1, name + "@stunt.dev", name, 20 + i*10})
		if status != 200 || !strings.Contains(body, "\"changes\":1") {
			t.Fatalf("insert %s: status %d body %s", name, status, body)
		}
	}

	// SELECT * with WHERE + ORDER BY + LIMIT.
	body, status = exec("SELECT * FROM users WHERE age > ? ORDER BY age DESC LIMIT 2", []any{15})
	if status != 200 {
		t.Fatalf("select: status %d body %s", status, body)
	}
	results, _ := cfResult(t, body).([]any)
	stmt, _ := results[0].(map[string]any)
	rows, _ := stmt["results"].([]any)
	if len(rows) != 2 {
		t.Fatalf("select: want 2 rows, got %v; body %s", stmt["results"], body)
	}
	first := rows[0].(map[string]any)
	if first["name"] != "carol" || first["email"] != "carol@stunt.dev" {
		t.Fatalf("select: wrong ordering/values; body %s", body)
	}
	if _, ok := first["_rid"]; ok {
		t.Fatalf("select: internal _rid leaked into results; body %s", body)
	}

	// Projection: SELECT id, name.
	body, status = exec("SELECT id, name FROM users ORDER BY id ASC", nil)
	if status != 200 {
		t.Fatalf("projected select: status %d body %s", status, body)
	}
	results, _ = cfResult(t, body).([]any)
	stmt, _ = results[0].(map[string]any)
	rows, _ = stmt["results"].([]any)
	proj := rows[0].(map[string]any)
	if len(proj) != 2 {
		t.Fatalf("projected select: want 2 keys, got %v; body %s", proj, body)
	}

	// UPDATE with parameters.
	body, status = exec("UPDATE users SET name = ? WHERE id = ?", []any{"carolyn", 3})
	if status != 200 || !strings.Contains(body, "\"changes\":1") {
		t.Fatalf("update: status %d body %s", status, body)
	}
	body, status = exec("SELECT name FROM users WHERE id = 3", nil)
	if status != 200 || !strings.Contains(body, "carolyn") {
		t.Fatalf("select after update: status %d body %s", status, body)
	}

	// DELETE.
	body, status = exec("DELETE FROM users WHERE id = 2", nil)
	if status != 200 || !strings.Contains(body, "\"changes\":1") {
		t.Fatalf("delete: status %d body %s", status, body)
	}
	body, status = exec("SELECT id FROM users", nil)
	if status != 200 || strings.Contains(body, "bob@stunt.dev") {
		t.Fatalf("select after delete: row still present; body %s", body)
	}

	// Unknown SQL -> 400 with the CF error envelope.
	body, status = exec("DROP TABLE users", nil)
	if status != 400 || !strings.Contains(body, "unsupported SQL statement") || !strings.Contains(body, "7501") {
		t.Fatalf("drop table: status %d body %s", status, body)
	}

	// Not enough bound parameters -> 400.
	body, status = exec("INSERT INTO users (id, email, name, age) VALUES (?, ?, ?, ?)", []any{9})
	if status != 400 || !strings.Contains(body, "not enough parameters") {
		t.Fatalf("insert missing params: status %d body %s", status, body)
	}
}

// TestCloudflareStyleWorkerMultipartDeploy deploys a Worker via a real
// multipart/form-data body (metadata + module parts) and verifies the
// script lands in the scripts list with a stable id.
func TestCloudflareStyleWorkerMultipartDeploy(t *testing.T) {
	base := cfServe(t)
	const bearer = "Bearer stunt-api-token-123"
	const accountID = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"

	metadata := `{"main_module":"worker.js","bindings":[]}`
	script := "export default { fetch() { return new Response('hi'); } }"

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	mw, _ := w.CreatePart(textproto.MIMEHeader{"Content-Disposition": []string{"form-data; name=\"metadata\""}, "Content-Type": []string{"application/json"}})
	mw.Write([]byte(metadata))
	fw, _ := w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": []string{"form-data; name=\"worker.js\"; filename=\"worker.js\""},
		"Content-Type":        []string{"application/javascript+module"},
	})
	fw.Write([]byte(script))
	w.Close()

	url := base + "/accounts/" + accountID + "/workers/scripts/mp-worker"
	req, err := http.NewRequest("PUT", url, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", bearer)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if resp.StatusCode != 200 {
		t.Fatalf("multipart deploy -> status %d, want 200; body %s", resp.StatusCode, body)
	}
	res, _ := cfResult(t, body).(map[string]any)
	workerID, _ := res["id"].(string)
	if workerID == "" {
		t.Fatalf("multipart deploy: missing id; body %s", body)
	}

	// Redeploy keeps the same worker id.
	body, status := cfPutMultipart(t, url, bearer, w.FormDataContentType(), buf.Bytes())
	if status != 200 {
		t.Fatalf("multipart redeploy -> status %d; body %s", status, body)
	}
	if res2, _ := cfResult(t, body).(map[string]any); res2["id"] != workerID {
		t.Fatalf("redeploy changed worker id: %v vs %v", res2["id"], workerID)
	}

	// The script shows up in the list and by name.
	body, status = cfGet(t, base+"/accounts/"+accountID+"/workers/scripts", bearer)
	if status != 200 || !strings.Contains(body, "mp-worker") {
		t.Fatalf("list scripts after multipart deploy: status %d body %s", status, body)
	}
	body, status = cfGet(t, url, bearer)
	if status != 200 || !strings.Contains(body, workerID) {
		t.Fatalf("get script after multipart deploy: status %d body %s", status, body)
	}
}

func cfPutMultipart(t *testing.T, rawurl, auth, contentType string, body []byte) (string, int) {
	t.Helper()
	req, err := http.NewRequest("PUT", rawurl, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// TestCloudflareStyleAsyncLifecycles proves the derive-on-read state
// machines: a created zone goes pending -> initializing -> active, and a
// Worker deployment goes active -> in_progress -> deployed (failed with the
// simulator-only simulate_fail flag).
func TestCloudflareStyleAsyncLifecycles(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "cloudflare-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"cf": {Adapter: adapterDir},
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
	time.Sleep(50 * time.Millisecond)
	base := addrs["cf"]

	const bearer = "Bearer stunt-api-token-123"
	const accountID = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"

	// Create a zone: immediately pending (or initializing).
	zoneBody, _ := json.Marshal(map[string]any{"name": "lifecycle.example"})
	body, status := cfPost(t, base+"/zones", bearer, zoneBody)
	if status != 200 {
		t.Fatalf("create zone -> %d; body %s", status, body)
	}
	var zoneResp map[string]any
	json.Unmarshal([]byte(body), &zoneResp)
	zoneResult := zoneResp["result"].(map[string]any)
	zoneID, _ := zoneResult["id"].(string)
	if zoneResult["status"] != "pending" && zoneResult["status"] != "initializing" {
		t.Fatalf("immediate zone status = %v, want pending|initializing", zoneResult["status"])
	}

	// Deploy a worker normally and with simulate_fail.
	if _, status := cfPut(t, base+"/accounts/"+accountID+"/workers/scripts/lc-worker", bearer, []byte(`{"main_module":"ok"}`)); status != 200 {
		t.Fatalf("deploy ok worker -> %d", status)
	}
	if _, status := cfPut(t, base+"/accounts/"+accountID+"/workers/scripts/lc-worker-fail", bearer, []byte(`{"main_module":"bad","simulate_fail":true}`)); status != 200 {
		t.Fatalf("deploy fail worker -> %d", status)
	}

	// After the 3s window: zone active, deployments deployed / failed.
	time.Sleep(3500 * time.Millisecond)

	body, status = cfGet(t, base+"/zones/"+zoneID, bearer)
	if status != 200 {
		t.Fatalf("get zone -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &zoneResp)
	if zoneResp["result"].(map[string]any)["status"] != "active" {
		t.Fatalf("zone after window = %v, want active", zoneResp["result"].(map[string]any)["status"])
	}

	body, status = cfGet(t, base+"/accounts/"+accountID+"/workers/scripts/lc-worker/deployments", bearer)
	if status != 200 {
		t.Fatalf("get deployments -> %d; body %s", status, body)
	}
	var depResp map[string]any
	json.Unmarshal([]byte(body), &depResp)
	deps := depResp["result"].([]any)
	if len(deps) != 1 || deps[0].(map[string]any)["status"] != "deployed" {
		t.Fatalf("ok deployment = %v, want one deployed", depResp["result"])
	}

	body, status = cfGet(t, base+"/accounts/"+accountID+"/workers/scripts/lc-worker-fail/deployments", bearer)
	if status != 200 {
		t.Fatalf("get fail deployments -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &depResp)
	deps = depResp["result"].([]any)
	if len(deps) != 1 || deps[0].(map[string]any)["status"] != "failed" {
		t.Fatalf("fail deployment = %v, want one failed", depResp["result"])
	}
}

func TestCloudflareStyleExtraPaths(t *testing.T) {
	base := cfServe(t)
	const bearer = "Bearer stunt-api-token-123"
	const accountID = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"

	check := func(name, body string, status int, want int) {
		t.Helper()
		if status != want {
			t.Fatalf("%s -> %d, want %d; body %s", name, status, want, body)
		}
		if strings.Contains(body, "500") && status >= 500 {
			t.Fatalf("%s -> 500; body %s", name, body)
		}
	}

	// Firewall batch POST (array body, the real shape).
	batch, _ := json.Marshal([]any{
		map[string]any{"action": "allow", "filter": map[string]string{"expression": "(ip.src eq 10.0.0.1)"}},
		map[string]any{"action": "log", "filter": map[string]string{"expression": "(ip.src eq 10.0.0.2)"}},
	})
	body, status := cfPost(t, base+"/zones/"+cfZoneID+"/firewall/rules", bearer, batch)
	check("fw batch", body, status, 200)
	var fwResp map[string]any
	json.Unmarshal([]byte(body), &fwResp)
	two, _ := fwResp["result"].([]any)
	if len(two) != 2 {
		t.Fatalf("fw batch: want 2 results, got %s", body)
	}
	fwID := two[0].(map[string]any)["id"].(string)

	// Firewall PUT (full).
	put, _ := json.Marshal(map[string]any{"action": "skip", "description": "skip logging", "filter": map[string]string{"expression": "(ip.src eq 10.0.0.3)"}})
	body, status = cfPut(t, base+"/zones/"+cfZoneID+"/firewall/rules/"+fwID, bearer, put)
	check("fw put", body, status, 200)

	// Firewall PUT without action -> 400.
	putBad, _ := json.Marshal(map[string]any{"filter": map[string]string{"expression": "(ip.src eq 10.0.0.3)"}})
	body, status = cfPut(t, base+"/zones/"+cfZoneID+"/firewall/rules/"+fwID, bearer, putBad)
	check("fw put bad", body, status, 400)

	// Page rule PUT + bad status.
	body, status = cfGet(t, base+"/zones/"+cfZoneID+"/page_rules", bearer)
	check("pr list", body, status, 200)
	var prResp map[string]any
	json.Unmarshal([]byte(body), &prResp)
	seed := prResp["result"].([]any)[0].(map[string]any)
	prID := seed["id"].(string)
	prPut, _ := json.Marshal(map[string]any{
		"targets": []any{map[string]any{"target": "url", "constraint": map[string]any{"operator": "matches", "value": "x/*"}}},
		"actions": []any{map[string]any{"id": "browser_cache_ttl", "value": 60}},
	})
	body, status = cfPut(t, base+"/zones/"+cfZoneID+"/page_rules/"+prID, bearer, prPut)
	check("pr put", body, status, 200)
	prBad, _ := json.Marshal(map[string]any{"status": "bogus"})
	body, status = cfPatch(t, base+"/zones/"+cfZoneID+"/page_rules/"+prID, bearer, prBad)
	check("pr patch bad status", body, status, 400)

	// DNS PUT missing name -> 400; PATCH keeps others.
	body, status = cfGet(t, base+"/zones/"+cfZoneID+"/dns_records", bearer)
	check("dns list", body, status, 200)
	var dnResp map[string]any
	json.Unmarshal([]byte(body), &dnResp)
	rec := dnResp["result"].([]any)[0].(map[string]any)
	recID := rec["id"].(string)
	putNoName, _ := json.Marshal(map[string]any{"type": "A", "content": "192.0.2.7"})
	body, status = cfPut(t, base+"/zones/"+cfZoneID+"/dns_records/"+recID, bearer, putNoName)
	check("dns put missing name", body, status, 400)
	patchOnly, _ := json.Marshal(map[string]any{"content": "192.0.2.8"})
	body, status = cfPatch(t, base+"/zones/"+cfZoneID+"/dns_records/"+recID, bearer, patchOnly)
	check("dns patch", body, status, 200)
	if !strings.Contains(body, "192.0.2.8") {
		t.Fatalf("dns patch: content not applied; %s", body)
	}

	// Zone delete cascades nothing else; also covers DELETE /zones/{id}.
	zb, _ := json.Marshal(map[string]string{"name": "scratch.dev"})
	body, status = cfPost(t, base+"/zones", bearer, zb)
	check("zone create", body, status, 200)
	var zr map[string]any
	json.Unmarshal([]byte(body), &zr)
	zid := zr["result"].(map[string]any)["id"].(string)
	body, status = cfDelete(t, base+"/zones/"+zid, bearer)
	check("zone delete", body, status, 200)

	// D1: multi-statement, INSERT without column list, DELETE without WHERE,
	// INSERT with a missing param, unknown column.
	dbb, _ := json.Marshal(map[string]string{"name": "scratch-db"})
	body, status = cfPost(t, base+"/accounts/"+accountID+"/d1/database", bearer, dbb)
	check("db create", body, status, 200)
	var dc map[string]any
	json.Unmarshal([]byte(body), &dc)
	dbUUID := dc["result"].(map[string]any)["uuid"].(string)
	q := base + "/accounts/" + accountID + "/d1/database/" + dbUUID + "/query"

	exec := func(sql string, params []any) (string, int) {
		b, _ := json.Marshal(map[string]any{"sql": sql, "params": params})
		return cfPost(t, q, bearer, b)
	}

	multi := "CREATE TABLE t (a INTEGER, b TEXT); INSERT INTO t VALUES (?, ?); INSERT INTO t VALUES (?, ?)"
	body, status = exec(multi, []any{1, "x", 2, "y"})
	check("d1 multi", body, status, 200)
	if !strings.Contains(body, "\"changes\":1") {
		t.Fatalf("d1 multi: missing changes; %s", body)
	}
	if !strings.Contains(body, "\"changes\":0") {
		t.Fatalf("d1 multi: missing create-table changes 0; %s", body)
	}

	body, status = exec("SELECT b FROM t WHERE a >= 1", nil)
	check("d1 select", body, status, 200)
	if !strings.Contains(body, "\"x\"") || !strings.Contains(body, "\"y\"") {
		t.Fatalf("d1 select: rows missing; %s", body)
	}

	body, status = exec("DELETE FROM t", nil)
	check("d1 delete all", body, status, 200)
	if !strings.Contains(body, "\"changes\":2") {
		t.Fatalf("d1 delete all: want 2 changes; %s", body)
	}

	body, status = exec("SELECT a FROM t WHERE a = ?", []any{})
	check("d1 params underflow", body, status, 400)

	body, status = exec("SELECT nope FROM t", nil)
	check("d1 unknown column", body, status, 400)

	body, status = exec("SELECT * FROM t LIMIT 1 OFFSET 1", nil)
	check("d1 limit offset empty", body, status, 200)

	// Database delete cascades tables.
	body, status = cfDelete(t, base+"/accounts/"+accountID+"/d1/database/"+dbUUID, bearer)
	check("db delete", body, status, 200)

	// Worker raw (non-JSON, non-multipart) body.
	body, status = cfPutRaw(t, base+"/accounts/"+accountID+"/workers/scripts/raw-worker", bearer, "text/plain", []byte("addEventListener('fetch', () => {})"))
	check("worker raw", body, status, 200)
	body, status = cfGet(t, base+"/accounts/"+accountID+"/workers/scripts/raw-worker", bearer)
	check("worker raw get", body, status, 200)

	// Malformed multipart -> 400 not 500.
	body, status = cfPutRaw(t, base+"/accounts/"+accountID+"/workers/scripts/bad-mp", bearer, "multipart/form-data; boundary=nomatch", []byte("garbage"))
	check("worker bad multipart", body, status, 400)
}

func cfPutRaw(t *testing.T, rawurl, auth, contentType string, body []byte) (string, int) {
	t.Helper()
	req, err := http.NewRequest("PUT", rawurl, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// TestCloudflareStyleLiveTimestamps verifies created_on/modified_on are
// live clock timestamps (RFC 3339) rather than a fixed synthetic date.
func TestCloudflareStyleLiveTimestamps(t *testing.T) {
	base := cfServe(t)

	start := time.Now().UTC()
	body, status := cfPost(t, base+"/zones", "Bearer stunt-api-token-123", []byte(`{"name":"clock-test.example","account":{"id":"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"},"type":"full"}`))
	if status != 200 {
		t.Fatalf("create zone -> %d, want 200; body %s", status, body)
	}
	zone, ok := cfResult(t, body).(map[string]any)
	if !ok {
		t.Fatalf("zone result not an object: %s", body)
	}
	for _, field := range []string{"created_on", "modified_on"} {
		raw, _ := zone[field].(string)
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t.Fatalf("zone %s = %q is not RFC 3339: %v", field, raw, err)
		}
		if ts.Before(start.Add(-time.Minute)) || ts.After(time.Now().Add(time.Minute)) {
			t.Fatalf("zone %s = %v not live (start %v)", field, ts, start)
		}
	}
}
