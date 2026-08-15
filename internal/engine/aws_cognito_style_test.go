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

// TestAWSCognitoStyleAdapter exercises the aws-cognito-style adapter
// end-to-end through both the hosted-UI OAuth flow and the service API:
//
// Hosted UI:
//   - GET /oauth2/authorize → 302 with code+state
//   - POST /oauth2/token (authorization_code) → {access_token, id_token, refresh_token}
//   - GET /oauth2/userInfo (Bearer) → {sub, username, email}
//   - GET /oauth2/userInfo without auth → 401
//
// Service API:
//   - SignUp → {UserSub, UserConfirmed}
//   - InitiateAuth (USER_PASSWORD_AUTH) → {AuthenticationResult:{AccessToken, IdToken, RefreshToken}}
//   - InitiateAuth wrong password → NotAuthorizedException
//   - GetUser (AccessToken) → {Username, UserAttributes}
//   - ListUsers → {Users:[...]}
//
// Error shapes use Cognito's __type envelope.
func TestAWSCognitoStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "aws-cognito-style")
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
			"cognito": {Adapter: absAdapterDir},
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

	base := addrs["cognito"]

	const redirectURI = "http://localhost:3000/callback"
	const state = "cognito-state-123"
	const clientID = "test-client-id"

	// ===== Hosted UI: authorize → 302 with code+state =====

	resp := cognitoGetNoRedirect(t, base+"/oauth2/authorize?"+
		"client_id="+clientID+
		"&redirect_uri="+url.QueryEscape(redirectURI)+
		"&response_type=code"+
		"&scope=openid+email+profile"+
		"&state="+state)
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> status %d, want 302", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("authorize: missing Location header")
	}
	authCode := cognitoExtractParam(location, "code")
	if authCode == "" {
		t.Fatalf("authorize: no code in Location %q", location)
	}
	if cognitoExtractParam(location, "state") != state {
		t.Fatalf("authorize: state mismatch in Location %q", location)
	}

	// ===== Hosted UI: token exchange (authorization_code) =====

	body, status := cognitoPostForm(t, base+"/oauth2/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"client_secret": {"test-client-secret"},
	})
	if status != 200 {
		t.Fatalf("token exchange -> status %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token response: %v (body %s)", err, body)
	}
	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty string", tokenResp["access_token"])
	}
	idToken, ok := tokenResp["id_token"].(string)
	if !ok || idToken == "" {
		t.Fatalf("id_token = %v, want non-empty string", tokenResp["id_token"])
	}
	refreshToken, ok := tokenResp["refresh_token"].(string)
	if !ok || refreshToken == "" {
		t.Fatalf("refresh_token = %v, want non-empty string", tokenResp["refresh_token"])
	}
	if tokenResp["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", tokenResp["token_type"])
	}
	if tokenResp["expires_in"] != float64(3600) {
		t.Fatalf("expires_in = %v, want 3600", tokenResp["expires_in"])
	}
	// JWT shape check: three dot-separated segments.
	if strings.Count(accessToken, ".") != 2 {
		t.Fatalf("access_token is not JWT-shaped (expected 3 segments): %s", accessToken)
	}
	if strings.Count(idToken, ".") != 2 {
		t.Fatalf("id_token is not JWT-shaped (expected 3 segments): %s", idToken)
	}

	// ===== JWKS: tokens are real RS256 JWTs verifiable against the served key =====

	jwksBody, jwksStatus := cognitoGetNoAuth(t, base+"/mock-user-pool/.well-known/jwks.json")
	if jwksStatus != 200 {
		t.Fatalf("GET jwks -> status %d, want 200; body %s", jwksStatus, jwksBody)
	}
	var jwks struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal([]byte(jwksBody), &jwks); err != nil {
		t.Fatalf("unmarshal jwks: %v (body %s)", err, jwksBody)
	}
	if len(jwks.Keys) != 1 {
		t.Fatalf("jwks keys = %d, want 1", len(jwks.Keys))
	}
	jwk := jwks.Keys[0]
	if jwk["kty"] != "RSA" || jwk["alg"] != "RS256" || jwk["use"] != "sig" {
		t.Fatalf("jwks key = %v, want RSA/RS256/sig", jwk)
	}
	cognitoVerifyRS256(t, accessToken, jwk, clientID)
	cognitoVerifyRS256(t, idToken, jwk, clientID)

	// Header must carry the JWKS kid.
	hdrBytes, err := base64.RawURLEncoding.DecodeString(strings.Split(accessToken, ".")[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var hdr map[string]any
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if hdr["kid"] != jwk["kid"] {
		t.Fatalf("header kid = %v, want %v", hdr["kid"], jwk["kid"])
	}

	// ===== userInfo with a tampered signature → 401 =====

	tampered := cognitoTamperToken(accessToken)
	body, status = cognitoGetBearer(t, base+"/oauth2/userInfo", tampered)
	if status != 401 {
		t.Fatalf("userInfo with tampered token -> status %d, want 401; body %s", status, body)
	}

	// ===== GetUser with an expired token → NotAuthorizedException =====

	expiredTok := cognitoMintExpiredToken(t, clientID)
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.GetUser",
		map[string]any{"AccessToken": expiredTok})
	if status != 400 {
		t.Fatalf("GetUser with expired token -> status %d, want 400; body %s", status, body)
	}
	var expiredErr map[string]any
	if err := json.Unmarshal([]byte(body), &expiredErr); err != nil {
		t.Fatalf("unmarshal expired error: %v", err)
	}
	if expiredErr["__type"] != "NotAuthorizedException" {
		t.Fatalf("expired token __type = %v, want NotAuthorizedException", expiredErr["__type"])
	}

	// ===== Hosted UI: userInfo (Bearer) =====

	body, status = cognitoGetBearer(t, base+"/oauth2/userInfo", accessToken)
	if status != 200 {
		t.Fatalf("userInfo -> status %d, want 200; body %s", status, body)
	}
	var userInfo map[string]any
	if err := json.Unmarshal([]byte(body), &userInfo); err != nil {
		t.Fatalf("unmarshal userInfo: %v (body %s)", err, body)
	}
	if _, ok := userInfo["sub"].(string); !ok {
		t.Fatalf("userInfo.sub = %v, want string", userInfo["sub"])
	}
	if _, ok := userInfo["username"].(string); !ok {
		t.Fatalf("userInfo.username = %v, want string", userInfo["username"])
	}
	if _, ok := userInfo["email"].(string); !ok {
		t.Fatalf("userInfo.email = %v, want string", userInfo["email"])
	}

	// ===== Hosted UI: userInfo without auth → 401 =====

	body, status = cognitoGetNoAuth(t, base+"/oauth2/userInfo")
	if status != 401 {
		t.Fatalf("userInfo without auth -> status %d, want 401; body %s", status, body)
	}

	// ===== Service API: SignUp =====

	const signUpUser = "service-api-user"
	const signUpPassword = "SecurePass123!"
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.SignUp",
		map[string]any{
			"ClientId": clientID,
			"Username": signUpUser,
			"Password": signUpPassword,
			"UserAttributes": []any{
				map[string]any{"Name": "email", "Value": "service-api@mock-cognito.com"},
			},
		})
	if status != 200 {
		t.Fatalf("SignUp -> status %d, want 200; body %s", status, body)
	}
	var signUpResp map[string]any
	if err := json.Unmarshal([]byte(body), &signUpResp); err != nil {
		t.Fatalf("unmarshal SignUp: %v (body %s)", err, body)
	}
	userSub, ok := signUpResp["UserSub"].(string)
	if !ok || userSub == "" {
		t.Fatalf("UserSub = %v, want non-empty string", signUpResp["UserSub"])
	}
	if _, ok := signUpResp["UserConfirmed"].(bool); !ok {
		t.Fatalf("UserConfirmed = %v, want bool", signUpResp["UserConfirmed"])
	}

	// ===== Service API: ConfirmSignUp =====

	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.ConfirmSignUp",
		map[string]any{
			"ClientId":         clientID,
			"Username":         signUpUser,
			"ConfirmationCode": "000000",
		})
	if status != 200 {
		t.Fatalf("ConfirmSignUp -> status %d, want 200; body %s", status, body)
	}

	// ===== Service API: InitiateAuth (USER_PASSWORD_AUTH) =====

	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.InitiateAuth",
		map[string]any{
			"AuthFlow": "USER_PASSWORD_AUTH",
			"AuthParameters": map[string]any{
				"USERNAME": signUpUser,
				"PASSWORD": signUpPassword,
			},
			"ClientId": clientID,
		})
	if status != 200 {
		t.Fatalf("InitiateAuth -> status %d, want 200; body %s", status, body)
	}
	var authResp map[string]any
	if err := json.Unmarshal([]byte(body), &authResp); err != nil {
		t.Fatalf("unmarshal InitiateAuth: %v (body %s)", err, body)
	}
	authResult, ok := authResp["AuthenticationResult"].(map[string]any)
	if !ok {
		t.Fatalf("AuthenticationResult = %v, want object", authResp["AuthenticationResult"])
	}
	svcAccessToken, ok := authResult["AccessToken"].(string)
	if !ok || svcAccessToken == "" {
		t.Fatalf("AccessToken = %v, want non-empty string", authResult["AccessToken"])
	}
	svcIDToken, ok := authResult["IdToken"].(string)
	if !ok || svcIDToken == "" {
		t.Fatalf("IdToken = %v, want non-empty string", authResult["IdToken"])
	}
	svcRefreshToken, ok := authResult["RefreshToken"].(string)
	if !ok || svcRefreshToken == "" {
		t.Fatalf("RefreshToken = %v, want non-empty string", authResult["RefreshToken"])
	}
	if authResult["ExpiresIn"] != float64(3600) {
		t.Fatalf("ExpiresIn = %v, want 3600", authResult["ExpiresIn"])
	}
	// JWT shape check.
	if strings.Count(svcAccessToken, ".") != 2 {
		t.Fatalf("service AccessToken is not JWT-shaped: %s", svcAccessToken)
	}

	// ===== Service API: InitiateAuth wrong password → NotAuthorizedException =====

	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.InitiateAuth",
		map[string]any{
			"AuthFlow": "USER_PASSWORD_AUTH",
			"AuthParameters": map[string]any{
				"USERNAME": signUpUser,
				"PASSWORD": "wrong-password",
			},
			"ClientId": clientID,
		})
	if status != 400 {
		t.Fatalf("InitiateAuth wrong password -> status %d, want 400", status)
	}
	var cognitoErr map[string]any
	if err := json.Unmarshal([]byte(body), &cognitoErr); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	errType, ok := cognitoErr["__type"].(string)
	if !ok || errType != "NotAuthorizedException" {
		t.Fatalf("__type = %v, want NotAuthorizedException", cognitoErr["__type"])
	}
	errMsg, ok := cognitoErr["message"].(string)
	if !ok || errMsg == "" {
		t.Fatalf("message = %v, want non-empty string", cognitoErr["message"])
	}

	// ===== Service API: GetUser (AccessToken) =====

	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.GetUser",
		map[string]any{
			"AccessToken": svcAccessToken,
		})
	if status != 200 {
		t.Fatalf("GetUser -> status %d, want 200; body %s", status, body)
	}
	var getUserResp map[string]any
	if err := json.Unmarshal([]byte(body), &getUserResp); err != nil {
		t.Fatalf("unmarshal GetUser: %v (body %s)", err, body)
	}
	if getUserResp["Username"] != signUpUser {
		t.Fatalf("GetUser Username = %v, want %v", getUserResp["Username"], signUpUser)
	}
	userAttrs, ok := getUserResp["UserAttributes"].([]any)
	if !ok || len(userAttrs) < 1 {
		t.Fatalf("UserAttributes = %v, want non-empty array", getUserResp["UserAttributes"])
	}

	// ===== Service API: ListUsers =====

	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.ListUsers",
		map[string]any{
			"UserPoolId": "us-east-1_mock",
		})
	if status != 200 {
		t.Fatalf("ListUsers -> status %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal ListUsers: %v", err)
	}
	users, ok := listResp["Users"].([]any)
	if !ok || len(users) < 1 {
		t.Fatalf("Users = %v, want non-empty array", listResp["Users"])
	}

	// ===== Hosted UI: refresh_token grant =====

	body, status = cognitoPostForm(t, base+"/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {svcRefreshToken},
		"client_id":     {clientID},
	})
	if status != 200 {
		t.Fatalf("refresh token -> status %d, want 200; body %s", status, body)
	}
	var refreshResp map[string]any
	if err := json.Unmarshal([]byte(body), &refreshResp); err != nil {
		t.Fatalf("unmarshal refresh: %v", err)
	}
	newAccess, ok := refreshResp["access_token"].(string)
	if !ok || newAccess == "" {
		t.Fatalf("refreshed access_token = %v, want non-empty", refreshResp["access_token"])
	}
	if newAccess == accessToken || newAccess == svcAccessToken {
		t.Fatal("refresh: access token did not change")
	}

	// ===== Hosted UI: logout → 302 =====

	resp = cognitoGetNoRedirect(t, base+"/logout?logout_uri="+url.QueryEscape(redirectURI))
	if resp.StatusCode != 302 {
		t.Fatalf("logout -> status %d, want 302", resp.StatusCode)
	}

	// ===== Service API: GetId (identity pool) =====

	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityService.GetId",
		map[string]any{
			"IdentityPoolId": "us-east-1:mock-pool",
		})
	if status != 200 {
		t.Fatalf("GetId -> status %d, want 200; body %s", status, body)
	}
	var getIdResp map[string]any
	if err := json.Unmarshal([]byte(body), &getIdResp); err != nil {
		t.Fatalf("unmarshal GetId: %v", err)
	}
	identityID, ok := getIdResp["IdentityId"].(string)
	if !ok || identityID == "" {
		t.Fatalf("IdentityId = %v, want non-empty string", getIdResp["IdentityId"])
	}

	// ===== Service API: GetCredentialsForIdentity =====

	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityService.GetCredentialsForIdentity",
		map[string]any{
			"IdentityId": identityID,
		})
	if status != 200 {
		t.Fatalf("GetCredentialsForIdentity -> status %d, want 200; body %s", status, body)
	}
	var credsResp map[string]any
	if err := json.Unmarshal([]byte(body), &credsResp); err != nil {
		t.Fatalf("unmarshal GetCredentials: %v", err)
	}
	creds, ok := credsResp["Credentials"].(map[string]any)
	if !ok {
		t.Fatalf("Credentials = %v, want object", credsResp["Credentials"])
	}
	if _, ok := creds["AccessKeyId"].(string); !ok {
		t.Fatalf("AccessKeyId = %v, want string", creds["AccessKeyId"])
	}
	if _, ok := creds["SecretKey"].(string); !ok {
		t.Fatalf("SecretKey = %v, want string", creds["SecretKey"])
	}
}

// === AWS Cognito test helpers ===

// cognitoPrivateKeyPEM mirrors the adapter's documented fixed synthetic
// RSA-2048 keypair (see adapters/aws-cognito-style/scripts/lib.star): the
// public half is served at /{userPoolId}/.well-known/jwks.json, the private
// half lets tests mint properly signed tokens (e.g. expired ones).
const cognitoPrivateKeyPEM = `-----BEGIN RSA PRIVATE KEY-----
MIIEogIBAAKCAQEAuvUH4Lt/lv6L2u2E9qg15ZelZ7Olpmr7j9RhTr+3VybvKml5
dntEUSXKO70WVkZ36rjMecbOVU2vrCyJTpIYejqTp1c7O/67S7sdJPdLrKdL55JC
9r24Zp5gjUKW8ZoVn325oOsRlO4vezgkS93mSFGuq7yyXe36SmG/xwkFcB6eu2W1
IhuoHQkK47ja5PS1GuhctOKkYi9lqJPQYri6H3E7Cz1UHlQmdiY4T5porO1B9Dfe
9P77g6zw8jwzhkce0ORrWyvAbA5BFN5SodXaBN3G4PhjdklDPn++AXYhhahWGZy8
yVt81e+ra+jjHB7yutT5xDYzXY4JLF1FKsk4ywIDAQABAoIBAEL9Z8w8AxTcsspI
j3s+fMl+1BLbiUCfVvKLnC52fcBpwAsHbjFpK+qTyuoq7+UMLQ3bF9GOzgI86vSb
pLuVl9W8RYoRtLTjqsMREflb7y63Z3hbrUjyZC/JEjmroaCCoLrcdvZVJKCj1Dmn
vUG+CjThp9/7pkIH8sZSTkCIV/17LW03z07t3UpwPPbcwGfRb1GfYKtwPpeNyxtB
UvEM36jeYInSE+IKsP8fN6E/c+0Qn6xwZJsCkcB+y/V/QXiuaMTi9MU+TGtQ03eh
u4DMmoO5ITheuGLOGIqkhZ+OtlBxDbTnQsN1mvQ9zUiVMGqhAB1tagXnhVDpxm7I
eonU/0kCgYEAwlMpItTeGMrC79tKQl/NlDyxg6o9aqFZ5uFn905yh5AcRY8rEMS9
068HEJxh0xtmlsBhpWlaROeltHfUoPDTgI1F+4LijhfsAjMTLkCLIlm01GtkDSDw
0ZV3cNDmZMgEjSC2E5qSxNaAREltsVnMjYt1yQbGjsWI6MPqtmB0Xl8CgYEA9ks/
Nv5a9XRKqz9xsFsCZlTvV3fj63jFVP31IZS9UkZxq0alssBItEhsDpa+0s61z0ko
0Ggwl9V2Wa4Y3/79igROU1MJPbxfC7HH9+KdgANKpO7p8EHC2YsmlT9tbn+/eSbl
EqhoeculELQQzn3xbd5u7WBJqOJg0LvVhKM+ZRUCgYA9NLhGMknqASM5LRbMpSQ5
Roya7en+ReftIp3+dQT50dg1yIxF8dHgdMaC4t6lAYJkhR+8W9yEy3mTyBJ+xpu3
Z8fdGjKFkt9RKgkmjknEfgDIzzJqOC/hs3Q1YnbO03krglwW/J6xxOYNnBsiuygE
hSKKOModefZPajXpT6QXfQKBgEaqyHSK/qY2u8Xu6jvjoQijjhjWuXqyqEv+ofsE
pl2ZALxYBOsI6NNxhC+baR0rWlcjcqZ5fpfSE6cfoNuEWlLjcWXPCXPBPLQqSmoB
h5dXWm+AbXcWJ0Yr+uIP1OJDnTixxEBaOb/YgoAMalYVJNSVYdaSLhBbA9RgUJ9C
B4ERAoGAScLBJMcOVkQ/ulGbJKyjqis2aaQ0VBWPdQQ+OjBrRcG4oj7TUztxjy4F
KTSEsIsbZa2cq+a7wCImzCPikv8teaTGoOVnU1rxN61LG2G4IDwFGoj9/br0rkM7
7BVbYZiX+Z5DxA8ec/9pYP2TdF69zr8xT8Mm48gC6I/kYeRcGfo=
-----END RSA PRIVATE KEY-----`

// cognitoVerifyRS256 fetches nothing: it verifies token against a JWK
// (reconstructed RSA public key) over header.payload, and checks the claim
// set (exp fresh, iss, token_use, aud/client_id binding).
func cognitoVerifyRS256(t *testing.T, token string, jwk map[string]any, clientID string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
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
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig); err != nil {
		t.Fatalf("RS256 signature did not verify against the JWKS key: %v", err)
	}
	var claims map[string]any
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if exp, ok := claims["exp"].(float64); !ok || exp <= float64(time.Now().Unix()) {
		t.Fatalf("exp = %v, want future timestamp", claims["exp"])
	}
	if claims["iss"] != "https://cognito-idp.mock-region.amazonaws.com/mock-user-pool" {
		t.Fatalf("iss = %v", claims["iss"])
	}
	if claims["token_use"] == "access" && claims["client_id"] != clientID {
		t.Fatalf("client_id = %v, want %v", claims["client_id"], clientID)
	}
	if claims["token_use"] == "id" && claims["aud"] != clientID {
		t.Fatalf("aud = %v, want %v", claims["aud"], clientID)
	}
}

// cognitoTamperToken flips the last character of the signature segment.
func cognitoTamperToken(token string) string {
	parts := strings.Split(token, ".")
	sig := parts[2]
	last := "A"
	if sig[len(sig)-1] == 'A' {
		last = "B"
	}
	parts[2] = sig[:len(sig)-1] + last
	return strings.Join(parts, ".")
}

// cognitoMintExpiredToken mints a properly signed RS256 access token whose
// exp is in the past — the signature verifies, only the expiry fails.
func cognitoMintExpiredToken(t *testing.T, clientID string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(cognitoPrivateKeyPEM))
	if block == nil {
		t.Fatal("bad test key PEM")
	}
	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	now := time.Now().Unix()
	header := `{"alg":"RS256","kid":"mock-cognito-key-1","typ":"JWT"}`
	payload := `{"sub":"00000000-0000-0000-0000-000001","iss":"https://cognito-idp.mock-region.amazonaws.com/mock-user-pool",` +
		`"client_id":"` + clientID + `","token_use":"access","username":"expired-user",` +
		`"iat":` + strconv.FormatInt(now-7200, 10) + `,"exp":` + strconv.FormatInt(now-3600, 10) + `}`
	h := base64.RawURLEncoding.EncodeToString([]byte(header))
	p := base64.RawURLEncoding.EncodeToString([]byte(payload))
	digest := sha256.Sum256([]byte(h + "." + p))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func cognitoGetNoRedirect(t *testing.T, rawurl string) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func cognitoGetNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func cognitoGetBearer(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func cognitoPostForm(t *testing.T, rawurl string, form url.Values) (string, int) {
	t.Helper()
	resp, err := http.PostForm(rawurl, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func cognitoPostTarget(t *testing.T, rawurl, target string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", target)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func cognitoExtractParam(rawurl, param string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return u.Query().Get(param)
}

// Guard: ensure we don't accidentally import strings without using it.
var _ = strings.Contains
