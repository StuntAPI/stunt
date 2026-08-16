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

// TestAWSCognitoAuthorizeCoherence pins the hosted-UI authorize binding:
// codes bind EXISTING users (the seeded demo-user by default, or the
// login_hint user when given) — never a fresh random user. Unknown
// login_hint users and unsupported response_types get OAuth error
// redirects, the way real Cognito rejects the flow.
func TestAWSCognitoAuthorizeCoherence(t *testing.T) {
	base := cognitoServe(t)
	const redirectURI = "http://localhost:3000/callback"
	const clientID = "coherence-client"

	authorizeURL := func(extra string) string {
		return base + "/oauth2/authorize?client_id=" + clientID +
			"&redirect_uri=" + url.QueryEscape(redirectURI) + extra
	}

	// Default (no login_hint): binds the seeded demo-user.
	resp := cognitoGetNoRedirect(t, authorizeURL("&response_type=code&state=s1"))
	if resp.StatusCode != 302 {
		t.Fatalf("authorize default -> status %d, want 302", resp.StatusCode)
	}
	code1 := cognitoExtractParam(resp.Header.Get("Location"), "code")
	if code1 == "" {
		t.Fatalf("authorize default: no code in %q", resp.Header.Get("Location"))
	}

	// login_hint binds an existing user: sign one up + confirm it first.
	const hintUser = "coherence-user"
	body, status := cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.SignUp",
		map[string]any{
			"ClientId": clientID,
			"Username": hintUser,
			"Password": "Coherence123!",
			"UserAttributes": []any{
				map[string]any{"Name": "email", "Value": "coherence@mock-cognito.com"},
			},
		})
	if status != 200 {
		t.Fatalf("SignUp -> status %d, want 200; body %s", status, body)
	}
	var signUpResp map[string]any
	if err := json.Unmarshal([]byte(body), &signUpResp); err != nil {
		t.Fatalf("unmarshal SignUp: %v (body %s)", err, body)
	}
	wantSub, _ := signUpResp["UserSub"].(string)
	if wantSub == "" {
		t.Fatalf("UserSub = %v, want non-empty", signUpResp["UserSub"])
	}
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.ConfirmSignUp",
		map[string]any{"ClientId": clientID, "Username": hintUser, "ConfirmationCode": "000000"})
	if status != 200 {
		t.Fatalf("ConfirmSignUp -> status %d, want 200; body %s", status, body)
	}

	resp = cognitoGetNoRedirect(t, authorizeURL(
		"&response_type=code&state=s2&login_hint="+hintUser))
	if resp.StatusCode != 302 {
		t.Fatalf("authorize login_hint -> status %d, want 302", resp.StatusCode)
	}
	code2 := cognitoExtractParam(resp.Header.Get("Location"), "code")
	if code2 == "" || code2 == code1 {
		t.Fatalf("authorize login_hint: code = %q, want fresh code", code2)
	}
	if cognitoExtractParam(resp.Header.Get("Location"), "state") != "s2" {
		t.Fatalf("authorize login_hint: state mismatch in %q", resp.Header.Get("Location"))
	}

	// userInfo from both codes: default → demo-user, login_hint → hintUser's sub.
	exchange := func(code string) map[string]any {
		t.Helper()
		body, status := cognitoPostForm(t, base+"/oauth2/token", url.Values{
			"grant_type":   {"authorization_code"},
			"code":         {code},
			"client_id":    {clientID},
			"redirect_uri": {redirectURI},
		})
		if status != 200 {
			t.Fatalf("token exchange -> status %d, want 200; body %s", status, body)
		}
		var tok map[string]any
		if err := json.Unmarshal([]byte(body), &tok); err != nil {
			t.Fatalf("unmarshal token: %v (body %s)", err, body)
		}
		uiBody, uiStatus := cognitoGetBearer(t, base+"/oauth2/userInfo", tok["access_token"].(string))
		if uiStatus != 200 {
			t.Fatalf("userInfo -> status %d, want 200; body %s", uiStatus, uiBody)
		}
		var ui map[string]any
		if err := json.Unmarshal([]byte(uiBody), &ui); err != nil {
			t.Fatalf("unmarshal userInfo: %v (body %s)", err, uiBody)
		}
		return ui
	}

	uiDefault := exchange(code1)
	if uiDefault["username"] != "demo-user" {
		t.Fatalf("default authorize bound username = %v, want demo-user", uiDefault["username"])
	}
	uiHint := exchange(code2)
	if uiHint["sub"] != wantSub {
		t.Fatalf("login_hint authorize sub = %v, want %v (the existing user's sub)", uiHint["sub"], wantSub)
	}
	if uiHint["email"] != "coherence@mock-cognito.com" {
		t.Fatalf("login_hint authorize email = %v, want the existing user's email", uiHint["email"])
	}

	// Unknown login_hint → OAuth error redirect (never a minted user).
	resp = cognitoGetNoRedirect(t, authorizeURL("&response_type=code&login_hint=ghost-user"))
	if resp.StatusCode != 302 {
		t.Fatalf("authorize ghost login_hint -> status %d, want 302", resp.StatusCode)
	}
	if got := cognitoExtractParam(resp.Header.Get("Location"), "error"); got != "invalid_request" {
		t.Fatalf("ghost login_hint error = %q, want invalid_request (Location %q)", got, resp.Header.Get("Location"))
	}

	// Unsupported response_type → unsupported_response_type redirect.
	resp = cognitoGetNoRedirect(t, authorizeURL("&response_type=token"))
	if resp.StatusCode != 302 {
		t.Fatalf("authorize response_type=token -> status %d, want 302", resp.StatusCode)
	}
	if got := cognitoExtractParam(resp.Header.Get("Location"), "error"); got != "unsupported_response_type" {
		t.Fatalf("response_type=token error = %q, want unsupported_response_type (Location %q)", got, resp.Header.Get("Location"))
	}
}

// TestAWSCognitoRefreshGrants exercises REFRESH_TOKEN_AUTH /
// REFRESH_TOKEN_REFRESH and the hosted-UI refresh_token grant with real
// Cognito default semantics: refresh tokens are REUSABLE, access/id tokens
// rotate, and no new refresh token is returned. GlobalSignOut then revokes
// everything.
func TestAWSCognitoRefreshGrants(t *testing.T) {
	base := cognitoServe(t)
	const clientID = "refresh-client"

	// Password login as the seeded demo-user.
	body, status := cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.InitiateAuth",
		map[string]any{
			"AuthFlow": "USER_PASSWORD_AUTH",
			"AuthParameters": map[string]any{
				"USERNAME": "demo-user",
				"PASSWORD": "DemoPass123!",
			},
			"ClientId": clientID,
		})
	if status != 200 {
		t.Fatalf("InitiateAuth demo-user -> status %d, want 200; body %s", status, body)
	}
	var authResp map[string]any
	if err := json.Unmarshal([]byte(body), &authResp); err != nil {
		t.Fatalf("unmarshal InitiateAuth: %v (body %s)", err, body)
	}
	result := authResp["AuthenticationResult"].(map[string]any)
	refreshToken := result["RefreshToken"].(string)
	firstAccess := result["AccessToken"].(string)

	// REFRESH_TOKEN_AUTH: fresh access+id pair, NO RefreshToken in the
	// response (the presented one stays valid — real Cognito default).
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.InitiateAuth",
		map[string]any{
			"AuthFlow": "REFRESH_TOKEN_AUTH",
			"AuthParameters": map[string]any{
				"REFRESH_TOKEN": refreshToken,
			},
			"ClientId": clientID,
		})
	if status != 200 {
		t.Fatalf("InitiateAuth REFRESH_TOKEN_AUTH -> status %d, want 200; body %s", status, body)
	}
	var refreshAuth map[string]any
	if err := json.Unmarshal([]byte(body), &refreshAuth); err != nil {
		t.Fatalf("unmarshal REFRESH_TOKEN_AUTH: %v (body %s)", err, body)
	}
	rr := refreshAuth["AuthenticationResult"].(map[string]any)
	secondAccess := rr["AccessToken"].(string)
	if secondAccess == "" || secondAccess == firstAccess {
		t.Fatalf("REFRESH_TOKEN_AUTH AccessToken = %q, want a fresh token", secondAccess)
	}
	if _, ok := rr["RefreshToken"]; ok {
		t.Fatalf("REFRESH_TOKEN_AUTH returned a RefreshToken (%v), want none — refresh tokens are reusable, not rotated", rr["RefreshToken"])
	}
	if _, ok := rr["IdToken"].(string); !ok {
		t.Fatalf("REFRESH_TOKEN_AUTH IdToken = %v, want string", rr["IdToken"])
	}

	// The SAME refresh token works again via REFRESH_TOKEN_REFRESH...
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.InitiateAuth",
		map[string]any{
			"AuthFlow": "REFRESH_TOKEN_REFRESH",
			"AuthParameters": map[string]any{
				"REFRESH_TOKEN": refreshToken,
			},
			"ClientId": clientID,
		})
	if status != 200 {
		t.Fatalf("InitiateAuth REFRESH_TOKEN_REFRESH -> status %d, want 200; body %s", status, body)
	}

	// ...and again via the hosted-UI refresh_token grant (no refresh_token
	// field in the OAuth response either).
	body, status = cognitoPostForm(t, base+"/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	})
	if status != 200 {
		t.Fatalf("hosted-UI refresh grant -> status %d, want 200; body %s", status, body)
	}
	var oauthRefresh map[string]any
	if err := json.Unmarshal([]byte(body), &oauthRefresh); err != nil {
		t.Fatalf("unmarshal refresh grant: %v (body %s)", err, body)
	}
	thirdAccess := oauthRefresh["access_token"].(string)
	if thirdAccess == "" || thirdAccess == firstAccess || thirdAccess == secondAccess {
		t.Fatalf("refresh grant access_token = %q, want a fresh token", thirdAccess)
	}
	if _, ok := oauthRefresh["refresh_token"]; ok {
		t.Fatalf("refresh grant returned a refresh_token (%v), want none — refresh tokens are reusable, not rotated", oauthRefresh["refresh_token"])
	}
	if _, ok := oauthRefresh["id_token"].(string); !ok {
		t.Fatalf("refresh grant id_token = %v, want string", oauthRefresh["id_token"])
	}

	// The refreshed access token is a real RS256 JWT that GetUser accepts.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.GetUser",
		map[string]any{"AccessToken": thirdAccess})
	if status != 200 {
		t.Fatalf("GetUser with refreshed token -> status %d, want 200; body %s", status, body)
	}

	// GlobalSignOut revokes everything: access token, then refresh token.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.GlobalSignOut",
		map[string]any{"AccessToken": thirdAccess})
	if status != 200 {
		t.Fatalf("GlobalSignOut -> status %d, want 200; body %s", status, body)
	}

	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.GetUser",
		map[string]any{"AccessToken": thirdAccess})
	if status != 400 {
		t.Fatalf("GetUser after GlobalSignOut -> status %d, want 400", status)
	}
	cognitoAssertErrType(t, body, "NotAuthorizedException")

	uiBody, uiStatus := cognitoGetBearer(t, base+"/oauth2/userInfo", thirdAccess)
	if uiStatus != 401 {
		t.Fatalf("userInfo after GlobalSignOut -> status %d, want 401; body %s", uiStatus, uiBody)
	}

	body, status = cognitoPostForm(t, base+"/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	})
	if status != 400 {
		t.Fatalf("refresh grant after GlobalSignOut -> status %d, want 400; body %s", status, body)
	}
	var grantErr map[string]any
	if err := json.Unmarshal([]byte(body), &grantErr); err != nil {
		t.Fatalf("unmarshal revoked refresh error: %v (body %s)", err, body)
	}
	if grantErr["error"] != "invalid_grant" {
		t.Fatalf("revoked refresh error = %v, want invalid_grant", grantErr["error"])
	}

	// Unknown refresh tokens are invalid_grant from the start.
	body, status = cognitoPostForm(t, base+"/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"mock-refresh-token-nope"},
		"client_id":     {clientID},
	})
	if status != 400 {
		t.Fatalf("unknown refresh token -> status %d, want 400; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &grantErr); err != nil {
		t.Fatalf("unmarshal unknown refresh error: %v (body %s)", err, body)
	}
	if grantErr["error"] != "invalid_grant" {
		t.Fatalf("unknown refresh error = %v, want invalid_grant", grantErr["error"])
	}
}

// TestAWSCognitoNewPasswordChallenge walks the NEW_PASSWORD_REQUIRED
// challenge: AdminCreateUser lands the user in FORCE_CHANGE_PASSWORD, the
// first password login returns the challenge + session (no tokens), the
// response sets a policy-compliant password and mints tokens, and the
// session is single-use.
func TestAWSCognitoNewPasswordChallenge(t *testing.T) {
	base := cognitoServe(t)
	const clientID = "challenge-client"
	const challengeUser = "challenge-user"
	const tempPass = "TempPass7B!"
	const newPass = "Rotated1Pass!"

	body, status := cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.AdminCreateUser",
		map[string]any{
			"UserPoolId":        "us-east-1_mock",
			"Username":          challengeUser,
			"TemporaryPassword": tempPass,
			"UserAttributes": []any{
				map[string]any{"Name": "email", "Value": "challenge@mock-cognito.com"},
			},
		})
	if status != 200 {
		t.Fatalf("AdminCreateUser -> status %d, want 200; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal AdminCreateUser: %v (body %s)", err, body)
	}
	created := createResp["User"].(map[string]any)
	if created["UserStatus"] != "FORCE_CHANGE_PASSWORD" {
		t.Fatalf("AdminCreateUser UserStatus = %v, want FORCE_CHANGE_PASSWORD", created["UserStatus"])
	}

	// First login with the temporary password: challenge, not tokens.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.InitiateAuth",
		map[string]any{
			"AuthFlow": "USER_PASSWORD_AUTH",
			"AuthParameters": map[string]any{
				"USERNAME": challengeUser,
				"PASSWORD": tempPass,
			},
			"ClientId": clientID,
		})
	if status != 200 {
		t.Fatalf("InitiateAuth temp password -> status %d, want 200; body %s", status, body)
	}
	var challengeResp map[string]any
	if err := json.Unmarshal([]byte(body), &challengeResp); err != nil {
		t.Fatalf("unmarshal challenge response: %v (body %s)", err, body)
	}
	if challengeResp["ChallengeName"] != "NEW_PASSWORD_REQUIRED" {
		t.Fatalf("ChallengeName = %v, want NEW_PASSWORD_REQUIRED (body %s)", challengeResp["ChallengeName"], body)
	}
	if _, ok := challengeResp["AuthenticationResult"]; ok {
		t.Fatalf("challenge response carried AuthenticationResult = %v, want none until the challenge is answered", challengeResp["AuthenticationResult"])
	}
	session, ok := challengeResp["Session"].(string)
	if !ok || session == "" {
		t.Fatalf("Session = %v, want non-empty string", challengeResp["Session"])
	}
	params := challengeResp["ChallengeParameters"].(map[string]any)
	if params["USER_ID_FOR_SRP"] != challengeUser {
		t.Fatalf("USER_ID_FOR_SRP = %v, want %q", params["USER_ID_FOR_SRP"], challengeUser)
	}

	// Wrong temp password → generic NotAuthorizedException.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.InitiateAuth",
		map[string]any{
			"AuthFlow": "USER_PASSWORD_AUTH",
			"AuthParameters": map[string]any{
				"USERNAME": challengeUser,
				"PASSWORD": "wrong-temp-password",
			},
			"ClientId": clientID,
		})
	if status != 400 {
		t.Fatalf("InitiateAuth wrong temp password -> status %d, want 400", status)
	}
	cognitoAssertErrType(t, body, "NotAuthorizedException")

	// Weak new password → InvalidPasswordException (session stays usable).
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.RespondToAuthChallenge",
		map[string]any{
			"ChallengeName": "NEW_PASSWORD_REQUIRED",
			"Session":       session,
			"ClientId":      clientID,
			"ChallengeResponses": map[string]any{
				"USERNAME":     challengeUser,
				"NEW_PASSWORD": "short",
			},
		})
	if status != 400 {
		t.Fatalf("RespondToAuthChallenge weak password -> status %d, want 400", status)
	}
	cognitoAssertErrType(t, body, "InvalidPasswordException")

	// Policy-compliant password → tokens, user confirmed.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.RespondToAuthChallenge",
		map[string]any{
			"ChallengeName": "NEW_PASSWORD_REQUIRED",
			"Session":       session,
			"ClientId":      clientID,
			"ChallengeResponses": map[string]any{
				"USERNAME":     challengeUser,
				"NEW_PASSWORD": newPass,
			},
		})
	if status != 200 {
		t.Fatalf("RespondToAuthChallenge -> status %d, want 200; body %s", status, body)
	}
	var answered map[string]any
	if err := json.Unmarshal([]byte(body), &answered); err != nil {
		t.Fatalf("unmarshal challenge answer: %v (body %s)", err, body)
	}
	finalResult := answered["AuthenticationResult"].(map[string]any)
	newAccess := finalResult["AccessToken"].(string)
	if newAccess == "" {
		t.Fatalf("AccessToken after challenge = %v, want non-empty", finalResult["AccessToken"])
	}

	// The session is single-use: replaying it fails.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.RespondToAuthChallenge",
		map[string]any{
			"ChallengeName": "NEW_PASSWORD_REQUIRED",
			"Session":       session,
			"ClientId":      clientID,
			"ChallengeResponses": map[string]any{
				"USERNAME":     challengeUser,
				"NEW_PASSWORD": newPass,
			},
		})
	if status != 400 {
		t.Fatalf("replayed session -> status %d, want 400", status)
	}
	cognitoAssertErrType(t, body, "NotAuthorizedException")

	// Old temporary password no longer works; the new one does.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.InitiateAuth",
		map[string]any{
			"AuthFlow": "USER_PASSWORD_AUTH",
			"AuthParameters": map[string]any{
				"USERNAME": challengeUser,
				"PASSWORD": tempPass,
			},
			"ClientId": clientID,
		})
	if status != 400 {
		t.Fatalf("InitiateAuth with consumed temp password -> status %d, want 400", status)
	}
	cognitoAssertErrType(t, body, "NotAuthorizedException")

	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.InitiateAuth",
		map[string]any{
			"AuthFlow": "USER_PASSWORD_AUTH",
			"AuthParameters": map[string]any{
				"USERNAME": challengeUser,
				"PASSWORD": newPass,
			},
			"ClientId": clientID,
		})
	if status != 200 {
		t.Fatalf("InitiateAuth with new password -> status %d, want 200; body %s", status, body)
	}
}

// TestAWSCognitoAdminInitiateAuth covers the ADMIN_USER_PASSWORD_AUTH
// subset of AdminInitiateAuth against the seeded force-change user, plus
// AdminRespondToAuthChallenge, and the flow-separation error paths.
func TestAWSCognitoAdminInitiateAuth(t *testing.T) {
	base := cognitoServe(t)
	const clientID = "admin-client"
	const newUser = "Changed1Pass!"

	// The seeded force-change user challenges on first admin login.
	body, status := cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.AdminInitiateAuth",
		map[string]any{
			"UserPoolId": "us-east-1_mock",
			"AuthFlow":   "ADMIN_USER_PASSWORD_AUTH",
			"AuthParameters": map[string]any{
				"USERNAME": "force-change-user",
				"PASSWORD": "TempPass1A!",
			},
			"ClientId": clientID,
		})
	if status != 200 {
		t.Fatalf("AdminInitiateAuth -> status %d, want 200; body %s", status, body)
	}
	var challenge map[string]any
	if err := json.Unmarshal([]byte(body), &challenge); err != nil {
		t.Fatalf("unmarshal AdminInitiateAuth: %v (body %s)", err, body)
	}
	if challenge["ChallengeName"] != "NEW_PASSWORD_REQUIRED" {
		t.Fatalf("ChallengeName = %v, want NEW_PASSWORD_REQUIRED", challenge["ChallengeName"])
	}
	session := challenge["Session"].(string)

	// AdminRespondToAuthChallenge completes the flow with tokens.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.AdminRespondToAuthChallenge",
		map[string]any{
			"UserPoolId":    "us-east-1_mock",
			"ChallengeName": "NEW_PASSWORD_REQUIRED",
			"Session":       session,
			"ClientId":      clientID,
			"ChallengeResponses": map[string]any{
				"USERNAME":     "force-change-user",
				"NEW_PASSWORD": newUser,
			},
		})
	if status != 200 {
		t.Fatalf("AdminRespondToAuthChallenge -> status %d, want 200; body %s", status, body)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("unmarshal AdminRespondToAuthChallenge: %v (body %s)", err, body)
	}
	adminAccess := result["AuthenticationResult"].(map[string]any)["AccessToken"].(string)
	if adminAccess == "" {
		t.Fatal("AdminRespondToAuthChallenge returned no AccessToken")
	}

	// The new password now works through the plain user flow too.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.InitiateAuth",
		map[string]any{
			"AuthFlow": "USER_PASSWORD_AUTH",
			"AuthParameters": map[string]any{
				"USERNAME": "force-change-user",
				"PASSWORD": newUser,
			},
			"ClientId": clientID,
		})
	if status != 200 {
		t.Fatalf("InitiateAuth after admin challenge -> status %d, want 200; body %s", status, body)
	}

	// Flow separation: InitiateAuth rejects the ADMIN_* flows.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.InitiateAuth",
		map[string]any{
			"AuthFlow": "ADMIN_USER_PASSWORD_AUTH",
			"AuthParameters": map[string]any{
				"USERNAME": "force-change-user",
				"PASSWORD": newUser,
			},
			"ClientId": clientID,
		})
	if status != 400 {
		t.Fatalf("InitiateAuth with ADMIN flow -> status %d, want 400", status)
	}
	cognitoAssertErrType(t, body, "InvalidParameterException")
}

// TestAWSCognitoForgotPassword walks the forgot-password round trip with the
// deterministic code convention (last 6 digits of the username, zero-padded
// — "forgot-user-42" → "000042"), including the InvalidPasswordException
// and CodeMismatchException failure paths.
func TestAWSCognitoForgotPassword(t *testing.T) {
	base := cognitoServe(t)
	const clientID = "forgot-client"
	const forgotUser = "forgot-user-42"
	const code = "000042" // digits of "forgot-user-42", zero-padded to 6

	body, status := cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.SignUp",
		map[string]any{
			"ClientId": clientID,
			"Username": forgotUser,
			"Password": "OldPass123!",
		})
	if status != 200 {
		t.Fatalf("SignUp -> status %d, want 200; body %s", status, body)
	}

	// ForgotPassword → delivery details.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.ForgotPassword",
		map[string]any{"ClientId": clientID, "Username": forgotUser})
	if status != 200 {
		t.Fatalf("ForgotPassword -> status %d, want 200; body %s", status, body)
	}
	var fpResp map[string]any
	if err := json.Unmarshal([]byte(body), &fpResp); err != nil {
		t.Fatalf("unmarshal ForgotPassword: %v (body %s)", err, body)
	}
	delivery, ok := fpResp["CodeDeliveryDetails"].(map[string]any)
	if !ok {
		t.Fatalf("CodeDeliveryDetails = %v, want object", fpResp["CodeDeliveryDetails"])
	}
	if dest, _ := delivery["Destination"].(string); dest != "forgot-user-42@mock-cognito.com" {
		t.Fatalf("Destination = %v, want the user's email", delivery["Destination"])
	}

	// Unknown user → UserNotFoundException.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.ForgotPassword",
		map[string]any{"ClientId": clientID, "Username": "ghost-user"})
	if status != 400 {
		t.Fatalf("ForgotPassword unknown user -> status %d, want 400", status)
	}
	cognitoAssertErrType(t, body, "UserNotFoundException")

	// Wrong code → CodeMismatchException.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.ConfirmForgotPassword",
		map[string]any{
			"ClientId":         clientID,
			"Username":         forgotUser,
			"ConfirmationCode": "999999",
			"Password":         "NewPass1234!",
		})
	if status != 400 {
		t.Fatalf("ConfirmForgotPassword wrong code -> status %d, want 400", status)
	}
	cognitoAssertErrType(t, body, "CodeMismatchException")

	// Right code but weak password → InvalidPasswordException.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.ConfirmForgotPassword",
		map[string]any{
			"ClientId":         clientID,
			"Username":         forgotUser,
			"ConfirmationCode": code,
			"Password":         "alllowercase1",
		})
	if status != 400 {
		t.Fatalf("ConfirmForgotPassword weak password -> status %d, want 400", status)
	}
	cognitoAssertErrType(t, body, "InvalidPasswordException")

	// Right code + policy password → success, then login with the new password.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.ConfirmForgotPassword",
		map[string]any{
			"ClientId":         clientID,
			"Username":         forgotUser,
			"ConfirmationCode": code,
			"Password":         "NewPass1234!",
		})
	if status != 200 {
		t.Fatalf("ConfirmForgotPassword -> status %d, want 200; body %s", status, body)
	}

	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.InitiateAuth",
		map[string]any{
			"AuthFlow": "USER_PASSWORD_AUTH",
			"AuthParameters": map[string]any{
				"USERNAME": forgotUser,
				"PASSWORD": "NewPass1234!",
			},
			"ClientId": clientID,
		})
	if status != 200 {
		t.Fatalf("InitiateAuth after reset -> status %d, want 200; body %s", status, body)
	}
}

// TestAWSCognitoForgotPasswordLockout verifies the LimitExceededException
// ladder: five wrong confirmation codes lock the reset attempt.
func TestAWSCognitoForgotPasswordLockout(t *testing.T) {
	base := cognitoServe(t)
	const clientID = "lockout-client"
	const lockUser = "lockout-user-3"
	const rightCode = "000003" // digits of "lockout-user-3", zero-padded

	body, status := cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.SignUp",
		map[string]any{
			"ClientId": clientID,
			"Username": lockUser,
			"Password": "LockPass123!",
		})
	if status != 200 {
		t.Fatalf("SignUp -> status %d, want 200; body %s", status, body)
	}
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.ForgotPassword",
		map[string]any{"ClientId": clientID, "Username": lockUser})
	if status != 200 {
		t.Fatalf("ForgotPassword -> status %d, want 200; body %s", status, body)
	}

	for i := 0; i < 5; i++ {
		body, status = cognitoPostTarget(t, base+"/",
			"AWSCognitoIdentityProviderService.ConfirmForgotPassword",
			map[string]any{
				"ClientId":         clientID,
				"Username":         lockUser,
				"ConfirmationCode": "111111",
				"Password":         "NewPass1234!",
			})
		if status != 400 {
			t.Fatalf("wrong code attempt %d -> status %d, want 400", i+1, status)
		}
		cognitoAssertErrType(t, body, "CodeMismatchException")
	}

	// Even the RIGHT code is locked out after five failures (real Cognito
	// returns LimitExceededException "Attempt limit exceeded").
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.ConfirmForgotPassword",
		map[string]any{
			"ClientId":         clientID,
			"Username":         lockUser,
			"ConfirmationCode": rightCode,
			"Password":         "NewPass1234!",
		})
	if status != 400 {
		t.Fatalf("locked-out confirm -> status %d, want 400; body %s", status, body)
	}
	cognitoAssertErrType(t, body, "LimitExceededException")
}

// TestAWSCognitoAdminUserGlobalSignOut covers the admin revocation variant.
func TestAWSCognitoAdminUserGlobalSignOut(t *testing.T) {
	base := cognitoServe(t)
	const clientID = "admin-signout-client"
	const user = "admin-signout-user"

	body, status := cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.SignUp",
		map[string]any{"ClientId": clientID, "Username": user, "Password": "SignOut123!"})
	if status != 200 {
		t.Fatalf("SignUp -> status %d, want 200; body %s", status, body)
	}
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.ConfirmSignUp",
		map[string]any{"ClientId": clientID, "Username": user, "ConfirmationCode": "000000"})
	if status != 200 {
		t.Fatalf("ConfirmSignUp -> status %d, want 200; body %s", status, body)
	}
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.InitiateAuth",
		map[string]any{
			"AuthFlow": "USER_PASSWORD_AUTH",
			"AuthParameters": map[string]any{
				"USERNAME": user,
				"PASSWORD": "SignOut123!",
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
	access := authResp["AuthenticationResult"].(map[string]any)["AccessToken"].(string)

	// Unknown username → ResourceNotFoundException.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.AdminUserGlobalSignOut",
		map[string]any{"UserPoolId": "us-east-1_mock", "Username": "ghost-user"})
	if status != 400 {
		t.Fatalf("AdminUserGlobalSignOut unknown user -> status %d, want 400", status)
	}
	cognitoAssertErrType(t, body, "ResourceNotFoundException")

	// Revoke by username → subsequent token use is rejected.
	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.AdminUserGlobalSignOut",
		map[string]any{"UserPoolId": "us-east-1_mock", "Username": user})
	if status != 200 {
		t.Fatalf("AdminUserGlobalSignOut -> status %d, want 200; body %s", status, body)
	}

	body, status = cognitoPostTarget(t, base+"/",
		"AWSCognitoIdentityProviderService.GetUser",
		map[string]any{"AccessToken": access})
	if status != 400 {
		t.Fatalf("GetUser after admin sign-out -> status %d, want 400", status)
	}
	cognitoAssertErrType(t, body, "NotAuthorizedException")
}

// TestAWSCognitoMalformedServiceBody posts garbage where the service API
// expects x-amz-json-1.1: the handlers must fail with a Cognito error
// envelope, never a 500 (undecodable bodies surface as an empty dict via
// req.body, so handlers decode req.raw_body defensively).
func TestAWSCognitoMalformedServiceBody(t *testing.T) {
	base := cognitoServe(t)

	req, err := http.NewRequest("POST", base+"/", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AWSCognitoIdentityProviderService.InitiateAuth")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 {
		t.Fatalf("malformed body InitiateAuth -> status %d, want 400; body %s", resp.StatusCode, b)
	}
	// The empty body surfaces as a missing-AuthFlow parameter error, not a 500.
	cognitoAssertErrType(t, string(b), "InvalidParameterException")

	// A malformed SigV4 Authorization header is rejected structurally.
	req, err = http.NewRequest("POST", base+"/", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AWSCognitoIdentityProviderService.ListUsers")
	req.Header.Set("Authorization", "Bearer nope")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 400 {
		t.Fatalf("malformed SigV4 -> status %d, want 400; body %s", resp.StatusCode, b)
	}
	cognitoAssertErrType(t, string(b), "UnrecognizedClientException")
}

// cognitoServe boots the aws-cognito-style adapter on a fresh test server
// and returns its base URL.
func cognitoServe(t *testing.T) string {
	t.Helper()
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "aws-cognito-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"cognito": {Adapter: adapterDir},
		},
	}
	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	t.Cleanup(cancel)
	time.Sleep(50 * time.Millisecond)
	return addrs["cognito"]
}

// cognitoAssertErrType unmarshals a Cognito error envelope and asserts the
// __type value.
func cognitoAssertErrType(t *testing.T, body, wantType string) {
	t.Helper()
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal error envelope: %v (body %s)", err, body)
	}
	if errResp["__type"] != wantType {
		t.Fatalf("__type = %v, want %v (body %s)", errResp["__type"], wantType, body)
	}
	if msg, _ := errResp["message"].(string); msg == "" {
		t.Fatalf("message = %v, want non-empty string (body %s)", errResp["message"], body)
	}
}

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
