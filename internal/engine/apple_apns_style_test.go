package engine

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// apnsPrivateKeyPEM mirrors the adapter's documented fixed synthetic EC
// P-256 provider key (see adapters/apple-apns-style/README.md). The public
// half is baked into scripts/lib.star; the private half signs provider
// tokens in tests exactly the way a real provider key would.
const apnsPrivateKeyPEM = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgz399eDP4CEo1JoR7
A5uHueHShJKhKvna8BiAvVQPkvyhRANCAAS6OMBYKYI6moCMo0FeQ23CAvQMT5sy
MZrf7jMKmvhmI/aMJuodNWq4eLSq6/X4oWriaY7RsKxIrQ5F/Ql+y6XJ
-----END PRIVATE KEY-----`

// apnsSignJWT signs a compact-JSON header/payload pair as a real ES256 JWT
// (raw r||s signature) with the given key.
func apnsSignJWT(t *testing.T, priv *ecdsa.PrivateKey, header, payload string) string {
	t.Helper()
	h := base64.RawURLEncoding.EncodeToString([]byte(header))
	p := base64.RawURLEncoding.EncodeToString([]byte(payload))
	digest := sha256.Sum256([]byte(h + "." + p))
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// apnsMintProviderToken mints a provider JWT: header
// {"alg":"ES256","kid":...}, payload {"iss","iat"} plus an exp claim when
// exp > 0. Signed with the adapter's documented synthetic provider key.
func apnsMintProviderToken(t *testing.T, iat, exp int64) string {
	t.Helper()
	block, _ := pem.Decode([]byte(apnsPrivateKeyPEM))
	if block == nil {
		t.Fatal("bad test key PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	priv, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("test key is not ECDSA")
	}
	header := `{"alg":"ES256","kid":"mock-apns-key-1"}`
	payload := `{"iss":"MOCKTEAMID","iat":` + strconv.FormatInt(iat, 10)
	if exp > 0 {
		payload += `,"exp":` + strconv.FormatInt(exp, 10)
	}
	payload += `}`
	return apnsSignJWT(t, priv, header, payload)
}

// apnsMintForgedToken signs a well-formed ES256 provider JWT with a random
// (unregistered) key — the signature must not verify.
func apnsMintForgedToken(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	header := `{"alg":"ES256","kid":"mock-apns-key-1"}`
	payload := `{"iss":"MOCKTEAMID","iat":` + strconv.FormatInt(now, 10) + `}`
	return apnsSignJWT(t, priv, header, payload)
}

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
	jwt := apnsMintProviderToken(t, time.Now().Unix(), 0) // real ES256, iat now

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

	// ===== POST with expired provider token → 403 ExpiredProviderToken =====
	// iat is 2h in the past; APNs provider tokens live at most 1h.
	expiredJWT := apnsMintProviderToken(t, time.Now().Unix()-7200, 0)
	resp = apnsPost(t, base+"/3/device/"+knownToken, expiredJWT, notifBody)
	if resp.StatusCode != 403 {
		t.Fatalf("POST with expired provider token -> status %d, want 403", resp.StatusCode)
	}
	var expBody map[string]any
	json.NewDecoder(resp.Body).Decode(&expBody)
	resp.Body.Close()
	if expBody["reason"] != "ExpiredProviderToken" {
		t.Fatalf("expired token reason = %v, want ExpiredProviderToken", expBody["reason"])
	}

	// ===== POST with explicit exp in the past → 403 ExpiredProviderToken =====
	explicitExpJWT := apnsMintProviderToken(t, time.Now().Unix()-7200, time.Now().Unix()-3600)
	resp = apnsPost(t, base+"/3/device/"+knownToken, explicitExpJWT, notifBody)
	if resp.StatusCode != 403 {
		t.Fatalf("POST with explicitly expired token -> status %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// ===== POST with forged signature (wrong key) → 403 =====
	resp = apnsPost(t, base+"/3/device/"+knownToken, apnsMintForgedToken(t), notifBody)
	if resp.StatusCode != 403 {
		t.Fatalf("POST with forged-signature JWT -> status %d, want 403", resp.StatusCode)
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
