package adapters

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// Drives the signin-with-apple-style adapter scripts directly (lib.star
// preloaded) over a shared store and virtual clock: the /auth/authorize
// redirect with its single-use code, the /auth/token exchange and refresh
// (client_secret JWTs are minted and verified with REAL ES256 crypto), and
// the /auth/keys JWKS whose served key verifies the minted id_token.
const (
	siwaVMHost        = "appleid.apple.test"
	siwaVMClientID    = "com.example.signin.service"
	siwaVMRedirectURI = "https://client.example.test/callback"
	siwaVMState       = "vm-state-xyz"
)

// siwaVMPrivPEM mirrors the adapter's documented synthetic EC P-256 keypair
// (README): the public half is served at GET /auth/keys, the private half
// signs client_secret JWTs here exactly the way a real developer key would.
const siwaVMPrivPEM = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgwp+ZPlH6FJFcHfYS
Nd1ENT6RzZQUkTDz67JzlvlvoRShRANCAAREw7SM/k20F3w/oDzR9M6V6jHDK4Hi
RkybQejVvpvgn2EoiMcG6uzUH+aAOgtE+0wCB2gWqc5DoeX6fHyFgDqT
-----END PRIVATE KEY-----`

type siwaVMFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vm   *starlark.VM
	host string
}

func newSiwaVMFixture(t *testing.T, start time.Time) *siwaVMFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "signin-with-apple-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(root, "scripts", "oauth.star"))
	if err != nil {
		t.Fatalf("read oauth.star: %v", err)
	}
	tmp := t.TempDir()
	store, _ := primitives.Open(filepath.Join(tmp, "s.db"))
	t.Cleanup(func() { store.Close() })
	kvStore, _ := kv.Open(filepath.Join(tmp, "s.kv.db"))
	t.Cleanup(func() { kvStore.Close() })
	blobStore, _ := blob.Open(filepath.Join(tmp, "blobs"))
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: events.NewEmitter(),
	})
	vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
	if err != nil {
		t.Fatalf("LoadWithLib oauth.star: %v", err)
	}
	return &siwaVMFixture{t: t, vc: vc, vm: vm, host: siwaVMHost}
}

// call drives a handler: query params for the GET routes, form-field map for
// the POSTed /auth/token (what the engine's form parser hands the handler).
func (f *siwaVMFixture) call(handler, method, path string, query, body map[string]string) starlark.Response {
	f.t.Helper()
	if query == nil {
		query = map[string]string{}
	}
	if body == nil {
		body = map[string]string{}
	}
	anyBody := make(map[string]any, len(body))
	for k, v := range body {
		anyBody[k] = v
	}
	resp, err := f.vm.Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: map[string]string{}, Body: anyBody, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// siwaVMKey parses the documented private key PEM.
func siwaVMKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	block, _ := pem.Decode([]byte(siwaVMPrivPEM))
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
	return priv
}

// siwaVMSecret mints a REAL ES256 client_secret JWT (raw r||s signature)
// with the documented key: alg ES256, aud appleid.apple.com, exp at
// now+ttl — the exact shape the adapter's verifier demands.
func siwaVMSecret(t *testing.T, priv *ecdsa.PrivateKey, now, ttl int64, aud string) string {
	t.Helper()
	header := `{"alg":"ES256","kid":"mock-siwa-key-1","typ":"JWT"}`
	payload := `{"iss":"MOCKTEAMID","sub":"` + siwaVMClientID + `","aud":"` + aud +
		`","iat":` + jsonInt(now) + `,"exp":` + jsonInt(now+ttl) + `}`
	return siwaVMSign(t, priv, header, payload)
}

// siwaVMSign signs a compact-JSON header/payload pair as an ES256 JWT
// (base64url segments, 64-byte raw r||s signature).
func siwaVMSign(t *testing.T, priv *ecdsa.PrivateKey, header, payload string) string {
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

// jsonInt formats an int64 for hand-built compact JSON payloads.
func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// siwaVMCode runs the authorize redirect once and extracts the code param.
func siwaVMCode(t *testing.T, f *siwaVMFixture, state string) string {
	t.Helper()
	resp := f.call("on_authorize", "GET", "/auth/authorize", map[string]string{
		"client_id": siwaVMClientID, "redirect_uri": siwaVMRedirectURI,
		"state": state, "response_type": "code", "scope": "name email",
	}, nil)
	if resp.Status != 302 {
		t.Fatalf("authorize -> %d: %v", resp.Status, resp.Body)
	}
	loc := resp.Headers["Location"]
	if loc == "" {
		t.Fatal("authorize: missing Location header")
	}
	var code string
	for _, kv := range strings.Split(strings.SplitN(loc, "?", 2)[1], "&") {
		if strings.HasPrefix(kv, "code=") {
			code = strings.TrimPrefix(kv, "code=")
		}
	}
	if code == "" {
		t.Fatalf("authorize: no code in Location %q", loc)
	}
	return code
}

// siwaVMSegment decodes a base64url JWT segment to a JSON map.
func siwaVMSegment(t *testing.T, token string, idx int) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q has %d segments, want 3", token, len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[idx])
	if err != nil {
		t.Fatalf("decode segment %d: %v", idx, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal segment %d (%s): %v", idx, raw, err)
	}
	return m
}

func TestSigninWithAppleVMOAuthFlow(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newSiwaVMFixture(t, base)
	priv := siwaVMKey(t)
	secret := siwaVMSecret(t, priv, base.Unix(), 3600, "https://appleid.apple.com")

	// ===== authorize redirects with a single-use code plus state and validates its params =====
	resp := f.call("on_authorize", "GET", "/auth/authorize", map[string]string{
		"client_id": siwaVMClientID, "redirect_uri": siwaVMRedirectURI,
		"state": siwaVMState, "response_type": "code", "scope": "name email",
	}, nil)
	if resp.Status != 302 {
		t.Fatalf("authorize -> %d: %v", resp.Status, resp.Body)
	}
	loc := resp.Headers["Location"]
	if !strings.HasPrefix(loc, siwaVMRedirectURI+"?code=") {
		t.Fatalf("authorize Location = %q, want %s?code=...", loc, siwaVMRedirectURI)
	}
	if !strings.Contains(loc, "state="+siwaVMState) {
		t.Fatalf("authorize Location %q missing state %q", loc, siwaVMState)
	}
	// A redirect_uri that already carries a query gets & not ?.
	extra := f.call("on_authorize", "GET", "/auth/authorize", map[string]string{
		"client_id": siwaVMClientID, "redirect_uri": "https://client.example.test/cb?x=1",
		"response_type": "code",
	}, nil)
	if l := extra.Headers["Location"]; !strings.Contains(l, "cb?x=1&code=") {
		t.Fatalf("authorize Location %q, want & separator after existing query", l)
	}
	// Missing required params and a wrong response_type are 400s.
	if r := f.call("on_authorize", "GET", "/auth/authorize", map[string]string{"response_type": "code"}, nil); r.Status != 400 {
		t.Fatalf("authorize without client_id/redirect_uri -> %d, want 400", r.Status)
	}
	if r := f.call("on_authorize", "GET", "/auth/authorize", map[string]string{
		"client_id": siwaVMClientID, "redirect_uri": siwaVMRedirectURI, "response_type": "token",
	}, nil); r.Status != 400 || r.Body["error"] != "unsupported_response_type" {
		t.Fatalf("authorize response_type=token -> %d %v, want 400 unsupported_response_type", r.Status, r.Body)
	}

	// ===== the token exchange mints a real es256 id_token with apple claim shapes =====
	code := siwaVMCode(t, f, siwaVMState)
	tok := f.call("on_token", "POST", "/auth/token", nil, map[string]string{
		"grant_type": "authorization_code", "code": code,
		"client_id": siwaVMClientID, "client_secret": secret, "redirect_uri": siwaVMRedirectURI,
	})
	if tok.Status != 200 {
		t.Fatalf("token exchange -> %d: %v", tok.Status, tok.Body)
	}
	if tok.Body["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", tok.Body["token_type"])
	}
	if n := ckVMNum(tok.Body["expires_in"]); n != 3600 {
		t.Fatalf("expires_in = %v, want 3600", tok.Body["expires_in"])
	}
	access, _ := tok.Body["access_token"].(string)
	refresh, _ := tok.Body["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("access/refresh token = %q / %q, want non-empty", access, refresh)
	}
	idToken, _ := tok.Body["id_token"].(string)
	if idToken == "" {
		t.Fatalf("id_token = %v, want non-empty", tok.Body["id_token"])
	}
	header := siwaVMSegment(t, idToken, 0)
	if header["alg"] != "ES256" || header["kid"] != "mock-siwa-key-1" || header["typ"] != "JWT" {
		t.Fatalf("id_token JOSE header = %v, want ES256/mock-siwa-key-1/JWT", header)
	}
	claims := siwaVMSegment(t, idToken, 1)
	if claims["iss"] != "https://appleid.apple.com" {
		t.Fatalf("id_token iss = %v", claims["iss"])
	}
	if claims["aud"] != siwaVMClientID {
		t.Fatalf("id_token aud = %v, want %s", claims["aud"], siwaVMClientID)
	}
	if sub, _ := claims["sub"].(string); !strings.HasPrefix(sub, "00") {
		t.Fatalf("id_token sub = %v, want 00-prefixed user id", claims["sub"])
	}
	// Apple ships email_verified / is_private_email as STRINGS and
	// nonce_supported as a bool — the adapter mirrors the real shapes.
	if email, _ := claims["email"].(string); !strings.HasSuffix(email, "@privaterelay.appleid.com") {
		t.Fatalf("id_token email = %v, want privaterelay address", claims["email"])
	}
	if claims["email_verified"] != "true" || claims["is_private_email"] != "false" {
		t.Fatalf("id_token email flags = %v/%v, want \"true\"/\"false\" strings", claims["email_verified"], claims["is_private_email"])
	}
	if claims["nonce_supported"] != true {
		t.Fatalf("id_token nonce_supported = %v, want boolean true", claims["nonce_supported"])
	}
	if iat, exp := ckVMNum(claims["iat"]), ckVMNum(claims["exp"]); exp-iat != 3600 || iat != float64(base.Unix()) {
		t.Fatalf("id_token iat/exp = %v/%v, want virtual now and +1h", iat, exp)
	}

	// ===== the served jwks verifies the minted id_token signature =====
	keys := f.call("on_get_keys", "GET", "/auth/keys", nil, nil)
	if keys.Status != 200 {
		t.Fatalf("auth/keys -> %d: %v", keys.Status, keys.Body)
	}
	keyList, _ := keys.Body["keys"].([]any)
	if len(keyList) != 1 {
		t.Fatalf("JWKS keys = %d, want 1", len(keyList))
	}
	jwk, _ := keyList[0].(map[string]any)
	if jwk["kty"] != "EC" || jwk["crv"] != "P-256" || jwk["alg"] != "ES256" || jwk["use"] != "sig" || jwk["kid"] != "mock-siwa-key-1" {
		t.Fatalf("JWKS key = %v, want EC/P-256/ES256/sig/mock-siwa-key-1", jwk)
	}
	x, err := base64.RawURLEncoding.DecodeString(jwk["x"].(string))
	if err != nil || len(x) != 32 {
		t.Fatalf("JWKS x = %v (%v), want 32 bytes", jwk["x"], err)
	}
	y, err := base64.RawURLEncoding.DecodeString(jwk["y"].(string))
	if err != nil || len(y) != 32 {
		t.Fatalf("JWKS y = %v (%v), want 32 bytes", jwk["y"], err)
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	segs := strings.Split(idToken, ".")
	sig, err := base64.RawURLEncoding.DecodeString(segs[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("id_token signature = %d bytes (%v), want 64 raw r||s", len(sig), err)
	}
	digest := sha256.Sum256([]byte(segs[0] + "." + segs[1]))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("id_token ES256 signature did not verify against the served JWKS key")
	}

	// ===== auth codes are single-use and client_secrets are verified cryptographically =====
	replay := f.call("on_token", "POST", "/auth/token", nil, map[string]string{
		"grant_type": "authorization_code", "code": code,
		"client_id": siwaVMClientID, "client_secret": secret, "redirect_uri": siwaVMRedirectURI,
	})
	if replay.Status != 400 || replay.Body["error"] != "invalid_grant" {
		t.Fatalf("replay auth code -> %d %v, want 400 invalid_grant", replay.Status, replay.Body)
	}
	// A non-JWT secret, an expired secret, a forged signature and a wrong aud
	// are all invalid_client (the secret check precedes the code lookup, so
	// the consumed code still exercises it).
	for _, tc := range []struct {
		name   string
		secret string
	}{
		{"not a jwt", "not-a-jwt"},
		{"expired", siwaVMSecret(t, priv, base.Unix(), -60, "https://appleid.apple.com")},
		{"forged key", siwaVMSecret(t, mustRandomKey(t), base.Unix(), 3600, "https://appleid.apple.com")},
		{"wrong aud", siwaVMSecret(t, priv, base.Unix(), 3600, "https://someone-else.test")},
	} {
		r := f.call("on_token", "POST", "/auth/token", nil, map[string]string{
			"grant_type": "authorization_code", "code": code,
			"client_id": siwaVMClientID, "client_secret": tc.secret,
		})
		if r.Status != 400 || r.Body["error"] != "invalid_client" {
			t.Fatalf("%s client_secret -> %d %v, want 400 invalid_client", tc.name, r.Status, r.Body)
		}
	}
	// A code from a different client_id does not exchange.
	otherCode := siwaVMCode(t, f, "other")
	if r := f.call("on_token", "POST", "/auth/token", nil, map[string]string{
		"grant_type": "authorization_code", "code": otherCode,
		"client_id": "com.someone.else", "client_secret": secret,
	}); r.Status != 400 || r.Body["error"] != "invalid_grant" {
		t.Fatalf("client_id mismatch -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}

	// ===== the refresh grant rotates access tokens and rejects stale or foreign inputs =====
	refreshed := f.call("on_token", "POST", "/auth/token", nil, map[string]string{
		"grant_type": "refresh_token", "refresh_token": refresh,
		"client_id": siwaVMClientID, "client_secret": secret,
	})
	if refreshed.Status != 200 {
		t.Fatalf("refresh -> %d: %v", refreshed.Status, refreshed.Body)
	}
	newAccess, _ := refreshed.Body["access_token"].(string)
	if newAccess == "" || newAccess == access {
		t.Fatalf("refresh access_token = %q, want rotated from %q", newAccess, access)
	}
	if refreshed.Body["token_type"] != "Bearer" || ckVMNum(refreshed.Body["expires_in"]) != 3600 {
		t.Fatalf("refresh envelope = %v, want Bearer/3600", refreshed.Body)
	}
	// An access token is not a refresh token; a foreign client_id is rejected.
	if r := f.call("on_token", "POST", "/auth/token", nil, map[string]string{
		"grant_type": "refresh_token", "refresh_token": access,
		"client_id": siwaVMClientID, "client_secret": secret,
	}); r.Status != 400 || r.Body["error"] != "invalid_grant" {
		t.Fatalf("refresh with access token -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}
	if r := f.call("on_token", "POST", "/auth/token", nil, map[string]string{
		"grant_type": "refresh_token", "refresh_token": refresh,
		"client_id": "com.someone.else", "client_secret": secret,
	}); r.Status != 400 || r.Body["error"] != "invalid_grant" {
		t.Fatalf("refresh client_id mismatch -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}
	// Unknown grants are refused before anything else.
	if r := f.call("on_token", "POST", "/auth/token", nil, map[string]string{
		"grant_type": "password", "client_id": siwaVMClientID, "client_secret": secret,
	}); r.Status != 400 || r.Body["error"] != "unsupported_grant_type" {
		t.Fatalf("grant_type=password -> %d %v, want 400 unsupported_grant_type", r.Status, r.Body)
	}
}

// mustRandomKey generates an unregistered P-256 key (for forged-signature
// cases: well-formed ES256 JWTs that must NOT verify).
func mustRandomKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}
