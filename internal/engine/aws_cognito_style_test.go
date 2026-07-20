package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
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
