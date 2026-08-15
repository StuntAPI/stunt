package engine

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
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

	// The minted claims now include aud (the client_id) + iat/exp.
	var claims map[string]any
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims["aud"] != "test-client-id" {
		t.Fatalf("claims aud = %v, want test-client-id", claims["aud"])
	}
	exp, ok := claims["exp"].(float64)
	if !ok || exp <= float64(time.Now().Unix()) {
		t.Fatalf("claims exp = %v, want future timestamp", claims["exp"])
	}
	if claims["iss"] != "https://login.microsoftonline.com/mock-tenant/v2.0" {
		t.Fatalf("claims iss = %v", claims["iss"])
	}

	// ===== Inbound verification is enforced: /v1.0/me with the token works =====

	meBody, meStatus := entraGetBearer(base+"/v1.0/me", accessToken)
	if meStatus != 200 {
		t.Fatalf("GET /v1.0/me -> %d; body %s", meStatus, meBody)
	}

	// ===== Tampered signature → 401 InvalidAuthenticationToken =====

	tamperedParts := strings.Split(accessToken, ".")
	last := "A"
	if tamperedParts[2][len(tamperedParts[2])-1] == 'A' {
		last = "B"
	}
	tamperedParts[2] = tamperedParts[2][:len(tamperedParts[2])-1] + last
	tampered := strings.Join(tamperedParts, ".")
	meBody, meStatus = entraGetBearer(base+"/v1.0/me", tampered)
	if meStatus != 401 {
		t.Fatalf("GET /v1.0/me with tampered token -> %d, want 401; body %s", meStatus, meBody)
	}
	if !strings.Contains(meBody, "InvalidAuthenticationToken") {
		t.Fatalf("tampered token body %q, want InvalidAuthenticationToken", meBody)
	}

	// ===== Properly signed but EXPIRED token → 401 =====

	expired := entraMintExpiredToken(t)
	meBody, meStatus = entraGetBearer(base+"/v1.0/me", expired)
	if meStatus != 401 {
		t.Fatalf("GET /v1.0/me with expired token -> %d, want 401; body %s", meStatus, meBody)
	}
}

// entraPrivateKeyPEM mirrors the adapter's fixed mock RSA keypair (see
// adapters/entra-id-style/scripts/lib.star); it lets tests mint properly
// signed tokens with adversarial claims (e.g. an expired exp).
const entraPrivateKeyPEM = `-----BEGIN PRIVATE KEY-----
MIIEowIBAAKCAQEAvr9/xXKqJmLa53C2IDf3DUu83XmYEwqLbxD62LJg7x+aLJ1d
1uFHDiCphI5ab6d1fEx6BKa0CVJ9VG74+Fg5vM07SgztJgWvHpFGDM4lB5v/BCe4
5sfObjMkPkRCcBAvZoPmRt3OilX8pmbABpCAjBiaT/nb6O7a85VfrYzif7iYpDWq
pymZgCS6PO/o9ju/UHqwhB2EzjZdNZGiwPfn2dEri50WCoKWtXSJEG3GhsBueCRG
5jzbhIGtPP7XjnZCf79ISr2V44OahuK0LxrFYV9d3r7sCGj0mtH5ExXUjlX9yIKq
WDwHr2RuadNFzE8AjkyKyH7Xgc06y5t2/9cAoQIDAQABAoIBAA77NOKR1y3FIVrA
ljlRE/ECJA8EBA7oyts6Cu2Wkvjs8zuyT2K3VlCUfaPo108CKL7Otd2kJys9RJUr
UxgUO9KpjtDJ051jIGYm9EjArxVaKe0OXp4Xjs3GbAAM9efdyY9EaENkG9rvFnUO
SGIrmsEGFKaX4e75RY6Qio9zq31q5B3Iw8u8hWe0/EdcFSLdRojmRfNb5KrIrF2U
V/TwWkaY77QrctX2uXMliq2p4vE1jQunCaiMM6hLHhu3lBLXq/OWtPgB0fW+Gugh
jQODvW2/Oo/3eA3Ie25mg1c1cEhqXwxh2cgB4vmes7VEvLMS8IzAQfQrvNWtmL0Z
l49IY/ECgYEA7GVcGjnqYqYeOsW6u0nooXaKtp4oBwyAcA+fXTXo8mSXfyufgj6Q
k9KTTrKryJ6sOWIWGneBi7hatyiSVGCqjRTQIdfGm6NaKrsyKBub9ypIRcVtZgBh
4Z6HSio6fD7j3farJdSiLvYBByZGWnSbEC8Nt0APJriDR92+vpwNwVkCgYEAzpEL
GLa9bEfolTUqiQbvMOQntkbNJGX+iE9NpWGpzIpqFo32ltjpLNT6rSH11eokrpRR
E7KKT+86DVOqWzIV7IlNWJJVVueElOYdFlZG99HFhnoG9PdAq14sbqP8AlAVrjyN
0HEqal+oMiK3KJnbOUMXpkoyCxwZ/u4xoqY1yIkCgYAYN8IZxbknZhFOwBcDPO0i
LXzEfKtpHXTDBjazW+SDgJ6snpF2zGYPXtFMjK1gnjDSqCPPjlKtN7PDc9qZ3lVa
ork33l0wcKm6GvdmeH2f8qr4yuMMQhnE/XKqvGzFccPyZ2TdOU1sNjOgweEPP0br
f4aOMXfb5ac9Y5A5As+98QKBgQCOfDIhS/wBcuCV+2RpvKTFHrvd2ZyrnMckEz/F
8kYD1v4yrJ4Jk3nT+N0pC6HdenLvEVOTuLX7SVLL2ohJ+5Rv4o29qMLA/VXQt6Ic
xEqTqtkLV6Tw2JR9IKqZbvfoSIGL/Cz+OPE/CtikLJoWoXo8V3E6vTcjvrCXzoni
Xa//sQKBgEzzVG6cFZ3heWNs1aw0iLWkiyNVy1bMGHG0uCHdkUooBVVDhePuFJjC
d1e1sGLm/t1FsCtnZfjUaz4UDBtZIuzsa8l+6V4oYonWEL4za2S28BZEOKfI0I5d
CbHMqyTew/B29KnXJaLkcOiQ4NFgjOQ/fMjzckrXedZp7Za6WZEg
-----END PRIVATE KEY-----`

// entraMintExpiredToken mints a properly RS256-signed access token whose
// exp is in the past — the signature verifies, only the expiry fails.
func entraMintExpiredToken(t *testing.T) string {
	t.Helper()
	block, _ := pem.Decode([]byte(entraPrivateKeyPEM))
	if block == nil {
		t.Fatal("bad test key PEM")
	}
	var priv *rsa.PrivateKey
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		pk, ok := key.(*rsa.PrivateKey)
		if !ok {
			t.Fatal("test key is not RSA")
		}
		priv = pk
	} else {
		priv, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			t.Fatalf("parse test key: %v", err)
		}
	}
	now := time.Now().Unix()
	header := `{"alg":"RS256","typ":"JWT","kid":"mock-entra-kid-1"}`
	payload := `{"sub":"00000000-0000-0000-0000-000001","name":"Expired",` +
		`"scp":"User.Read","nonce":"0","iss":"https://login.microsoftonline.com/mock-tenant/v2.0",` +
		`"aud":"test-client-id","iat":` + strconv.FormatInt(now-7200, 10) +
		`,"exp":` + strconv.FormatInt(now-3600, 10) + `}`
	hb := base64.RawURLEncoding.EncodeToString([]byte(header))
	pb := base64.RawURLEncoding.EncodeToString([]byte(payload))
	digest := sha256.Sum256([]byte(hb + "." + pb))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return hb + "." + pb + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// entraGetBearer performs a GET with an Authorization: Bearer header.
func entraGetBearer(rawurl, token string) (string, int) {
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return "", 0
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
