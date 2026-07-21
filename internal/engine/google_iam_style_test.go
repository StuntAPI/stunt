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

	"stuntapi.com/stunt/internal/manifest"
)

// TestGoogleIAMStyleAdapter exercises the google-iam-style adapter:
//
//   - JWT-bearer token exchange → access_token
//   - list service accounts (seeded)
//   - create service account → appears in listing
//   - list service-account keys
//   - generateAccessToken for a service account
//   - queryGrantableRoles
//   - 401 without bearer on protected endpoints
func TestGoogleIAMStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "google-iam-style")
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
			"iam": {Adapter: absAdapterDir},
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

	base := addrs["iam"]

	// ===== JWT-bearer token exchange =====

	// Create a synthetic JWT assertion (header.payload.sig).
	assertion := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJtb2NrLWRlZmF1bHRAbW9jay1wcm9qZWN0LmlhbS5nc2VydmljZWFjY291bnQuY29tIiwic2NvcGUiOiJodHRwczovL3d3dy5nb29nbGVhcGlzLmNvbS9hdXRoL2Nsb3VkLXBsYXRmb3JtIn0.mock-signature"

	body, status := iamPostForm(t, base+"/oauth2/v4/token", url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	})
	if status != 200 {
		t.Fatalf("jwt exchange -> status %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token response: %v (body %s)", err, body)
	}
	token, ok := tokenResp["access_token"].(string)
	if !ok || token == "" {
		t.Fatalf("access_token = %v, want non-empty string", tokenResp["access_token"])
	}
	if tokenResp["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", tokenResp["token_type"])
	}
	if tokenResp["expires_in"] != float64(3600) {
		t.Fatalf("expires_in = %v, want 3600", tokenResp["expires_in"])
	}

	// ===== List service accounts (seeded) =====

	body, status = iamGetAuth(t, base+"/v1/projects/mock-project/serviceAccounts", token)
	if status != 200 {
		t.Fatalf("list SA -> status %d, want 200; body %s", status, body)
	}
	var saResp map[string]any
	if err := json.Unmarshal([]byte(body), &saResp); err != nil {
		t.Fatalf("unmarshal SA list: %v (body %s)", err, body)
	}
	accounts, ok := saResp["accounts"].([]any)
	if !ok || len(accounts) == 0 {
		t.Fatalf("accounts = %v, want non-empty list", saResp["accounts"])
	}
	firstSA := accounts[0].(map[string]any)
	if _, ok := firstSA["email"].(string); !ok {
		t.Fatalf("email = %v, want string", firstSA["email"])
	}
	if _, ok := firstSA["uniqueId"].(string); !ok {
		t.Fatalf("uniqueId = %v, want string", firstSA["uniqueId"])
	}
	if _, ok := firstSA["name"].(string); !ok {
		t.Fatalf("name = %v, want string", firstSA["name"])
	}

	// ===== Create service account =====

	createBody := map[string]any{
		"accountId": "test-sa",
		"serviceAccount": map[string]any{
			"displayName": "Test Service Account",
		},
	}
	body, status = iamPostJSONAuth(t, base+"/v1/projects/mock-project/serviceAccounts", token, createBody)
	if status != 200 {
		t.Fatalf("create SA -> status %d, want 200; body %s", status, body)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("unmarshal created SA: %v (body %s)", err, body)
	}
	createdEmail, ok := created["email"].(string)
	if !ok || !strings.HasSuffix(createdEmail, "@mock-project.iam.gserviceaccount.com") {
		t.Fatalf("created email = %v, want @mock-project.iam.gserviceaccount.com suffix", created["email"])
	}

	// Created SA appears in listing.
	body, status = iamGetAuth(t, base+"/v1/projects/mock-project/serviceAccounts", token)
	json.Unmarshal([]byte(body), &saResp)
	accounts = saResp["accounts"].([]any)
	found := false
	for _, a := range accounts {
		am := a.(map[string]any)
		if am["email"] == createdEmail {
			found = true
		}
	}
	if !found {
		t.Fatalf("created SA %q not found in listing", createdEmail)
	}

	// ===== List service-account keys =====

	body, status = iamGetAuth(t, base+"/v1/projects/mock-project/serviceAccounts/"+createdEmail+"/keys", token)
	if status != 200 {
		t.Fatalf("list keys -> status %d, want 200; body %s", status, body)
	}
	var keysResp map[string]any
	if err := json.Unmarshal([]byte(body), &keysResp); err != nil {
		t.Fatalf("unmarshal keys: %v (body %s)", err, body)
	}
	keys, ok := keysResp["keys"].([]any)
	if !ok || len(keys) == 0 {
		t.Fatalf("keys = %v, want non-empty list", keysResp["keys"])
	}

	// ===== generateAccessToken =====

	genBody := map[string]any{
		"scope": []string{"https://www.googleapis.com/auth/cloud-platform"},
	}
	body, status = iamPostJSONAuth(t, base+"/v1/projects/mock-project/serviceAccounts/"+createdEmail+":generateAccessToken", token, genBody)
	if status != 200 {
		t.Fatalf("generateAccessToken -> status %d, want 200; body %s", status, body)
	}
	var genResp map[string]any
	if err := json.Unmarshal([]byte(body), &genResp); err != nil {
		t.Fatalf("unmarshal generateAccessToken: %v (body %s)", err, body)
	}
	if _, ok := genResp["accessToken"].(string); !ok {
		t.Fatalf("accessToken = %v, want string", genResp["accessToken"])
	}

	// ===== queryGrantableRoles =====

	queryBody := map[string]any{
		"fullResourceName": "//cloudresourcemanager.googleapis.com/projects/mock-project",
	}
	body, status = iamPostJSONAuth(t, base+"/v1/projects/mock-project/roles:queryGrantableRoles", token, queryBody)
	if status != 200 {
		t.Fatalf("queryGrantableRoles -> status %d, want 200; body %s", status, body)
	}
	var rolesResp map[string]any
	if err := json.Unmarshal([]byte(body), &rolesResp); err != nil {
		t.Fatalf("unmarshal roles: %v (body %s)", err, body)
	}
	roles, ok := rolesResp["roles"].([]any)
	if !ok || len(roles) == 0 {
		t.Fatalf("roles = %v, want non-empty list", rolesResp["roles"])
	}

	// ===== 401 without bearer on protected endpoint =====

	body, status = iamGetAuth(t, base+"/v1/projects/mock-project/serviceAccounts", "")
	if status != 401 {
		t.Fatalf("list SA without token -> status %d, want 401; body %s", status, body)
	}
}

// === Helpers ===

func iamGetAuth(t *testing.T, url, token string) (string, int) {
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

func iamPostJSONAuth(t *testing.T, url, token string, body map[string]any) (string, int) {
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

func iamPostForm(t *testing.T, url string, form url.Values) (string, int) {
	t.Helper()
	resp, err := http.PostForm(url, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
