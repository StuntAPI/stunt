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

func TestAppleAPNsStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "apple-apns-style")
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
			"apns": {Adapter: absAdapterDir},
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

	base := addrs["apns"]
	jwt := mintES256JWT(t)

	// Known device token (seeded).
	const knownToken = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	const unknownToken = "0000000000000000000000000000000000000000000000000000000000000000"

	// ===== POST to known device → 200 + apns-id header =====
	notifBody := map[string]any{
		"aps": map[string]any{
			"alert": map[string]any{
				"title": "Test Push",
				"body":  "Hello from stunt!",
			},
			"badge": 1,
		},
	}
	resp := apnsPost(t, base+"/3/device/"+knownToken, jwt, notifBody)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST known device -> status %d, want 200; body %s", resp.StatusCode, string(body))
	}
	apnsID := resp.Header.Get("apns-id")
	if apnsID == "" {
		t.Fatal("POST known device: missing apns-id response header")
	}
	resp.Body.Close()

	// ===== POST to unknown device → 400 BadDeviceToken =====
	resp = apnsPost(t, base+"/3/device/"+unknownToken, jwt, notifBody)
	if resp.StatusCode != 400 {
		t.Fatalf("POST unknown device -> status %d, want 400", resp.StatusCode)
	}
	var errBody map[string]any
	json.NewDecoder(resp.Body).Decode(&errBody)
	resp.Body.Close()
	if errBody["reason"] != "BadDeviceToken" {
		t.Fatalf("unknown device reason = %v, want BadDeviceToken", errBody["reason"])
	}

	// ===== POST without auth → 403 =====
	resp = apnsPost(t, base+"/3/device/"+knownToken, "", notifBody)
	if resp.StatusCode != 403 {
		t.Fatalf("POST without auth -> status %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// ===== POST with bad alg → 403 =====
	resp = apnsPost(t, base+"/3/device/"+knownToken, mintBadAlgJWT(t), notifBody)
	if resp.StatusCode != 403 {
		t.Fatalf("POST with HS256 JWT -> status %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// ===== POST with empty payload → 400 PayloadEmpty =====
	emptyBody := map[string]any{
		"aps": map[string]any{},
	}
	resp = apnsPost(t, base+"/3/device/"+knownToken, jwt, emptyBody)
	if resp.StatusCode != 400 {
		t.Fatalf("POST empty aps -> status %d, want 400", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&errBody)
	resp.Body.Close()
	if errBody["reason"] != "PayloadEmpty" {
		t.Fatalf("empty aps reason = %v, want PayloadEmpty", errBody["reason"])
	}

	// ===== POST second notification to known device → 200 + different apns-id =====
	resp = apnsPost(t, base+"/3/device/"+knownToken, jwt, notifBody)
	if resp.StatusCode != 200 {
		t.Fatalf("POST second -> status %d, want 200", resp.StatusCode)
	}
	apnsID2 := resp.Header.Get("apns-id")
	resp.Body.Close()
	if apnsID2 == "" || apnsID2 == apnsID {
		t.Fatalf("second apns-id = %q, want non-empty and different from first %q", apnsID2, apnsID)
	}

	// ===== GET sent notifications → shows both (STATEFUL) =====
	body, status := apnsGet(t, base+"/3/device/"+knownToken+"/notifications", jwt)
	if status != 200 {
		t.Fatalf("GET notifications -> status %d, want 200; body %s", status, body)
	}
	var notifList []any
	if err := json.Unmarshal([]byte(body), &notifList); err != nil {
		t.Fatalf("unmarshal notifications: %v (body %s)", err, body)
	}
	if len(notifList) < 2 {
		t.Fatalf("notifications count = %d, want >= 2 (both sent)", len(notifList))
	}
}

// === APNs test helpers ===

func apnsPost(t *testing.T, rawurl, jwt string, body map[string]any) *http.Response {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func apnsGet(t *testing.T, rawurl, jwt string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
