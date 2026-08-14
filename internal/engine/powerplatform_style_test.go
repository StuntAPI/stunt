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

func ppPlatformPatch(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PATCH", rawurl, bytes.NewReader(data))
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

func ppPlatformDelete(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
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

// TestPowerPlatformStyleDataverseCRUD exercises the Dataverse write side +
// key-in-parens routing: list ($count, $select), create, retrieve by
// accounts({id}), PATCH, DELETE.
func TestPowerPlatformStyleDataverseCRUD(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "powerplatform-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"pp": {Adapter: adapterDir},
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
	const token = "mock-entra-token"

	// Resolve an environment name (the Dataverse route is env-scoped).
	body, _ := ppPlatformGet(t, base+"/v2/environments", token)
	var envResp map[string]any
	json.Unmarshal([]byte(body), &envResp)
	envs, _ := envResp["value"].([]any)
	if len(envs) == 0 {
		t.Fatal("no environments seeded")
	}
	envName, _ := envs[0].(map[string]any)["name"].(string)
	acctURL := base + "/v2/environments/" + envName + "/api/data/v9.2/accounts"

	// $count + seeded list.
	body, _ = ppPlatformGet(t, acctURL+"?$count=true", token)
	var list map[string]any
	json.Unmarshal([]byte(body), &list)
	if list["@odata.count"] == nil {
		t.Fatal("$count=true did not yield @odata.count")
	}

	// $select projects to only the named field.
	body, _ = ppPlatformGet(t, acctURL+"?$select=name", token)
	json.Unmarshal([]byte(body), &list)
	vals, _ := list["value"].([]any)
	if len(vals) == 0 {
		t.Fatal("no accounts listed")
	}
	first := vals[0].(map[string]any)
	if len(first) != 1 || first["name"] == nil {
		t.Fatalf("$select projected to %v, want only {name}", first)
	}

	// Create.
	body, status := ppPlatformPost(t, acctURL, token, map[string]any{"name": "Globex", "revenue": 1000})
	if status != 201 {
		t.Fatalf("create -> %d; body %s", status, body)
	}
	var created map[string]any
	json.Unmarshal([]byte(body), &created)
	id, _ := created["accountid"].(string)
	if id == "" {
		t.Fatal("create returned no accountid")
	}

	// Retrieve by key-in-parens.
	body, status = ppPlatformGet(t, acctURL+"("+id+")", token)
	if status != 200 {
		t.Fatalf("retrieve -> %d; body %s", status, body)
	}
	var got map[string]any
	json.Unmarshal([]byte(body), &got)
	if got["name"] != "Globex" {
		t.Fatalf("retrieved name = %v, want Globex", got["name"])
	}

	// Update via PATCH.
	if body, status = ppPlatformPatch(t, acctURL+"("+id+")", token, map[string]any{"revenue": 5000}); status != 204 {
		t.Fatalf("update -> %d; body %s", status, body)
	}
	body, _ = ppPlatformGet(t, acctURL+"("+id+")", token)
	json.Unmarshal([]byte(body), &got)
	if got["revenue"].(float64) != 5000 {
		t.Fatalf("revenue after update = %v, want 5000", got["revenue"])
	}

	// Delete, then retrieve -> 404.
	if body, status = ppPlatformDelete(t, acctURL+"("+id+")", token); status != 204 {
		t.Fatalf("delete -> %d; body %s", status, body)
	}
	if _, status = ppPlatformGet(t, acctURL+"("+id+")", token); status != 404 {
		t.Fatalf("retrieve after delete -> %d, want 404", status)
	}
}
