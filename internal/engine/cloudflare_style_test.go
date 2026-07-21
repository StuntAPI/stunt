package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
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

	// Query the database
	queryBody, _ := json.Marshal(map[string]string{"sql": "SELECT * FROM users"})
	body, status = cfPost(t, base+"/accounts/"+accountID+"/d1/database/"+dbUUID+"/query", bearer, queryBody)
	if status != 200 {
		t.Fatalf("query database -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "\"success\":true") {
		t.Fatalf("query: missing success; body %s", body)
	}
	if !strings.Contains(body, "alice@stunt.dev") {
		t.Fatalf("query: missing seeded row data; body %s", body)
	}
	if !strings.Contains(body, "\"changes\":0") {
		t.Fatalf("query: missing meta.changes; body %s", body)
	}

	// INSERT query returns changes
	insertBody, _ := json.Marshal(map[string]string{"sql": "INSERT INTO users (email) VALUES ('new@stunt.dev')"})
	body, status = cfPost(t, base+"/accounts/"+accountID+"/d1/database/"+dbUUID+"/query", bearer, insertBody)
	if !strings.Contains(body, "\"changes\":1") {
		t.Fatalf("insert query: missing changes:1; body %s", body)
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
