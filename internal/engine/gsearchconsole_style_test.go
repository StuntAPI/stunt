package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestGSearchConsoleStyleAdapter exercises the gsearchconsole-style adapter:
//
//   - 401 without auth
//   - Search analytics query → rows with keys, clicks, impressions, ctr, position
//   - List sites → siteEntry
//   - Sitemaps
//   - URL inspect
func TestGSearchConsoleStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "gsearchconsole-style")
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
			"gsc": {Adapter: absAdapterDir},
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

	base := addrs["gsc"]
	token := "mock-oauth2-token"
	siteURL := "sc-domain:example.com"

	// ===== 401 without auth =====

	_, status := gscGet(t, base+"/webmasters/v3/sites", "")
	if status != 401 {
		t.Fatalf("sites without auth -> status %d, want 401", status)
	}

	// ===== Search analytics query → rows =====

	body, status := gscPost(t, base+"/webmasters/v3/sites/"+urlEncode(siteURL)+"/searchAnalytics/query", token, map[string]any{
		"startDate":  "2024-01-01",
		"endDate":    "2024-01-31",
		"dimensions": []string{"query"},
		"rowLimit":   3,
	})
	if status != 200 {
		t.Fatalf("query -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	rows, ok := resp["rows"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("rows = %v, want non-empty array", resp["rows"])
	}
	row := rows[0].(map[string]any)
	keys, ok := row["keys"].([]any)
	if !ok || len(keys) != 1 {
		t.Fatalf("keys = %v, want array of 1", row["keys"])
	}
	if _, ok := keys[0].(string); !ok {
		t.Fatalf("keys[0] = %v, want string", keys[0])
	}
	if _, ok := row["clicks"].(float64); !ok {
		t.Fatalf("clicks = %v, want number", row["clicks"])
	}
	if _, ok := row["impressions"].(float64); !ok {
		t.Fatalf("impressions = %v, want number", row["impressions"])
	}
	if _, ok := row["ctr"].(float64); !ok {
		t.Fatalf("ctr = %v, want number", row["ctr"])
	}
	if _, ok := row["position"].(float64); !ok {
		t.Fatalf("position = %v, want number", row["position"])
	}

	// ===== List sites =====

	body, status = gscGet(t, base+"/webmasters/v3/sites", token)
	if status != 200 {
		t.Fatalf("sites -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal sites: %v (body %s)", err, body)
	}
	siteEntry, ok := resp["siteEntry"].([]any)
	if !ok || len(siteEntry) < 1 {
		t.Fatalf("siteEntry = %v, want non-empty array", resp["siteEntry"])
	}
	first := siteEntry[0].(map[string]any)
	if _, ok := first["siteUrl"].(string); !ok {
		t.Fatalf("siteUrl = %v, want string", first["siteUrl"])
	}
	if _, ok := first["permissionLevel"].(string); !ok {
		t.Fatalf("permissionLevel = %v, want string", first["permissionLevel"])
	}

	// ===== Sitemaps =====

	body, status = gscGet(t, base+"/webmasters/v3/sites/"+urlEncode(siteURL)+"/sitemaps", token)
	if status != 200 {
		t.Fatalf("sitemaps -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal sitemaps: %v (body %s)", err, body)
	}
	sitemaps, ok := resp["sitemap"].([]any)
	if !ok || len(sitemaps) < 1 {
		t.Fatalf("sitemap = %v, want non-empty array", resp["sitemap"])
	}

	// ===== URL inspect =====

	body, status = gscPost(t, base+"/webmasters/v3/sites/"+urlEncode(siteURL)+"/inspect", token, map[string]any{
		"inspectionUrl": "https://www.example.com/page",
	})
	if status != 200 {
		t.Fatalf("inspect -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal inspect: %v (body %s)", err, body)
	}
	ir, ok := resp["inspectionResult"].(map[string]any)
	if !ok {
		t.Fatalf("inspectionResult = %v, want object", resp["inspectionResult"])
	}
	idxStatus, ok := ir["indexStatusResult"].(map[string]any)
	if !ok {
		t.Fatalf("indexStatusResult = %v, want object", ir["indexStatusResult"])
	}
	if idxStatus["verdict"] != "PASS" {
		t.Fatalf("verdict = %v, want PASS", idxStatus["verdict"])
	}
}

// urlEncode returns a query-safe URL encoding for embedding siteUrl in a path.
func urlEncode(s string) string {
	return url.QueryEscape(s)
}

// === GSC test helpers ===

func gscGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func gscPost(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
