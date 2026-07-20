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

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestGA4StyleAdapter exercises the ga4-style adapter end-to-end:
//
//   - Admin: list accounts → properties → dataStreams hierarchy
//   - Data: runReport → dimensionHeaders, metricHeaders, rows with values
//   - Data: runRealtimeReport → rows
//   - 401 without bearer token
func TestGA4StyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "ga4-style")
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
			"ga4": {Adapter: absAdapterDir},
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

	base := addrs["ga4"]
	const token = "ya29.mock-token"

	// ===== Admin: accounts → properties → dataStreams =====

	body, status := ga4GetAuth(t, base+"/v1admin/accounts", token)
	if status != 200 {
		t.Fatalf("accounts -> status %d, want 200; body %s", status, body)
	}
	var accountsResp map[string]any
	if err := json.Unmarshal([]byte(body), &accountsResp); err != nil {
		t.Fatalf("unmarshal accounts: %v (body %s)", err, body)
	}
	accounts, ok := accountsResp["accounts"].([]any)
	if !ok || len(accounts) == 0 {
		t.Fatalf("accounts = %v, want non-empty list", accountsResp["accounts"])
	}

	body, status = ga4GetAuth(t, base+"/v1admin/properties", token)
	if status != 200 {
		t.Fatalf("properties -> status %d, want 200; body %s", status, body)
	}
	var propsResp map[string]any
	if err := json.Unmarshal([]byte(body), &propsResp); err != nil {
		t.Fatalf("unmarshal properties: %v (body %s)", err, body)
	}
	properties, ok := propsResp["properties"].([]any)
	if !ok || len(properties) == 0 {
		t.Fatalf("properties = %v, want non-empty list", propsResp["properties"])
	}
	propName := properties[0].(map[string]any)["name"].(string)
	if !strings.HasPrefix(propName, "properties/") {
		t.Fatalf("property name = %v, want properties/* prefix", propName)
	}

	body, status = ga4GetAuth(t, base+"/v1admin/properties/123456789/dataStreams", token)
	if status != 200 {
		t.Fatalf("dataStreams -> status %d, want 200; body %s", status, body)
	}
	var dsResp map[string]any
	if err := json.Unmarshal([]byte(body), &dsResp); err != nil {
		t.Fatalf("unmarshal dataStreams: %v (body %s)", err, body)
	}
	streams, ok := dsResp["dataStreams"].([]any)
	if !ok || len(streams) == 0 {
		t.Fatalf("dataStreams = %v, want non-empty list", dsResp["dataStreams"])
	}

	// ===== Data: runReport → dimensionHeaders + metricHeaders + rows =====

	reportBody := map[string]any{
		"dateRanges": []map[string]any{
			{"startDate": "2024-01-01", "endDate": "2024-01-07"},
		},
		"dimensions": []map[string]any{
			{"name": "date"},
		},
		"metrics": []map[string]any{
			{"name": "sessions"},
			{"name": "activeUsers"},
			{"name": "screenPageViews"},
		},
		"limit": 10,
	}

	body, status = ga4PostJSONAuth(t, base+"/v1beta/properties/123456789:runReport", token, reportBody)
	if status != 200 {
		t.Fatalf("runReport -> status %d, want 200; body %s", status, body)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("unmarshal runReport: %v (body %s)", err, body)
	}

	// dimensionHeaders
	dimHeaders, ok := report["dimensionHeaders"].([]any)
	if !ok || len(dimHeaders) != 1 {
		t.Fatalf("dimensionHeaders = %v, want list of 1", report["dimensionHeaders"])
	}
	if dimHeaders[0].(map[string]any)["name"] != "date" {
		t.Fatalf("dimensionHeaders[0].name = %v, want 'date'", dimHeaders[0].(map[string]any)["name"])
	}

	// metricHeaders
	metricHeaders, ok := report["metricHeaders"].([]any)
	if !ok || len(metricHeaders) != 3 {
		t.Fatalf("metricHeaders = %v, want list of 3", report["metricHeaders"])
	}

	// rows
	rows, ok := report["rows"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("rows = %v, want non-empty list", report["rows"])
	}
	firstRow := rows[0].(map[string]any)
	dimValues, ok := firstRow["dimensionValues"].([]any)
	if !ok || len(dimValues) != 1 {
		t.Fatalf("dimensionValues = %v, want list of 1", firstRow["dimensionValues"])
	}
	metricValues, ok := firstRow["metricValues"].([]any)
	if !ok || len(metricValues) != 3 {
		t.Fatalf("metricValues = %v, want list of 3", firstRow["metricValues"])
	}

	// rowCount
	rowCount, ok := report["rowCount"].(float64)
	if !ok || rowCount < 1 {
		t.Fatalf("rowCount = %v, want >= 1", report["rowCount"])
	}

	// metadata
	if _, ok := report["metadata"].(map[string]any); !ok {
		t.Fatalf("metadata = %v, want map", report["metadata"])
	}

	// ===== Data: runRealtimeReport =====

	realtimeBody := map[string]any{
		"dimensions": []map[string]any{{"name": "country"}},
		"metrics":    []map[string]any{{"name": "activeUsers"}},
		"limit":      10,
	}
	body, status = ga4PostJSONAuth(t, base+"/v1beta/properties/123456789:runRealtimeReport", token, realtimeBody)
	if status != 200 {
		t.Fatalf("runRealtimeReport -> status %d, want 200; body %s", status, body)
	}

	// ===== 401 without bearer =====

	body, status = ga4GetAuth(t, base+"/v1admin/accounts", "")
	if status != 401 {
		t.Fatalf("accounts without token -> status %d, want 401; body %s", status, body)
	}
}

// === Helpers ===

func ga4GetAuth(t *testing.T, url, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
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

func ga4PostJSONAuth(t *testing.T, url, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
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
