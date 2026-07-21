package engine

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestAwsIamStsStyleAdapter exercises the AWS IAM/STS-style adapter:
//
//   - AssumeRole → returns ASIA... creds + SessionToken (STATEFUL)
//   - GetCallerIdentity → shows the assumed role (credential provider chain)
//   - ListRoles → XML with seeded roles
//   - GetRole → single role
//   - CreateRole → creates + appears in ListRoles
//   - CreateAccessKey → returns AKIA... long-term key
//   - GetSessionToken → temp creds
//   - AssumeRoleWithWebIdentity → OIDC federation
//   - DecodeAuthorizationMessage
//   - Without SigV4 auth → 403 error XML
//   - Invalid Action → 400 error XML
func TestAwsIamStsStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "aws-iam-sts-style")
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
			"sts": {Adapter: absAdapterDir},
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

	base := addrs["sts"]

	// Fake but well-formed SigV4 Authorization header for STS.
	const sigv4 = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260120/us-east-1/sts/aws4_request, SignedHeaders=host;x-amz-date, Signature=fe5f80f77d5fa3beca038a248ff027d044aca418"

	// ===== AssumeRole → ASIA... temp creds + SessionToken =====

	body, status := stsGet(t, base+"/?Action=AssumeRole&RoleArn=arn:aws:iam::123456789012:role/my-role&RoleSessionName=dev-session&DurationSeconds=3600", sigv4)
	if status != 200 {
		t.Fatalf("AssumeRole -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "AssumeRoleResponse") {
		t.Fatalf("AssumeRole: missing AssumeRoleResponse; body %s", body)
	}
	if !strings.Contains(body, "<AccessKeyId>ASIA") {
		t.Fatalf("AssumeRole: AccessKeyId does not start with ASIA; body %s", body)
	}
	if !strings.Contains(body, "<SecretAccessKey>") {
		t.Fatalf("AssumeRole: missing SecretAccessKey; body %s", body)
	}
	if !strings.Contains(body, "<SessionToken>") {
		t.Fatalf("AssumeRole: missing SessionToken; body %s", body)
	}
	if !strings.Contains(body, "<Expiration>") {
		t.Fatalf("AssumeRole: missing Expiration; body %s", body)
	}
	if !strings.Contains(body, "my-role") {
		t.Fatalf("AssumeRole: Arn missing role name; body %s", body)
	}
	if !strings.Contains(body, "dev-session") {
		t.Fatalf("AssumeRole: missing session name in AssumedRoleId; body %s", body)
	}

	// ===== GetCallerIdentity → shows the assumed role (credential chain) =====

	body, status = stsGet(t, base+"/?Action=GetCallerIdentity", sigv4)
	if status != 200 {
		t.Fatalf("GetCallerIdentity -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "GetCallerIdentityResponse") {
		t.Fatalf("GetCallerIdentity: missing response wrapper; body %s", body)
	}
	if !strings.Contains(body, "my-role") {
		t.Fatalf("GetCallerIdentity: does not reflect assumed role 'my-role'; body %s", body)
	}
	if !strings.Contains(body, "<Account>") {
		t.Fatalf("GetCallerIdentity: missing Account; body %s", body)
	}
	if !strings.Contains(body, "<UserId>") {
		t.Fatalf("GetCallerIdentity: missing UserId; body %s", body)
	}

	// ===== ListRoles → XML with seeded roles =====

	body, status = stsGet(t, base+"/?Action=ListRoles", sigv4)
	if status != 200 {
		t.Fatalf("ListRoles -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "ListRolesResponse") {
		t.Fatalf("ListRoles: missing ListRolesResponse; body %s", body)
	}
	if !strings.Contains(body, "<RoleName>") {
		t.Fatalf("ListRoles: missing RoleName; body %s", body)
	}
	if !strings.Contains(body, "stunt-role") {
		t.Fatalf("ListRoles: missing seeded stunt-role; body %s", body)
	}

	// ===== GetRole → single role =====

	body, status = stsGet(t, base+"/?Action=GetRole&RoleName=stunt-role", sigv4)
	if status != 200 {
		t.Fatalf("GetRole -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "GetRoleResponse") {
		t.Fatalf("GetRole: missing GetRoleResponse; body %s", body)
	}
	if !strings.Contains(body, "stunt-role") {
		t.Fatalf("GetRole: missing role name; body %s", body)
	}

	// ===== CreateRole → creates + appears in ListRoles (STATEFUL) =====

	body, status = stsGet(t, base+"/?Action=CreateRole&RoleName=my-test-role&AssumeRolePolicyDocument=%7B%22Version%22%3A%222012-10-17%22%7D", sigv4)
	if status != 200 {
		t.Fatalf("CreateRole -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "CreateRoleResponse") {
		t.Fatalf("CreateRole: missing CreateRoleResponse; body %s", body)
	}

	// Verify it appears in ListRoles
	body, status = stsGet(t, base+"/?Action=ListRoles", sigv4)
	if !strings.Contains(body, "my-test-role") {
		t.Fatalf("CreateRole: new role 'my-test-role' not in ListRoles; body %s", body)
	}

	// ===== CreateAccessKey → AKIA... long-term key =====

	body, status = stsGet(t, base+"/?Action=CreateAccessKey&UserName=stunt-admin", sigv4)
	if status != 200 {
		t.Fatalf("CreateAccessKey -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "CreateAccessKeyResponse") {
		t.Fatalf("CreateAccessKey: missing response wrapper; body %s", body)
	}
	if !strings.Contains(body, "<AccessKeyId>AKIA") {
		t.Fatalf("CreateAccessKey: AccessKeyId does not start with AKIA; body %s", body)
	}

	// ===== GetSessionToken → temp creds =====

	body, status = stsGet(t, base+"/?Action=GetSessionToken", sigv4)
	if status != 200 {
		t.Fatalf("GetSessionToken -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "GetSessionTokenResponse") {
		t.Fatalf("GetSessionToken: missing response wrapper; body %s", body)
	}
	if !strings.Contains(body, "<AccessKeyId>ASIA") {
		t.Fatalf("GetSessionToken: temp creds should start with ASIA; body %s", body)
	}

	// ===== AssumeRoleWithWebIdentity → OIDC federation =====

	body, status = stsGet(t, base+"/?Action=AssumeRoleWithWebIdentity&RoleArn=arn:aws:iam::123456789012:role/oidc-role&WebIdentityToken=fake-jwt-token", sigv4)
	if status != 200 {
		t.Fatalf("AssumeRoleWithWebIdentity -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "AssumeRoleWithWebIdentityResponse") {
		t.Fatalf("AssumeRoleWithWebIdentity: missing response wrapper; body %s", body)
	}
	if !strings.Contains(body, "<AccessKeyId>ASIA") {
		t.Fatalf("AssumeRoleWithWebIdentity: temp creds should start with ASIA; body %s", body)
	}

	// ===== DecodeAuthorizationMessage =====

	body, status = stsGet(t, base+"/?Action=DecodeAuthorizationMessage&EncodedMessage=dXNlcm5hbWU=", sigv4)
	if status != 200 {
		t.Fatalf("DecodeAuthorizationMessage -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "DecodeAuthorizationMessageResponse") {
		t.Fatalf("DecodeAuthorizationMessage: missing response wrapper; body %s", body)
	}
	if !strings.Contains(body, "<DecodedMessage>") {
		t.Fatalf("DecodeAuthorizationMessage: missing DecodedMessage; body %s", body)
	}

	// ===== ListUsers =====

	body, status = stsGet(t, base+"/?Action=ListUsers", sigv4)
	if status != 200 {
		t.Fatalf("ListUsers -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "ListUsersResponse") {
		t.Fatalf("ListUsers: missing response wrapper; body %s", body)
	}
	if !strings.Contains(body, "stunt-admin") {
		t.Fatalf("ListUsers: missing seeded stunt-admin; body %s", body)
	}

	// ===== Without auth → 403 error XML =====

	body, status = stsGetNoAuth(t, base+"/?Action=GetCallerIdentity")
	if status != 403 {
		t.Fatalf("GetCallerIdentity without auth -> status %d, want 403; body %s", status, body)
	}
	if !strings.Contains(body, "ErrorResponse") {
		t.Fatalf("GetCallerIdentity without auth: missing ErrorResponse; body %s", body)
	}

	// ===== Malformed auth → 403 =====

	body, status = stsGet(t, base+"/?Action=GetCallerIdentity", "Bearer some-token")
	if status != 403 {
		t.Fatalf("GetCallerIdentity with bad auth -> status %d, want 403; body %s", status, body)
	}
	if !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("GetCallerIdentity with bad auth: missing SignatureDoesNotMatch; body %s", body)
	}

	// ===== Invalid Action → 400 =====

	body, status = stsGet(t, base+"/?Action=BogusAction", sigv4)
	if status != 400 {
		t.Fatalf("Invalid Action -> status %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "InvalidAction") {
		t.Fatalf("Invalid Action: missing InvalidAction; body %s", body)
	}

	// ===== POST form-encoded body also works (query API) =====

	form := url.Values{}
	form.Set("Action", "AssumeRole")
	form.Set("RoleArn", "arn:aws:iam::123456789012:role/post-role")
	form.Set("RoleSessionName", "post-session")
	body, status = stsPost(t, base+"/", sigv4, form.Encode())
	if status != 200 {
		t.Fatalf("POST AssumeRole -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "<AccessKeyId>ASIA") {
		t.Fatalf("POST AssumeRole: missing ASIA creds; body %s", body)
	}

	// ===== Missing required param → 400 =====

	body, status = stsGet(t, base+"/?Action=AssumeRole", sigv4)
	if status != 400 {
		t.Fatalf("AssumeRole missing RoleArn -> status %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "ValidationError") {
		t.Fatalf("AssumeRole missing RoleArn: missing ValidationError; body %s", body)
	}

	// ===== GetRole on nonexistent → 404 =====

	body, status = stsGet(t, base+"/?Action=GetRole&RoleName=does-not-exist", sigv4)
	if status != 404 {
		t.Fatalf("GetRole nonexistent -> status %d, want 404; body %s", status, body)
	}
	if !strings.Contains(body, "NoSuchEntity") {
		t.Fatalf("GetRole nonexistent: missing NoSuchEntity; body %s", body)
	}
}

// === STS test helpers ===

func stsGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func stsGetNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func stsPost(t *testing.T, rawurl, auth, formBody string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, strings.NewReader(formBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
