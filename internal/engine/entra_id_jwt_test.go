package engine

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestEntraIDStyleJWTVerifies proves the adapter now mints a cryptographically
// real RS256 access token: it runs the authorization-code flow, fetches the
// JWKS the adapter serves, reconstructs the RSA public key from n/e, and
// verifies the token signature over header.payload. Previously the "signature"
// was the literal string "mock-signature-...".
func TestEntraIDStyleJWTVerifies(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "entra-id-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"entra": {Adapter: adapterDir},
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
	base := addrs["entra"]

	const redirectURI = "http://localhost:3000/callback"
	resp := entraGetNoRedirect(t, base+"/common/oauth2/v2.0/authorize?"+
		"client_id=test-client-id"+
		"&redirect_uri="+url.QueryEscape(redirectURI)+
		"&state=s&response_type=code&scope="+url.QueryEscape("openid profile User.Read"))
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> %d, want 302", resp.StatusCode)
	}
	authCode := entraExtractParam(resp.Header.Get("Location"), "code")
	if authCode == "" {
		t.Fatal("no code in authorize redirect")
	}

	body, status := entraPostForm(t, base+"/common/oauth2/v2.0/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {"test-client-id"},
		"client_secret": {"test-client-secret"},
		"redirect_uri":  {redirectURI},
		"scope":         {"openid profile User.Read"},
	})
	if status != 200 {
		t.Fatalf("token exchange -> %d; body %s", status, body)
	}
	var tok map[string]any
	if err := json.Unmarshal([]byte(body), &tok); err != nil {
		t.Fatalf("unmarshal token: %v", err)
	}
	accessToken, _ := tok["access_token"].(string)
	if accessToken == "" {
		t.Fatal("access_token empty")
	}

	// Fetch JWKS and verify the token signature with the served public key.
	jwksResp, err := http.Get(base + "/common/discovery/v2.0/keys")
	if err != nil {
		t.Fatal(err)
	}
	defer jwksResp.Body.Close()
	jwksBytes, _ := io.ReadAll(jwksResp.Body)
	var jwks struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(jwksBytes, &jwks); err != nil {
		t.Fatalf("unmarshal jwks: %v (body %s)", err, jwksBytes)
	}
	if len(jwks.Keys) != 1 {
		t.Fatalf("jwks keys = %d, want 1", len(jwks.Keys))
	}
	key := jwks.Keys[0]
	if key["kty"] != "RSA" || key["alg"] != "RS256" {
		t.Fatalf("jwks key = %v, want RSA/RS256", key)
	}
	kid, _ := key["kid"].(string)

	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	// Header must carry the matching kid + RS256 alg.
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var hdr map[string]any
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if hdr["alg"] != "RS256" || hdr["kid"] != kid {
		t.Fatalf("header = %v, want alg=RS256 kid=%s", hdr, kid)
	}

	// Reconstruct the RSA public key from n/e and verify the signature.
	nBytes, err := base64.RawURLEncoding.DecodeString(key["n"].(string))
	if err != nil {
		t.Fatalf("decode n: %v", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(key["e"].(string))
	if err != nil {
		t.Fatalf("decode e: %v", err)
	}
	pub := &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(new(big.Int).SetBytes(eBytes).Int64()),
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	signingInput := parts[0] + "." + parts[1]
	h := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig); err != nil {
		t.Fatalf("RS256 signature did not verify against the JWKS public key: %v", err)
	}
}
