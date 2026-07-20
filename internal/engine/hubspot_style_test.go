package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestHubspotStyleAdapter exercises the HubSpot-style adapter end-to-end:
//
//   - Bearer auth → list contacts (cursor pagination)
//   - create contact
//   - PATCH contact
//   - GET contact by id
//   - associate contact to company
//   - list companies
//   - batch read contacts
//   - 401 without auth → HubSpot error envelope
func TestHubspotStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "hubspot-style")
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
			"hubspot": {Adapter: absAdapterDir},
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

	base := addrs["hubspot"]

	const token = "pat-mock-token"

	// ===== list contacts (cursor pagination) =====

	body, status := hsAuthGet(t, base+"/crm/v3/objects/contacts", token)
	if status != 200 {
		t.Fatalf("list contacts -> %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list resp: %v (body %s)", err, body)
	}
	results, ok := listResp["results"].([]any)
	if !ok || len(results) == 0 {
		t.Fatalf("results = %v, want non-empty array", listResp["results"])
	}
	// Verify the contact shape.
	contact0 := results[0].(map[string]any)
	if _, ok := contact0["id"].(string); !ok {
		t.Fatalf("contact id = %v, want string", contact0["id"])
	}
	props, ok := contact0["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %v, want object", contact0["properties"])
	}
	if _, ok := props["firstname"].(string); !ok {
		t.Fatalf("firstname = %v, want string", props["firstname"])
	}
	// Should have paging field (present, may be null at end of results).
	if _, ok := listResp["paging"]; !ok {
		t.Fatalf("paging field missing from response: %v", listResp)
	}

	// ===== create contact =====

	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/contacts", token, map[string]any{
		"properties": map[string]any{
			"firstname": "John",
			"lastname":  "Smith",
			"email":     "john.smith@example.com",
		},
	})
	if status != 201 {
		t.Fatalf("create contact -> %d, want 201; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal create resp: %v (body %s)", err, body)
	}
	contactID, ok := createResp["id"].(string)
	if !ok || contactID == "" {
		t.Fatalf("id = %v, want non-empty string", createResp["id"])
	}
	contactProps, ok := createResp["properties"].(map[string]any)
	if !ok {
		t.Fatalf("created contact properties = %v, want object", createResp["properties"])
	}
	if contactProps["firstname"] != "John" {
		t.Fatalf("firstname = %v, want 'John'", contactProps["firstname"])
	}
	if _, ok := createResp["createdAt"].(string); !ok {
		t.Fatalf("createdAt = %v, want string", createResp["createdAt"])
	}

	// ===== PATCH contact =====

	body, status = hsAuthPatchJSON(t, base+"/crm/v3/objects/contacts/"+contactID, token, map[string]any{
		"properties": map[string]any{
			"firstname": "Johnny",
			"lastname":  "Smith",
		},
	})
	if status != 200 {
		t.Fatalf("patch contact -> %d, want 200; body %s", status, body)
	}
	var patchResp map[string]any
	if err := json.Unmarshal([]byte(body), &patchResp); err != nil {
		t.Fatalf("unmarshal patch resp: %v (body %s)", err, body)
	}
	patchProps := patchResp["properties"].(map[string]any)
	if patchProps["firstname"] != "Johnny" {
		t.Fatalf("patched firstname = %v, want 'Johnny'", patchProps["firstname"])
	}

	// ===== GET contact by id =====

	body, status = hsAuthGet(t, base+"/crm/v3/objects/contacts/"+contactID, token)
	if status != 200 {
		t.Fatalf("get contact -> %d, want 200; body %s", status, body)
	}
	var getResp map[string]any
	if err := json.Unmarshal([]byte(body), &getResp); err != nil {
		t.Fatalf("unmarshal get resp: %v (body %s)", err, body)
	}
	if getResp["id"] != contactID {
		t.Fatalf("retrieved id = %v, want %s", getResp["id"], contactID)
	}

	// ===== list companies =====

	body, status = hsAuthGet(t, base+"/crm/v3/objects/companies", token)
	if status != 200 {
		t.Fatalf("list companies -> %d, want 200; body %s", status, body)
	}
	var companyResp map[string]any
	if err := json.Unmarshal([]byte(body), &companyResp); err != nil {
		t.Fatalf("unmarshal companies: %v (body %s)", err, body)
	}
	companyResults, ok := companyResp["results"].([]any)
	if !ok || len(companyResults) == 0 {
		t.Fatalf("companies results = %v, want non-empty", companyResp["results"])
	}
	company0 := companyResults[0].(map[string]any)
	companyID, _ := company0["id"].(string)
	if companyID == "" {
		t.Fatalf("company id = %v, want non-empty", company0["id"])
	}

	// ===== associate contact to company =====

	body, status = hsAuthPut(t, base+"/crm/v3/objects/contacts/"+contactID+
		"/associations/company/"+companyID+"/contact_to_company", token)
	if status != 200 {
		t.Fatalf("associate -> %d, want 200; body %s", status, body)
	}

	// Verify association stored.
	body, status = hsAuthGet(t, base+"/crm/v3/objects/contacts/"+contactID+
		"/associations/company", token)
	if status != 200 {
		t.Fatalf("get associations -> %d, want 200; body %s", status, body)
	}

	// ===== batch read contacts =====

	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/contacts/batch/read", token, map[string]any{
		"properties": []string{"firstname", "lastname", "email"},
		"inputs": []map[string]any{
			{"id": contactID},
		},
	})
	if status != 200 {
		t.Fatalf("batch read -> %d, want 200; body %s", status, body)
	}
	var batchResp map[string]any
	if err := json.Unmarshal([]byte(body), &batchResp); err != nil {
		t.Fatalf("unmarshal batch resp: %v (body %s)", err, body)
	}
	batchResults, ok := batchResp["results"].([]any)
	if !ok || len(batchResults) != 1 {
		t.Fatalf("batch results = %v, want exactly 1", batchResp["results"])
	}
	batchRec := batchResults[0].(map[string]any)
	if batchRec["id"] != contactID {
		t.Fatalf("batch record id = %v, want %s", batchRec["id"], contactID)
	}

	// ===== DELETE contact =====

	_, status = hsAuthDelete(t, base+"/crm/v3/objects/contacts/"+contactID, token)
	if status != 204 {
		t.Fatalf("delete contact -> %d, want 204", status)
	}
	// Verify deletion.
	_, status = hsAuthGet(t, base+"/crm/v3/objects/contacts/"+contactID, token)
	if status != 404 {
		t.Fatalf("get after delete -> %d, want 404", status)
	}

	// ===== 401 without auth → HubSpot error envelope =====

	body, status = hsNoAuthGet(t, base+"/crm/v3/objects/contacts")
	if status != 401 {
		t.Fatalf("no-auth contacts -> %d, want 401; body %s", status, body)
	}
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal error resp: %v (body %s)", err, body)
	}
	if _, ok := errResp["message"].(string); !ok {
		t.Fatalf("error message = %v, want string", errResp["message"])
	}
	if _, ok := errResp["status"].(string); !ok {
		t.Fatalf("error status = %v, want string", errResp["status"])
	}
	if _, ok := errResp["category"].(string); !ok {
		t.Fatalf("error category = %v, want string", errResp["category"])
	}

	// ===== hapikey query param also works =====

	body, status = hsNoAuthGet(t, base+"/crm/v3/objects/contacts?hapikey=mock-hapikey")
	if status != 200 {
		t.Fatalf("hapikey auth -> %d, want 200; body %s", status, body)
	}
}

// === HubSpot test helpers ===

func hsAuthGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func hsNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func hsAuthPostJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func hsAuthPatchJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("PATCH", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func hsAuthPut(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("PUT", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func hsAuthDelete(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// Suppress unused import if no url is used directly.
var _ = url.QueryEscape
var _ = strings.Contains
