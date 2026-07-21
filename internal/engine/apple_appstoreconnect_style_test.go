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

// mintES256JWT creates a structurally valid ES256 JWT for testing.
// The signature is a synthetic placeholder — structural validation only.
func mintES256JWT(t *testing.T) string {
	t.Helper()
	// {"alg":"ES256","kid":"TESTKEY123","typ":"JWT"}
	header := "eyJhbGciOiJFUzI1NiIsImtpZCI6IlRFU1RLRVkxMjMiLCJ0eXAiOiJKV1QifQ"
	// {"iss":"test-issuer","iat":1700000000,"exp":1700003600,"aud":"appstoreconnect-v1"}
	payload := "eyJpc3MiOiJ0ZXN0LWlzc3VlciIsImlhdCI6MTcwMDAwMDAwMCwiZXhwIjoxNzAwMDAzNjAwLCJhdWQiOiJhcHBzdG9yZWNvbm5lY3QtdjEifQ"
	return header + "." + payload + ".c3ludGhldGljLXNpZ25hdHVyZQ"
}

// mintBadAlgJWT creates a JWT with HS256 alg (should be rejected).
func mintBadAlgJWT(t *testing.T) string {
	t.Helper()
	// {"alg":"HS256","typ":"JWT"}
	header := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"
	payload := "eyJpc3MiOiJ0ZXN0LWlzc3VlciJ9"
	return header + "." + payload + ".c3ludGhldGljLXNpZ25hdHVyZQ"
}

func TestAppStoreConnectStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "apple-appstoreconnect-style")
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
			"asc": {Adapter: absAdapterDir},
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

	base := addrs["asc"]
	jwt := mintES256JWT(t)

	// ===== No auth → 401 JSON:API error =====
	body, status := ascGet(t, base+"/v1/apps", "")
	if status != 401 {
		t.Fatalf("GET /v1/apps without auth -> status %d, want 401; body %s", status, body)
	}
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal 401 error: %v (body %s)", err, body)
	}
	errorsRaw, ok := errResp["errors"]
	if !ok {
		t.Fatalf("401 error missing 'errors' key: %v", errResp)
	}
	errors, ok := errorsRaw.([]any)
	if !ok || len(errors) == 0 {
		t.Fatalf("401 'errors' is not a non-empty array: %v", errorsRaw)
	}
	firstErr := errors[0].(map[string]any)
	if firstErr["code"] != "NOT_AUTHORIZED" {
		t.Fatalf("401 error code = %v, want NOT_AUTHORIZED", firstErr["code"])
	}

	// ===== Bad alg (HS256) → 401 =====
	body, status = ascGet(t, base+"/v1/apps", mintBadAlgJWT(t))
	if status != 401 {
		t.Fatalf("GET /v1/apps with HS256 JWT -> status %d, want 401; body %s", status, body)
	}

	// ===== GET /v1/apps with JWT → seeded app in list =====
	body, status = ascGet(t, base+"/v1/apps", jwt)
	if status != 200 {
		t.Fatalf("GET /v1/apps -> status %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal apps list: %v (body %s)", err, body)
	}
	data, ok := listResp["data"].([]any)
	if !ok {
		t.Fatalf("apps list 'data' is not an array: %v", listResp["data"])
	}
	if len(data) == 0 {
		t.Fatal("apps list is empty; expected seeded app")
	}
	firstApp := data[0].(map[string]any)
	if firstApp["type"] != "apps" {
		t.Fatalf("app type = %v, want 'apps'", firstApp["type"])
	}
	attrs := firstApp["attributes"].(map[string]any)
	seededAppID, _ := firstApp["id"].(string)
	if attrs["bundleId"] == "" {
		t.Fatal("seeded app has no bundleId")
	}

	// ===== GET /v1/apps/{id} → single app =====
	body, status = ascGet(t, base+"/v1/apps/"+seededAppID, jwt)
	if status != 200 {
		t.Fatalf("GET /v1/apps/%s -> status %d, want 200; body %s", seededAppID, status, body)
	}
	var appResp map[string]any
	if err := json.Unmarshal([]byte(body), &appResp); err != nil {
		t.Fatalf("unmarshal single app: %v (body %s)", err, body)
	}
	singleData := appResp["data"].(map[string]any)
	if singleData["id"] != seededAppID {
		t.Fatalf("single app id = %v, want %v", singleData["id"], seededAppID)
	}
	if singleData["type"] != "apps" {
		t.Fatalf("single app type = %v, want 'apps'", singleData["type"])
	}

	// ===== POST /v1/apps → create, then appears in list =====
	createBody := map[string]any{
		"data": map[string]any{
			"type": "apps",
			"attributes": map[string]any{
				"name":          "My New App",
				"bundleId":      "com.example.newapp",
				"sku":           "NEW_SKU_42",
				"primaryLocale": "en-US",
			},
		},
	}
	body, status = ascPostJSON(t, base+"/v1/apps", jwt, createBody)
	if status != 201 {
		t.Fatalf("POST /v1/apps -> status %d, want 201; body %s", status, body)
	}
	var createdResp map[string]any
	if err := json.Unmarshal([]byte(body), &createdResp); err != nil {
		t.Fatalf("unmarshal created app: %v (body %s)", err, body)
	}
	createdData := createdResp["data"].(map[string]any)
	newAppID, ok := createdData["id"].(string)
	if !ok || newAppID == "" {
		t.Fatalf("created app id = %v, want non-empty string", createdData["id"])
	}
	newAttrs := createdData["attributes"].(map[string]any)
	if newAttrs["name"] != "My New App" {
		t.Fatalf("created app name = %v, want 'My New App'", newAttrs["name"])
	}
	if newAttrs["bundleId"] != "com.example.newapp" {
		t.Fatalf("created app bundleId = %v, want 'com.example.newapp'", newAttrs["bundleId"])
	}

	// Verify it appears in the list (STATEFUL).
	body, status = ascGet(t, base+"/v1/apps", jwt)
	if status != 200 {
		t.Fatalf("GET /v1/apps (after create) -> status %d, want 200", status)
	}
	json.Unmarshal([]byte(body), &listResp)
	data = listResp["data"].([]any)
	foundNew := false
	for _, a := range data {
		am := a.(map[string]any)
		if am["id"] == newAppID {
			foundNew = true
		}
	}
	if !foundNew {
		t.Fatalf("created app %s not found in list", newAppID)
	}

	// ===== GET /v1/apps/{id}/appStoreVersions =====
	body, status = ascGet(t, base+"/v1/apps/"+seededAppID+"/appStoreVersions", jwt)
	if status != 200 {
		t.Fatalf("GET appStoreVersions -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &listResp)
	versionData := listResp["data"].([]any)
	if len(versionData) == 0 {
		t.Fatal("appStoreVersions list is empty")
	}
	versionObj := versionData[0].(map[string]any)
	if versionObj["type"] != "appStoreVersions" {
		t.Fatalf("version type = %v, want 'appStoreVersions'", versionObj["type"])
	}

	// ===== GET /v1/apps/{id}/builds =====
	body, status = ascGet(t, base+"/v1/apps/"+seededAppID+"/builds", jwt)
	if status != 200 {
		t.Fatalf("GET builds -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &listResp)
	buildData := listResp["data"].([]any)
	if len(buildData) == 0 {
		t.Fatal("builds list is empty")
	}

	// ===== GET /v1/apps/{id}/appPrices =====
	body, status = ascGet(t, base+"/v1/apps/"+seededAppID+"/appPrices", jwt)
	if status != 200 {
		t.Fatalf("GET appPrices -> status %d, want 200; body %s", status, body)
	}

	// ===== GET /v1/users =====
	body, status = ascGet(t, base+"/v1/users", jwt)
	if status != 200 {
		t.Fatalf("GET /v1/users -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &listResp)
	userData := listResp["data"].([]any)
	if len(userData) == 0 {
		t.Fatal("users list is empty")
	}

	// ===== GET /v1/salesReports =====
	body, status = ascGet(t, base+"/v1/salesReports", jwt)
	if status != 200 {
		t.Fatalf("GET /v1/salesReports -> status %d, want 200; body %s", status, body)
	}

	// ===== GET unknown app → 404 =====
	body, status = ascGet(t, base+"/v1/apps/nonexistent", jwt)
	if status != 404 {
		t.Fatalf("GET unknown app -> status %d, want 404; body %s", status, body)
	}
}

// === App Store Connect test helpers ===

func ascGet(t *testing.T, rawurl, jwt string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func ascPostJSON(t *testing.T, rawurl, jwt string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
