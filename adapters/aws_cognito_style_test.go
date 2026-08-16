package adapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// These tests drive the aws-cognito-style adapter scripts directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock, covering the
// time-dependent behaviors that the HTTP-level engine test cannot reach
// without waiting: verification-code expiry, challenge-session expiry
// (AuthSessionValidity), and refresh-token expiry.

// cognitoFixture is one shared store + virtual clock with a loaded VM per
// handler script (oauth.star and service.star each need their own VM, but
// they observe the same collections/kv/blob state, like the engine).
type cognitoFixture struct {
	t       *testing.T
	vc      *clock.Clock
	vmOauth *starlark.VM
	vmSvc   *starlark.VM
	store   *primitives.Store
}

func newCognitoFixture(t *testing.T, start time.Time) *cognitoFixture {
	t.Helper()
	dir := repoAdaptersDir(t)
	root := filepath.Join(dir, "aws-cognito-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}

	tmp := t.TempDir()
	store, err := primitives.Open(filepath.Join(tmp, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	kvStore, err := kv.Open(filepath.Join(tmp, "c.kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kvStore.Close() })
	blobStore, err := blob.Open(filepath.Join(tmp, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store:       store,
		KV:          kvStore,
		Blob:        blobStore,
		Clock:       vc,
		ServiceName: "test",
	})

	load := func(script string) *starlark.VM {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(root, "scripts", script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
		if err != nil {
			t.Fatalf("LoadWithLib %s: %v", script, err)
		}
		return vm
	}

	return &cognitoFixture{
		t:       t,
		vc:      vc,
		vmOauth: load("oauth.star"),
		vmSvc:   load("service.star"),
		store:   store,
	}
}

// svcCall invokes a service-API operation (X-Amz-Target dispatch) with a
// JSON body carried in raw_body only — req.body stays empty, exercising the
// handlers' raw-body-first decoding.
func (f *cognitoFixture) svcCall(target string, payload map[string]any) starlark.Response {
	f.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := f.vmSvc.Call("on_service_api", starlark.Request{
		Method:  "POST",
		Path:    "/",
		Headers: map[string]string{"X-Amz-Target": target},
		RawBody: string(raw),
	})
	if err != nil {
		f.t.Fatalf("on_service_api %s: %v", target, err)
	}
	return resp
}

// oauthCall invokes a hosted-UI handler.
func (f *cognitoFixture) oauthCall(handler, method, path string, query map[string]string, body map[string]any) starlark.Response {
	f.t.Helper()
	resp, err := f.vmOauth.Call(handler, starlark.Request{
		Method: method,
		Path:   path,
		Query:  query,
		Body:   body,
	})
	if err != nil {
		f.t.Fatalf("oauth %s: %v", handler, err)
	}
	return resp
}

// errType digs __type out of a Cognito error envelope.
func errType(t *testing.T, resp starlark.Response) string {
	t.Helper()
	et, _ := resp.Body["__type"].(string)
	return et
}

// TestCognitoForgotPasswordExpiry: a reset code is valid for one hour; after
// the window lapses (virtual clock advanced), even the correct code returns
// ExpiredCodeException.
func TestCognitoForgotPasswordExpiry(t *testing.T) {
	f := newCognitoFixture(t, time.Unix(1_750_000_000, 0).UTC())
	const user = "forgot-exp-8"
	const code = "000008" // digits of "forgot-exp-8", zero-padded to 6

	if resp := f.svcCall("AWSCognitoIdentityProviderService.SignUp", map[string]any{
		"ClientId": "vm-client", "Username": user, "Password": "OldPass123!",
	}); resp.Status != 200 {
		t.Fatalf("SignUp -> %d: %v", resp.Status, resp.Body)
	}
	if resp := f.svcCall("AWSCognitoIdentityProviderService.ForgotPassword", map[string]any{
		"ClientId": "vm-client", "Username": user,
	}); resp.Status != 200 {
		t.Fatalf("ForgotPassword -> %d: %v", resp.Status, resp.Body)
	}

	f.vc.Advance(2 * time.Hour)

	resp := f.svcCall("AWSCognitoIdentityProviderService.ConfirmForgotPassword", map[string]any{
		"ClientId":         "vm-client",
		"Username":         user,
		"ConfirmationCode": code,
		"Password":         "NewPass1234!",
	})
	if resp.Status != 400 {
		t.Fatalf("expired ConfirmForgotPassword -> %d, want 400: %v", resp.Status, resp.Body)
	}
	if got := errType(t, resp); got != "ExpiredCodeException" {
		t.Fatalf("__type = %q, want ExpiredCodeException", got)
	}
}

// TestCognitoChallengeSessionExpiry: a NEW_PASSWORD_REQUIRED session lives
// for the app client's AuthSessionValidity (3 minutes here); after it lapses
// the challenge response fails with NotAuthorizedException.
func TestCognitoChallengeSessionExpiry(t *testing.T) {
	f := newCognitoFixture(t, time.Unix(1_750_000_000, 0).UTC())

	resp := f.svcCall("AWSCognitoIdentityProviderService.AdminInitiateAuth", map[string]any{
		"UserPoolId": "us-east-1_mock",
		"AuthFlow":   "ADMIN_USER_PASSWORD_AUTH",
		"AuthParameters": map[string]any{
			"USERNAME": "force-change-user",
			"PASSWORD": "TempPass1A!",
		},
		"ClientId": "vm-client",
	})
	if resp.Status != 200 {
		t.Fatalf("AdminInitiateAuth -> %d: %v", resp.Status, resp.Body)
	}
	session, _ := resp.Body["Session"].(string)
	if resp.Body["ChallengeName"] != "NEW_PASSWORD_REQUIRED" || session == "" {
		t.Fatalf("challenge response = %v", resp.Body)
	}

	// Before expiry the session is usable (policy failure keeps it alive).
	ok := f.svcCall("AWSCognitoIdentityProviderService.AdminRespondToAuthChallenge", map[string]any{
		"ChallengeName": "NEW_PASSWORD_REQUIRED",
		"Session":       session,
		"ChallengeResponses": map[string]any{
			"USERNAME":     "force-change-user",
			"NEW_PASSWORD": "weak",
		},
	})
	if errType(t, ok) != "InvalidPasswordException" {
		t.Fatalf("weak password before expiry -> __type %q, want InvalidPasswordException (body %v)", errType(t, ok), ok.Body)
	}

	f.vc.Advance(4 * time.Minute)

	expired := f.svcCall("AWSCognitoIdentityProviderService.AdminRespondToAuthChallenge", map[string]any{
		"ChallengeName": "NEW_PASSWORD_REQUIRED",
		"Session":       session,
		"ChallengeResponses": map[string]any{
			"USERNAME":     "force-change-user",
			"NEW_PASSWORD": "Rotated1Pass!",
		},
	})
	if expired.Status != 400 {
		t.Fatalf("expired session -> %d, want 400: %v", expired.Status, expired.Body)
	}
	if got := errType(t, expired); got != "NotAuthorizedException" {
		t.Fatalf("__type = %q, want NotAuthorizedException", got)
	}
}

// TestCognitoRefreshTokenExpiry: the refresh token is reusable (two grants
// with the same token both succeed — real Cognito default, no rotation) and
// dies at its 30-day expiry.
func TestCognitoRefreshTokenExpiry(t *testing.T) {
	f := newCognitoFixture(t, time.Unix(1_750_000_000, 0).UTC())

	auth := f.svcCall("AWSCognitoIdentityProviderService.InitiateAuth", map[string]any{
		"AuthFlow": "USER_PASSWORD_AUTH",
		"AuthParameters": map[string]any{
			"USERNAME": "demo-user",
			"PASSWORD": "DemoPass123!",
		},
		"ClientId": "vm-client",
	})
	if auth.Status != 200 {
		t.Fatalf("InitiateAuth -> %d: %v", auth.Status, auth.Body)
	}
	refresh := auth.Body["AuthenticationResult"].(map[string]any)["RefreshToken"].(string)

	grant := func() starlark.Response {
		return f.oauthCall("on_token", "POST", "/oauth2/token", nil, map[string]any{
			"grant_type":    "refresh_token",
			"refresh_token": refresh,
			"client_id":     "vm-client",
		})
	}

	first := grant()
	if first.Status != 200 {
		t.Fatalf("first refresh grant -> %d: %v", first.Status, first.Body)
	}
	if _, rotated := first.Body["refresh_token"]; rotated {
		t.Fatalf("refresh grant returned a new refresh_token (%v), want none — no rotation", first.Body["refresh_token"])
	}
	second := grant()
	if second.Status != 200 {
		t.Fatalf("second refresh grant with the SAME token -> %d: %v (refresh tokens must be reusable)", second.Status, second.Body)
	}
	if first.Body["access_token"] == second.Body["access_token"] {
		t.Fatal("access token did not rotate between refresh grants")
	}

	f.vc.Advance(31 * 24 * time.Hour)

	third := grant()
	if third.Status != 400 {
		t.Fatalf("refresh grant after expiry -> %d, want 400: %v", third.Status, third.Body)
	}
	if third.Body["error"] != "invalid_grant" {
		t.Fatalf("error = %v, want invalid_grant", third.Body["error"])
	}
}

// TestCognitoAuthorizeBindsExistingUsers: hosted-UI authorize binds the
// seeded demo-user (never mints random users) — two independent authorize
// flows resolve the same sub, and the users collection stays at exactly the
// two seeded users.
func TestCognitoAuthorizeBindsExistingUsers(t *testing.T) {
	f := newCognitoFixture(t, time.Unix(1_750_000_000, 0).UTC())
	const redirectURI = "http://localhost:3000/cb"

	userInfo := func() (string, string) {
		t.Helper()
		authResp := f.oauthCall("on_authorize", "GET", "/oauth2/authorize", map[string]string{
			"client_id":     "vm-client",
			"redirect_uri":  redirectURI,
			"response_type": "code",
			"state":         "s",
		}, nil)
		if authResp.Status != 302 {
			t.Fatalf("authorize -> %d: %v", authResp.Status, authResp.Body)
		}
		location := authResp.Headers["Location"]
		code := location[strings.Index(location, "code=")+len("code="):]
		if i := strings.Index(code, "&"); i >= 0 {
			code = code[:i]
		}

		tokResp := f.oauthCall("on_token", "POST", "/oauth2/token", nil, map[string]any{
			"grant_type":   "authorization_code",
			"code":         code,
			"client_id":    "vm-client",
			"redirect_uri": redirectURI,
		})
		if tokResp.Status != 200 {
			t.Fatalf("token -> %d: %v", tokResp.Status, tokResp.Body)
		}
		access := tokResp.Body["access_token"].(string)

		uiResp, err := f.vmOauth.Call("on_user_info", starlark.Request{
			Method:  "GET",
			Path:    "/oauth2/userInfo",
			Headers: map[string]string{"Authorization": "Bearer " + access},
		})
		if err != nil {
			t.Fatalf("userInfo: %v", err)
		}
		if uiResp.Status != 200 {
			t.Fatalf("userInfo -> %d: %v", uiResp.Status, uiResp.Body)
		}
		return uiResp.Body["sub"].(string), uiResp.Body["username"].(string)
	}

	sub1, user1 := userInfo()
	sub2, user2 := userInfo()
	if sub1 != sub2 || user1 != user2 {
		t.Fatalf("authorize bound different users across calls: (%s,%s) vs (%s,%s)", sub1, user1, sub2, user2)
	}
	if user1 != "demo-user" {
		t.Fatalf("default authorize bound %q, want the seeded demo-user", user1)
	}

	users, err := f.store.Collection("users")
	if err != nil {
		t.Fatal(err)
	}
	docs, err := users.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("users collection has %d docs, want exactly the 2 seeded users (no random user minting)", len(docs))
	}
}
