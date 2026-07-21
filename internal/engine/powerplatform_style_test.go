package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestPowerPlatformStyleAdapter exercises the powerplatform-style adapter:
//
//   - 401 without auth
//   - List environments → OData {value}
//   - Dataverse accounts → OData {value} with accountid
//   - Connectors
//   - List flows + create flow (STATEFUL)
func TestPowerPlatformStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "powerplatform-style")
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
			"pp": {Adapter: absAdapterDir},
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

	base := addrs["pp"]
	token := "mock-entra-token"

	// ===== 401 without auth =====

	_, status := ppPlatformGet(t, base+"/v2/environments", "")
	if status != 401 {
		t.Fatalf("environments without auth -> status %d, want 401", status)
	}

	// ===== List environments =====

	body, status := ppPlatformGet(t, base+"/v2/environments", token)
	if status != 200 {
		t.Fatalf("environments -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	value, ok := resp["value"].([]any)
	if !ok || len(value) < 1 {
		t.Fatalf("value = %v, want non-empty array", resp["value"])
	}
	env := value[0].(map[string]any)
	envName, ok := env["name"].(string)
	if !ok || envName == "" {
		t.Fatalf("env name = %v, want non-empty string", env["name"])
	}
	props, ok := env["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %v, want object", env["properties"])
	}
	if _, ok := props["displayName"].(string); !ok {
		t.Fatalf("displayName = %v, want string", props["displayName"])
	}
	if _, ok := props["environmentSku"].(string); !ok {
		t.Fatalf("environmentSku = %v, want string", props["environmentSku"])
	}

	// ===== Dataverse accounts =====

	body, status = ppPlatformGet(t, base+"/v2/environments/"+envName+"/api/data/v9.2/accounts", token)
	if status != 200 {
		t.Fatalf("accounts -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal accounts: %v (body %s)", err, body)
	}
	value = resp["value"].([]any)
	if len(value) < 1 {
		t.Fatalf("accounts count = %d, want >= 1", len(value))
	}
	account := value[0].(map[string]any)
	if _, ok := account["accountid"].(string); !ok {
		t.Fatalf("accountid = %v, want string", account["accountid"])
	}
	if _, ok := account["name"].(string); !ok {
		t.Fatalf("name = %v, want string", account["name"])
	}
	if _, ok := account["_primarycontactid_value"].(string); !ok {
		t.Fatalf("_primarycontactid_value = %v, want string", account["_primarycontactid_value"])
	}

	// ===== Connectors =====

	body, status = ppPlatformGet(t, base+"/v2/environments/"+envName+"/connectors", token)
	if status != 200 {
		t.Fatalf("connectors -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal connectors: %v (body %s)", err, body)
	}
	value = resp["value"].([]any)
	if len(value) < 1 {
		t.Fatalf("connectors count = %d, want >= 1", len(value))
	}

	// ===== List flows =====

	body, status = ppPlatformGet(t, base+"/v2/environments/"+envName+"/flows", token)
	if status != 200 {
		t.Fatalf("flows -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal flows: %v (body %s)", err, body)
	}
	value = resp["value"].([]any)
	if len(value) < 1 {
		t.Fatalf("flows count = %d, want >= 1", len(value))
	}

	// ===== Create flow (STATEFUL) =====

	body, status = ppPlatformPost(t, base+"/v2/environments/"+envName+"/flows", token, map[string]any{
		"properties": map[string]any{
			"displayName": "My New Flow",
		},
	})
	if status != 201 {
		t.Fatalf("create flow -> status %d, want 201; body %s", status, body)
	}

	// Verify it appears in list.
	body, status = ppPlatformGet(t, base+"/v2/environments/"+envName+"/flows", token)
	if status != 200 {
		t.Fatalf("flows after create -> status %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	value = resp["value"].([]any)
	found := false
	for _, v := range value {
		f := v.(map[string]any)
		if p, ok := f["properties"].(map[string]any); ok {
			if p["displayName"] == "My New Flow" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("created flow not found in list")
	}
}

// === Power Platform test helpers ===

func ppPlatformGet(t *testing.T, rawurl, token string) (string, int) {
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

func ppPlatformPost(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
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
