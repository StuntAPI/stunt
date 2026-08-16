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

// mintUnregisteredJWT creates a structurally valid ES256 JWT that is NOT
// registered in the adapter's token registry (should be rejected with 401).
func mintUnregisteredJWT(t *testing.T) string {
	t.Helper()
	// {"alg":"ES256","kid":"TESTKEY123","typ":"JWT"}
	header := "eyJhbGciOiJFUzI1NiIsImtpZCI6IlRFU1RLRVkxMjMiLCJ0eXAiOiJKV1QifQ"
	// {"iss":"other-issuer"}
	payload := "eyJpc3MiOiJvdGhlci1pc3N1ZXIifQ"
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

	// ===== Structurally valid but unregistered JWT → 401 =====
	body, status = ascGet(t, base+"/v1/apps", mintUnregisteredJWT(t))
	if status != 401 {
		t.Fatalf("GET /v1/apps with unregistered JWT -> status %d, want 401; body %s", status, body)
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

func ascPatchJSON(t *testing.T, rawurl, jwt string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PATCH", rawurl, bytes.NewReader(data))
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

// TestAppStoreConnectStyleBuildLifecycle proves the derive-on-read build
// processing state machine: a fresh app's build is PROCESSING immediately
// and settles VALID after the 3s window (INVALID with the simulator-only
// simulate_fail attribute). Also exercises GET /v1/builds/{id}.
func TestAppStoreConnectStyleBuildLifecycle(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "apple-appstoreconnect-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"appstoreconnect": {Adapter: adapterDir},
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
	base := addrs["appstoreconnect"]
	jwt := mintES256JWT(t)

	// Seed the default app (its build lifecycle clock starts now).
	body, status := ascGet(t, base+"/v1/apps", jwt)
	if status != 200 {
		t.Fatalf("GET apps -> %d", status)
	}
	var appsResp map[string]any
	json.Unmarshal([]byte(body), &appsResp)
	appsData := appsResp["data"].([]any)
	if len(appsData) == 0 {
		t.Fatal("apps list is empty; expected seeded app")
	}
	seededAppID := appsData[0].(map[string]any)["id"].(string)

	// Create an app whose build will end INVALID.
	body, status = ascPostJSON(t, base+"/v1/apps", jwt, map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{
				"name":          "Fail App",
				"bundleId":      "com.example.failapp",
				"simulate_fail": true,
			},
		},
	})
	if status != 201 {
		t.Fatalf("POST fail app -> %d; body %s", status, body)
	}
	var createResp map[string]any
	json.Unmarshal([]byte(body), &createResp)
	failAppID := createResp["data"].(map[string]any)["id"].(string)

	ascBuildState := func(appID string) (string, string) {
		t.Helper()
		body, status := ascGet(t, base+"/v1/apps/"+appID+"/builds", jwt)
		if status != 200 {
			t.Fatalf("GET builds for %s -> %d; body %s", appID, status, body)
		}
		var listResp map[string]any
		json.Unmarshal([]byte(body), &listResp)
		data := listResp["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("builds for %s = %d items, want 1", appID, len(data))
		}
		b := data[0].(map[string]any)
		return b["attributes"].(map[string]any)["processingState"].(string), b["id"].(string)
	}

	// Immediately: both builds are PROCESSING.
	if st, _ := ascBuildState(seededAppID); st != "PROCESSING" {
		t.Fatalf("seeded build immediate state = %q, want PROCESSING", st)
	}
	failState, failBuildID := ascBuildState(failAppID)
	if failState != "PROCESSING" {
		t.Fatalf("fail build immediate state = %q, want PROCESSING", failState)
	}

	// After the 3s window: VALID and INVALID.
	time.Sleep(3500 * time.Millisecond)

	if st, _ := ascBuildState(seededAppID); st != "VALID" {
		t.Fatalf("seeded build after window = %q, want VALID", st)
	}
	if st, _ := ascBuildState(failAppID); st != "INVALID" {
		t.Fatalf("fail build after window = %q, want INVALID", st)
	}

	// Single-build endpoint agrees.
	body, status = ascGet(t, base+"/v1/builds/"+failBuildID, jwt)
	if status != 200 {
		t.Fatalf("GET single build -> %d; body %s", status, body)
	}
	var single map[string]any
	json.Unmarshal([]byte(body), &single)
	if single["data"].(map[string]any)["attributes"].(map[string]any)["processingState"] != "INVALID" {
		t.Fatalf("single build state = %v, want INVALID", single["data"])
	}
}

// TestAppStoreConnectStyleVersionLifecycle proves the appStoreVersions
// surface: create (PREPARE_FOR_SUBMISSION), PATCH, submission via the real
// action (POST /v1/appStoreVersionSubmissions), the derive-on-read review
// state machine (WAITING_FOR_REVIEW -> IN_REVIEW -> READY_FOR_SALE /
// REJECTED), builds listed per version, PATCH /v1/apps/{id}, and the
// duplicate-bundleId 409.
func TestAppStoreConnectStyleVersionLifecycle(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "apple-appstoreconnect-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"appstoreconnect": {Adapter: adapterDir},
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
	base := addrs["appstoreconnect"]
	jwt := mintES256JWT(t)

	// Seed the default app, then create a fresh app whose build is not yet
	// attached to any version (the version create attaches it).
	body, status := ascGet(t, base+"/v1/apps", jwt)
	if status != 200 {
		t.Fatalf("GET apps -> %d", status)
	}
	body, status = ascPostJSON(t, base+"/v1/apps", jwt, map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{
				"name":     "Lifecycle App",
				"bundleId": "com.example.lifecycle",
			},
		},
	})
	if status != 201 {
		t.Fatalf("POST lifecycle app -> %d; body %s", status, body)
	}
	var appsResp map[string]any
	json.Unmarshal([]byte(body), &appsResp)
	appID, _ := appsResp["data"].(map[string]any)["id"].(string)
	if appID == "" {
		t.Fatalf("lifecycle app id = %v", appsResp["data"])
	}

	// ===== Create a version (missing versionString -> 409) =====
	body, status = ascPostJSON(t, base+"/v1/apps/"+appID+"/appStoreVersions", jwt, map[string]any{
		"data": map[string]any{
			"type": "appStoreVersions",
			"attributes": map[string]any{
				"platform": "IOS",
			},
		},
	})
	if status != 409 {
		t.Fatalf("POST appStoreVersions without versionString -> %d; body %s", status, body)
	}

	// ===== Create a version -> 201 PREPARE_FOR_SUBMISSION =====
	body, status = ascPostJSON(t, base+"/v1/apps/"+appID+"/appStoreVersions", jwt, map[string]any{
		"data": map[string]any{
			"type": "appStoreVersions",
			"attributes": map[string]any{
				"versionString": "2.0.0",
				"releaseType":   "MANUAL",
			},
		},
	})
	if status != 201 {
		t.Fatalf("POST appStoreVersions -> %d; body %s", status, body)
	}
	var createdVersion map[string]any
	json.Unmarshal([]byte(body), &createdVersion)
	versionData := createdVersion["data"].(map[string]any)
	versionID, _ := versionData["id"].(string)
	if versionID == "" {
		t.Fatalf("created version id = %v", versionData["id"])
	}
	versionAttrs := versionData["attributes"].(map[string]any)
	if versionAttrs["appStoreState"] != "PREPARE_FOR_SUBMISSION" {
		t.Fatalf("new version state = %v, want PREPARE_FOR_SUBMISSION", versionAttrs["appStoreState"])
	}
	if versionAttrs["versionString"] != "2.0.0" {
		t.Fatalf("new version versionString = %v, want 2.0.0", versionAttrs["versionString"])
	}

	// ===== Duplicate versionString for the same app -> 409 =====
	body, status = ascPostJSON(t, base+"/v1/apps/"+appID+"/appStoreVersions", jwt, map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{"versionString": "2.0.0"},
		},
	})
	if status != 409 {
		t.Fatalf("POST duplicate appStoreVersion -> %d, want 409; body %s", status, body)
	}

	// ===== A version whose review will be rejected. =====
	body, status = ascPostJSON(t, base+"/v1/apps/"+appID+"/appStoreVersions", jwt, map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{
				"versionString": "3.0.0",
				"simulate_fail": true,
			},
		},
	})
	if status != 201 {
		t.Fatalf("POST fail version -> %d; body %s", status, body)
	}
	var failVersion map[string]any
	json.Unmarshal([]byte(body), &failVersion)
	failVersionID, _ := failVersion["data"].(map[string]any)["id"].(string)

	// ===== PATCH the editable version (versionString change) =====
	body, status = ascPatchJSON(t, base+"/v1/appStoreVersions/"+versionID, jwt, map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{
				"versionString": "2.0.1",
				"usesIdfa":      true,
			},
		},
	})
	if status != 200 {
		t.Fatalf("PATCH appStoreVersion -> %d; body %s", status, body)
	}
	var patched map[string]any
	json.Unmarshal([]byte(body), &patched)
	patchedAttrs := patched["data"].(map[string]any)["attributes"].(map[string]any)
	if patchedAttrs["versionString"] != "2.0.1" {
		t.Fatalf("patched versionString = %v, want 2.0.1", patchedAttrs["versionString"])
	}

	// ===== GET /v1/appStoreVersions/{id} agrees =====
	body, status = ascGet(t, base+"/v1/appStoreVersions/"+versionID, jwt)
	if status != 200 {
		t.Fatalf("GET appStoreVersion -> %d; body %s", status, body)
	}

	// ===== Builds listed per version: the seeded app's build attached at
	// version create (first version grabs the unattached build). =====
	body, status = ascGet(t, base+"/v1/appStoreVersions/"+versionID+"/builds", jwt)
	if status != 200 {
		t.Fatalf("GET version builds -> %d; body %s", status, body)
	}
	var buildsResp map[string]any
	json.Unmarshal([]byte(body), &buildsResp)
	buildsList, ok := buildsResp["data"].([]any)
	if !ok || len(buildsList) != 1 {
		t.Fatalf("version builds = %v, want 1 build", buildsResp["data"])
	}

	// ===== Submit for review (the real action) =====
	body, status = ascPostJSON(t, base+"/v1/appStoreVersionSubmissions", jwt, map[string]any{
		"data": map[string]any{
			"type": "appStoreVersionSubmissions",
			"relationships": map[string]any{
				"appStoreVersion": map[string]any{
					"data": map[string]any{"type": "appStoreVersions", "id": versionID},
				},
			},
		},
	})
	if status != 201 {
		t.Fatalf("POST appStoreVersionSubmissions -> %d; body %s", status, body)
	}

	// Immediately: WAITING_FOR_REVIEW.
	versionState := func(vid string) string {
		b, st := ascGet(t, base+"/v1/appStoreVersions/"+vid, jwt)
		if st != 200 {
			t.Fatalf("GET appStoreVersion %s -> %d; body %s", vid, st, b)
		}
		var vr map[string]any
		json.Unmarshal([]byte(b), &vr)
		return vr["data"].(map[string]any)["attributes"].(map[string]any)["appStoreState"].(string)
	}
	// The WAITING_FOR_REVIEW window is 1s of wall clock; on a loaded CI
	// runner the read can land past it, so accept any not-yet-terminal
	// review state here (the terminal state is asserted after the full sleep).
	if s := versionState(versionID); s != "WAITING_FOR_REVIEW" && s != "IN_REVIEW" && s != "DEVELOPER_REJECTED" {
		t.Fatalf("submitted version state = %q, want WAITING_FOR_REVIEW (or in-flight)", s)
	}

	// ===== Resubmitting an in-review version -> 409 =====
	body, status = ascPostJSON(t, base+"/v1/appStoreVersionSubmissions", jwt, map[string]any{
		"data": map[string]any{
			"relationships": map[string]any{
				"appStoreVersion": map[string]any{
					"data": map[string]any{"type": "appStoreVersions", "id": versionID},
				},
			},
		},
	})
	if status != 409 {
		t.Fatalf("resubmit -> %d, want 409; body %s", status, body)
	}

	// ===== PATCH while in review -> 409 =====
	body, status = ascPatchJSON(t, base+"/v1/appStoreVersions/"+versionID, jwt, map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{"versionString": "2.0.2"},
		},
	})
	if status != 409 {
		t.Fatalf("PATCH in-review version -> %d, want 409; body %s", status, body)
	}

	// Submit the fail version too.
	body, status = ascPostJSON(t, base+"/v1/appStoreVersionSubmissions", jwt, map[string]any{
		"data": map[string]any{
			"relationships": map[string]any{
				"appStoreVersion": map[string]any{
					"data": map[string]any{"type": "appStoreVersions", "id": failVersionID},
				},
			},
		},
	})
	if status != 201 {
		t.Fatalf("POST fail submission -> %d; body %s", status, body)
	}

	// Past the 3s decision window: READY_FOR_SALE / REJECTED.
	time.Sleep(3500 * time.Millisecond)
	if s := versionState(versionID); s != "READY_FOR_SALE" {
		t.Fatalf("version after window = %q, want READY_FOR_SALE", s)
	}
	if s := versionState(failVersionID); s != "REJECTED" {
		t.Fatalf("fail version after window = %q, want REJECTED", s)
	}

	// The app's version list reflects both the seeded release and the
	// lifecycle above, and the filter param agrees.
	body, status = ascGet(t, base+"/v1/apps/"+appID+"/appStoreVersions?filter[appStoreState]=READY_FOR_SALE", jwt)
	if status != 200 {
		t.Fatalf("GET app versions filtered -> %d", status)
	}
	var versionsResp map[string]any
	json.Unmarshal([]byte(body), &versionsResp)
	filtered, ok := versionsResp["data"].([]any)
	if !ok || len(filtered) != 1 {
		t.Fatalf("READY_FOR_SALE versions = %v, want exactly 1 (3.0.0 is REJECTED)", versionsResp["data"])
	}

	// ===== Unknown version -> 404 =====
	body, status = ascGet(t, base+"/v1/appStoreVersions/nonexistent", jwt)
	if status != 404 {
		t.Fatalf("GET unknown version -> %d, want 404; body %s", status, body)
	}

	// ===== PATCH /v1/apps/{id} renames the app and persists =====
	body, status = ascPatchJSON(t, base+"/v1/apps/"+appID, jwt, map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{
				"name": "Renamed Mock App",
				"sku":  "MOCK_SKU_002",
			},
		},
	})
	if status != 200 {
		t.Fatalf("PATCH app -> %d; body %s", status, body)
	}
	var patchedApp map[string]any
	json.Unmarshal([]byte(body), &patchedApp)
	patchedAppAttrs := patchedApp["data"].(map[string]any)["attributes"].(map[string]any)
	if patchedAppAttrs["name"] != "Renamed Mock App" {
		t.Fatalf("patched app name = %v, want Renamed Mock App", patchedAppAttrs["name"])
	}
	body, status = ascGet(t, base+"/v1/apps/"+appID, jwt)
	if status != 200 {
		t.Fatalf("GET patched app -> %d", status)
	}
	json.Unmarshal([]byte(body), &patchedApp)
	if patchedApp["data"].(map[string]any)["attributes"].(map[string]any)["name"] != "Renamed Mock App" {
		t.Fatalf("patched app name not persisted: %v", patchedApp["data"])
	}

	// ===== Duplicate bundleId on create -> 409 =====
	body, status = ascPostJSON(t, base+"/v1/apps", jwt, map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{
				"name":     "Clash App",
				"bundleId": "com.example.mockapp",
			},
		},
	})
	if status != 409 {
		t.Fatalf("POST duplicate bundleId -> %d, want 409; body %s", status, body)
	}
	var dupResp map[string]any
	json.Unmarshal([]byte(body), &dupResp)
	dupErrs := dupResp["errors"].([]any)
	if dupErrs[0].(map[string]any)["code"] != "ENTITY_ERROR.ATTRIBUTE.INVALID" {
		t.Fatalf("duplicate bundleId error code = %v", dupErrs[0])
	}

	// ===== Users come from the store and stay stable across calls =====
	body, status = ascGet(t, base+"/v1/users", jwt)
	if status != 200 {
		t.Fatalf("GET users -> %d; body %s", status, body)
	}
	var usersResp map[string]any
	json.Unmarshal([]byte(body), &usersResp)
	users := usersResp["data"].([]any)
	if len(users) < 2 {
		t.Fatalf("users = %d, want >= 2", len(users))
	}
	body, status = ascGet(t, base+"/v1/users?filter[roles]=ADMIN", jwt)
	if status != 200 {
		t.Fatalf("GET users filtered by role -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &usersResp)
	admins := usersResp["data"].([]any)
	if len(admins) != 1 {
		t.Fatalf("ADMIN users = %d, want 1", len(admins))
	}
	if admins[0].(map[string]any)["attributes"].(map[string]any)["username"] != "admin@example.com" {
		t.Fatalf("filtered admin username = %v", admins[0])
	}
}
