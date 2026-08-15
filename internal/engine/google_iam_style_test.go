package engine

import (
	"bytes"
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

// googleIAMPrivateKeyPEM mirrors the adapter's documented fixed synthetic
// "Google" RSA-2048 keypair (see adapters/google-iam-style/scripts/lib.star,
// shared with google-style): the public half is served at
// GET /oauth2/v3/certs, the private half signs jwt-bearer assertions in
// tests exactly the way a real service-account key would.
const googleIAMPrivateKeyPEM = `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEAvBDsZejhK5crr0/kWSHthMSxv42QviE9IYlSQf9lZG4AjBym
TX4q6UTuYoFnppDoLA0Llm2k8Ybj6GBpPFq1DzRuOF0/Iee8+qB+FCJb1hA4O1FL
BSoGHnyzx8PmvDth4LTKMgft9mtuozUe04WL0Cf/cx96wjo4BeO72jYZDYOI2kpC
H8lahdwYyykqnIdEALoTdIpCHd4P0cgBHz+sS3UCPcBF1yt61vUJrKcoJCFqQ9oQ
0t+aOHfIQpvoAgedjeJL9x2v9IUZN5lKfV2i2ShaeiCTe8444oLHYHKh59tiMdL3
DiJSdtMyrfJNT5+rvAqzYurkyR4eajWLTlxo6wIDAQABAoIBACmNsbYIwSvhAIGB
bQp2sSTrUvzomikwbfHphhfYBv6sQYmz0NkBfhjBpsx0HENU9D+7eCp6On41WEkh
eE8iGaxs4MeqbscejYZxDLqFJvaC6fHNUf6nnOeClTSX5/UCR+ue9qgcUWtnrG/6
Tj/dW5mYJNy6gWTF+VfvzDN4TYvK+czUp5bEPf65yZMN6ALv2Vd3BnT9/JfEUr8h
Hv8FqrOObt4XWG5tBdyBHs09zQYhUGwoHi30AQQN8n0+k3QVA+EbdUeTbj8ZT80G
F+DGrYYO/XcN6VjihyOn7CPWCDQGymrl26WMKeWAU1NP76U/+/Wk7UYu6ClVzcxK
RP53FUkCgYEA0I6cnvRRde2Yl+V0SAyNUHRDse1jwn9oNgh+mebjJ7jKGep2oGs/
zF91l8hT3iyXWCjTatldu/4rEziO5ORWrTL9t3LXTFch+J9N+B9K4WGD2y+TtUpJ
RPHbdVYsw0omkwQSmpjhJskgzPHdhAejFFxdjDFSouHUo4QyHXJIGVMCgYEA5tkD
b0rVcgAa1iyrk2zlHJ7DMoc0VK5huRcDum60HzcvffxtX/uSXMWajxmXF4MwLmIx
HbaANzRwkXK6Cpdkws/Z0M9l3p/X85rZx8sZLT4tSaTG5py6CJO0l5WkUZTJkLu8
KWBaQg6gaYG35t0WsMjymzW6elANlxzIFbDMxwkCgYEAmoZwCV5g1Q28GB+Mrq2O
LuRWHAkV91BLOG3Gz+VAvXevVtBgILAWTykTieiGK4HCiTGGpA514wqJg+5OAc4l
YqL7VecjGo8cvofaT1NwOdn0xnxT5ukprIm+3wuAkxnnxtonpqBLgl9XjEJQrLiz
3iwpq+wHnGPTF2ylbSf1v70CgYAhqPsLO0osOT+wgwrxkCtIJQ4pS/Whc1vkdSqi
AIpbEtzl7ey01iXdSSLkQsL5NrPLz52By56ebhML4kKmULTsgwornFIqR/xhFO80
ZrThF/PajSBDeA7YOVFX2QYAr0VEyVsCXX5Lq35QZA3Ap/QrCuH1J7xtIUcaBaRX
JVR2oQKBgQCiz10UVl5268YAq9UiyxTWLX9wnG0aJ+FeJKSzRq4qSJ7y/KtwN3u2
s952TPoihAZ2aIPBJGvL621VL30zV0X0T4phZfjxSyItHlJ9agu6HcMrvKHItGLE
uSorvUl2S6aV5QKC9LFe6Gm19SPBmok2jpL4DQCAoPbVV2y9+dFMsw==
-----END RSA PRIVATE KEY-----`

// iamSignAssertion builds a real RS256 jwt-bearer assertion (Google's SA
// claim set: iss, scope, aud == token endpoint, iat, exp) signed with key.
func iamSignAssertion(t *testing.T, key *rsa.PrivateKey, iss string, iat, exp int64) string {
	t.Helper()
	header := `{"alg":"RS256","typ":"JWT"}`
	payload := `{"iss":"` + iss + `",` +
		`"scope":"openid https://www.googleapis.com/auth/cloud-platform",` +
		`"aud":"https://oauth2.googleapis.com/token",` +
		`"iat":` + strconv.FormatInt(iat, 10) + `,"exp":` + strconv.FormatInt(exp, 10) + `}`
	h := base64.RawURLEncoding.EncodeToString([]byte(header))
	p := base64.RawURLEncoding.EncodeToString([]byte(payload))
	digest := sha256.Sum256([]byte(h + "." + p))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// iamMintAssertion mints a valid assertion with the adapter's documented key.
func iamMintAssertion(t *testing.T, ttl int64) string {
	t.Helper()
	block, _ := pem.Decode([]byte(googleIAMPrivateKeyPEM))
	if block == nil {
		t.Fatal("bad test key PEM")
	}
	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	now := time.Now().Unix()
	return iamSignAssertion(t, priv, "mock-default@mock-project.iam.gserviceaccount.com", now, now+ttl)
}

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

	// ===== JWT-bearer token exchange (real RS256 assertion) =====

	assertion := iamMintAssertion(t, 3600)

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

	// ===== openid scope → real RS256 id_token verifiable via /oauth2/v3/certs =====

	idToken, ok := tokenResp["id_token"].(string)
	if !ok || idToken == "" {
		t.Fatalf("id_token = %v, want non-empty string (assertion scope includes openid)", tokenResp["id_token"])
	}
	certsBody, certsStatus := iamGetAuth(t, base+"/oauth2/v3/certs", token)
	if certsStatus != 200 {
		t.Fatalf("GET /oauth2/v3/certs -> status %d, want 200; body %s", certsStatus, certsBody)
	}
	var certs struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal([]byte(certsBody), &certs); err != nil {
		t.Fatalf("unmarshal certs: %v (body %s)", err, certsBody)
	}
	if len(certs.Keys) != 1 {
		t.Fatalf("certs keys = %d, want 1", len(certs.Keys))
	}
	iamVerifyIDToken(t, idToken, certs.Keys[0])

	// ===== Forged assertion (wrong signing key) → 400 invalid_grant =====

	forgedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	forged := iamSignAssertion(t, forgedKey, "attacker@mock-project.iam.gserviceaccount.com", now, now+3600)
	body, status = iamPostForm(t, base+"/oauth2/v4/token", url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {forged},
	})
	if status != 400 {
		t.Fatalf("forged assertion -> status %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "invalid_grant") {
		t.Fatalf("forged assertion -> body %q, want invalid_grant", body)
	}

	// ===== Properly signed but EXPIRED assertion → 400 invalid_grant =====

	block, _ := pem.Decode([]byte(googleIAMPrivateKeyPEM))
	if block == nil {
		t.Fatal("bad test key PEM")
	}
	privKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	expired := iamSignAssertion(t, privKey, "mock-default@mock-project.iam.gserviceaccount.com", now-7200, now-3600)
	body, status = iamPostForm(t, base+"/oauth2/v4/token", url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {expired},
	})
	if status != 400 {
		t.Fatalf("expired assertion -> status %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "invalid_grant") {
		t.Fatalf("expired assertion -> body %q, want invalid_grant", body)
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

// iamVerifyIDToken verifies an RS256 id_token against a served JWK and
// checks the claim set (iss, aud == the SA email, future exp).
func iamVerifyIDToken(t *testing.T, token string, jwk map[string]any) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("id_token has %d parts, want 3", len(parts))
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(jwk["n"].(string))
	if err != nil {
		t.Fatalf("decode n: %v", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(jwk["e"].(string))
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
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("id_token RS256 signature did not verify against the served certs key: %v", err)
	}
	var claims map[string]any
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims["iss"] != "https://accounts.google.com" {
		t.Fatalf("id_token iss = %v", claims["iss"])
	}
	if claims["aud"] != "mock-default@mock-project.iam.gserviceaccount.com" {
		t.Fatalf("id_token aud = %v", claims["aud"])
	}
	if exp, ok := claims["exp"].(float64); !ok || exp <= float64(time.Now().Unix()) {
		t.Fatalf("id_token exp = %v, want future timestamp", claims["exp"])
	}
}

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
