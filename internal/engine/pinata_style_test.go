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

	"stuntapi.com/stunt/internal/manifest"
)

// TestPinataStyleAdapter exercises the Pinata IPFS pinning API:
//   - testAuthentication → success message
//   - pinJSONToIPFS → CID (IpfsHash)
//   - pinList → shows the pinned CID
//   - pinByHash → lookup
//   - unpin → removes the pin
//   - 401 without auth
func TestPinataStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "pinata-style")
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
			"pinata": {Adapter: absAdapterDir},
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

	base := addrs["pinata"]

	// ===== 401 without auth =====

	_, status := pinataGet(t, base+"/data/testAuthentication", "", "")
	if status != 401 {
		t.Fatalf("no-auth testAuthentication -> %d, want 401", status)
	}

	// ===== testAuthentication (API key pair) =====

	body, status := pinataGet(t, base+"/data/testAuthentication", "key-123", "secret-456")
	if status != 200 {
		t.Fatalf("testAuthentication -> %d, want 200; body %s", status, body)
	}
	var authResp map[string]any
	if err := json.Unmarshal([]byte(body), &authResp); err != nil {
		t.Fatalf("unmarshal auth resp: %v (body %s)", err, body)
	}
	msg, ok := authResp["message"].(string)
	if !ok || msg == "" {
		t.Fatalf("message = %v, want non-empty string", authResp["message"])
	}

	// ===== pinJSONToIPFS (Bearer JWT) =====

	body, status = pinataPostJSON(t, base+"/pinning/pinJSONToIPFS", "Bearer jwt-token", map[string]any{
		"pinataContent":  map[string]any{"hello": "world"},
		"pinataMetadata": map[string]any{"name": "test-pin"},
	})
	if status != 200 {
		t.Fatalf("pinJSONToIPFS -> %d, want 200; body %s", status, body)
	}
	var pinResp map[string]any
	if err := json.Unmarshal([]byte(body), &pinResp); err != nil {
		t.Fatalf("unmarshal pin resp: %v (body %s)", err, body)
	}
	ipfsHash, ok := pinResp["IpfsHash"].(string)
	if !ok || ipfsHash == "" {
		t.Fatalf("IpfsHash = %v, want non-empty", pinResp["IpfsHash"])
	}
	if !strings.HasPrefix(ipfsHash, "Qm") {
		t.Fatalf("IpfsHash = %v, want Qm-prefixed CID", ipfsHash)
	}
	pinSize, ok := pinResp["PinSize"]
	if !ok || pinSize == nil {
		t.Fatalf("PinSize missing")
	}
	if pinResp["isDuplicate"] != false {
		t.Fatalf("isDuplicate = %v, want false", pinResp["isDuplicate"])
	}

	// ===== pinList shows the pin =====

	body, status = pinataGet(t, base+"/data/pinList", "Bearer jwt-token", "")
	if status != 200 {
		t.Fatalf("pinList -> %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list resp: %v (body %s)", err, body)
	}
	count, ok := listResp["count"]
	if !ok {
		t.Fatalf("count missing from pinList")
	}
	if count.(float64) < 1 {
		t.Fatalf("count = %v, want >= 1", count)
	}
	rows, ok := listResp["rows"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("rows = %v, want non-empty array", listResp["rows"])
	}
	row0 := rows[0].(map[string]any)
	if row0["ipfs_pin_hash"] != ipfsHash {
		t.Fatalf("pin hash = %v, want %v", row0["ipfs_pin_hash"], ipfsHash)
	}
	if row0["metadata"] == nil {
		t.Fatalf("metadata missing from pin row")
	}

	// ===== pinByHash =====

	body, status = pinataGet(t, base+"/data/pinByHash?hash="+ipfsHash, "Bearer jwt-token", "")
	if status != 200 {
		t.Fatalf("pinByHash -> %d, want 200; body %s", status, body)
	}

	// ===== unpin =====

	body, status = pinataDelete(t, base+"/pinning/unpin/"+ipfsHash, "Bearer jwt-token")
	if status != 200 {
		t.Fatalf("unpin -> %d, want 200; body %s", status, body)
	}

	// ===== pinList now empty =====

	body, status = pinataGet(t, base+"/data/pinList", "Bearer jwt-token", "")
	if status != 200 {
		t.Fatalf("pinList after unpin -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list resp: %v (body %s)", err, body)
	}
	count = listResp["count"]
	if count.(float64) != 0 {
		t.Fatalf("count after unpin = %v, want 0", count)
	}

	// ===== unpin non-existent → 403 =====

	_, status = pinataDelete(t, base+"/pinning/unpin/QmNonexistent", "Bearer jwt-token")
	if status != 403 {
		t.Fatalf("unpin non-existent -> %d, want 403", status)
	}
}

// === Pinata test helpers ===

func pinataPostJSON(t *testing.T, rawurl, auth string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	pinataApplyAuth(req, auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func pinataGet(t *testing.T, rawurl, auth, secret string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if secret != "" {
		req.Header.Set("pinata_api_key", auth)
		req.Header.Set("pinata_secret_api_key", secret)
	} else {
		pinataApplyAuth(req, auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func pinataDelete(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	pinataApplyAuth(req, auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// pinataApplyAuth sets either the Bearer header or the API key pair.
// auth values: "" (no auth), "Bearer xxx" (JWT), "key-123" + secret-456 (API key).
func pinataApplyAuth(req *http.Request, auth string) {
	if auth == "" {
		return
	}
	if len(auth) > 7 && auth[:7] == "Bearer " {
		req.Header.Set("Authorization", auth)
	} else {
		req.Header.Set("pinata_api_key", auth)
		req.Header.Set("pinata_secret_api_key", "secret-456")
	}
}
