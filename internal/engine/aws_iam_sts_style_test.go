package engine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/primitives/clock"
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
//
// All requests are signed with real SigV4 (awsSigV4Sign in
// aws_s3_style_test.go) using the documented synthetic credentials.
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
	now := time.Now()

	// ===== AssumeRole → ASIA... temp creds + SessionToken =====

	body, status := stsGet(t, base+"/?Action=AssumeRole&RoleArn=arn:aws:iam::123456789012:role/my-role&RoleSessionName=dev-session&DurationSeconds=3600", now)
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

	// ===== GetCallerIdentity → shows the assumed role (the chain) =====

	body, status = stsGet(t, base+"/?Action=GetCallerIdentity", now)
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

	body, status = stsGet(t, base+"/?Action=ListRoles", now)
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

	body, status = stsGet(t, base+"/?Action=GetRole&RoleName=stunt-role", now)
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

	body, status = stsGet(t, base+"/?Action=CreateRole&RoleName=my-test-role&AssumeRolePolicyDocument=%7B%22Version%22%3A%222012-10-17%22%7D", now)
	if status != 200 {
		t.Fatalf("CreateRole -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "CreateRoleResponse") {
		t.Fatalf("CreateRole: missing CreateRoleResponse; body %s", body)
	}

	// Verify it appears in ListRoles
	body, status = stsGet(t, base+"/?Action=ListRoles", now)
	if !strings.Contains(body, "my-test-role") {
		t.Fatalf("CreateRole: new role 'my-test-role' not in ListRoles; body %s", body)
	}

	// ===== CreateAccessKey → AKIA... long-term key =====

	body, status = stsGet(t, base+"/?Action=CreateAccessKey&UserName=stunt-admin", now)
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

	body, status = stsGet(t, base+"/?Action=GetSessionToken", now)
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

	body, status = stsGet(t, base+"/?Action=AssumeRoleWithWebIdentity&RoleArn=arn:aws:iam::123456789012:role/oidc-role&WebIdentityToken=fake-jwt-token", now)
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

	body, status = stsGet(t, base+"/?Action=DecodeAuthorizationMessage&EncodedMessage=dXNlcm5hbWU=", now)
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

	body, status = stsGet(t, base+"/?Action=ListUsers", now)
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

	body, status = stsGetRawAuth(t, base+"/?Action=GetCallerIdentity", "Bearer some-token")
	if status != 403 {
		t.Fatalf("GetCallerIdentity with bad auth -> status %d, want 403; body %s", status, body)
	}
	if !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("GetCallerIdentity with bad auth: missing SignatureDoesNotMatch; body %s", body)
	}

	// ===== Invalid Action → 400 =====

	body, status = stsGet(t, base+"/?Action=BogusAction", now)
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
	body, status = stsPost(t, base+"/", form.Encode(), now)
	if status != 200 {
		t.Fatalf("POST AssumeRole -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "<AccessKeyId>ASIA") {
		t.Fatalf("POST AssumeRole: missing ASIA creds; body %s", body)
	}

	// ===== Missing required param → 400 =====

	body, status = stsGet(t, base+"/?Action=AssumeRole", now)
	if status != 400 {
		t.Fatalf("AssumeRole missing RoleArn -> status %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "ValidationError") {
		t.Fatalf("AssumeRole missing RoleArn: missing ValidationError; body %s", body)
	}

	// ===== GetRole on nonexistent → 404 =====

	body, status = stsGet(t, base+"/?Action=GetRole&RoleName=does-not-exist", now)
	if status != 404 {
		t.Fatalf("GetRole nonexistent -> status %d, want 404; body %s", status, body)
	}
	if !strings.Contains(body, "NoSuchEntity") {
		t.Fatalf("GetRole nonexistent: missing NoSuchEntity; body %s", body)
	}
}

// TestAwsIamStsStyleSigV4Verification drives the adapter's real SigV4
// recomputation with a deterministic virtual clock: a correctly signed
// request passes, tampered signatures / unknown access keys are rejected
// with real STS error envelopes, stale x-amz-date hits the skew window,
// and credential expirations derive from the clock.
func TestAwsIamStsStyleSigV4Verification(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "aws-iam-sts-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	base0 := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	vc := clock.NewVirtualClock(base0)
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"sts": {Adapter: adapterDir},
		},
	}
	e, err := New(m, WithClock(vc))
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

	// Positive path + clock-derived Expiration: now + DurationSeconds.
	body, status := stsGet(t, base+"/?Action=AssumeRole&RoleArn=arn:aws:iam::123456789012:role/clk-role&RoleSessionName=clk&DurationSeconds=3600", vc.Now())
	if status != 200 {
		t.Fatalf("AssumeRole (virtual clock) -> status %d; body %s", status, body)
	}
	wantExp := base0.Add(3600 * time.Second).UTC().Format(time.RFC3339)
	if !strings.Contains(body, "<Expiration>"+wantExp+"</Expiration>") {
		t.Fatalf("AssumeRole Expiration not clock-derived (want %s); body %s", wantExp, body)
	}

	// CreateRole CreateDate derives from the clock too.
	body, status = stsGet(t, base+"/?Action=CreateRole&RoleName=clk-role-created", vc.Now())
	if status != 200 || !strings.Contains(body, "<CreateDate>"+base0.UTC().Format(time.RFC3339)+"</CreateDate>") {
		t.Fatalf("CreateRole CreateDate not clock-derived -> status %d; body %s", status, body)
	}

	// Tampered signature → 403 SignatureDoesNotMatch.
	req := stsSignedReq(t, "GET", base+"/?Action=GetCallerIdentity", nil, vc.Now(), awsStyleAccessKey, awsStyleSecretKey)
	req.Header.Set("Authorization", awsTamperSignature(req.Header.Get("Authorization")))
	body, status = stsDo(t, req)
	if status != 403 || !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("tampered signature -> status %d; body %s", status, body)
	}

	// Wrong secret → 403 SignatureDoesNotMatch.
	req = stsSignedReq(t, "GET", base+"/?Action=GetCallerIdentity", nil, vc.Now(), awsStyleAccessKey, awsStyleBadSecret)
	body, status = stsDo(t, req)
	if status != 403 || !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("wrong secret -> status %d; body %s", status, body)
	}

	// Unknown access key → 403 InvalidClientTokenId (real STS error).
	req = stsSignedReq(t, "GET", base+"/?Action=GetCallerIdentity", nil, vc.Now(), "AKIAUNKNOWNKEY000000", awsStyleSecretKey)
	body, status = stsDo(t, req)
	if status != 403 || !strings.Contains(body, "InvalidClientTokenId") {
		t.Fatalf("unknown AKID -> status %d; body %s", status, body)
	}

	// Tampered payload on a signed POST → 403 SignatureDoesNotMatch (the
	// payload hash covers the verbatim form bytes).
	form := "Action=AssumeRole&RoleArn=arn:aws:iam::123456789012:role/x&RoleSessionName=y"
	req = stsSignedReq(t, "POST", base+"/", []byte(form), vc.Now(), awsStyleAccessKey, awsStyleSecretKey)
	tampered := []byte(form + "&Extra=tampered")
	req.Body = io.NopCloser(bytes.NewReader(tampered))
	req.ContentLength = int64(len(tampered))
	body, status = stsDo(t, req)
	if status != 403 || !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("tampered POST payload -> status %d; body %s", status, body)
	}

	// Stale x-amz-date beyond the 15-minute window → 403 RequestTimeTooSkewed.
	vc.Advance(20 * time.Minute)
	body, status = stsGet(t, base+"/?Action=GetCallerIdentity", base0) // signed at the old time
	if status != 403 || !strings.Contains(body, "RequestTimeTooSkewed") {
		t.Fatalf("stale request -> status %d; body %s", status, body)
	}

	// A fresh request at the advanced clock time works again.
	body, status = stsGet(t, base+"/?Action=GetCallerIdentity", vc.Now())
	if status != 200 || !strings.Contains(body, "GetCallerIdentityResponse") {
		t.Fatalf("fresh signed request -> status %d; body %s", status, body)
	}
}

// === STS test helpers ===

// stsSignedReq builds a request and signs it with real SigV4 (sts service,
// given credentials).
func stsSignedReq(t *testing.T, method, rawurl string, body []byte, at time.Time, accessKey, secretKey string) *http.Request {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawurl, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	awsSigV4Sign(t, req, body, "sts", accessKey, secretKey, at)
	return req
}

func stsDo(t *testing.T, req *http.Request) (string, int) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func stsGet(t *testing.T, rawurl string, at time.Time) (string, int) {
	t.Helper()
	return stsDo(t, stsSignedReq(t, "GET", rawurl, nil, at, awsStyleAccessKey, awsStyleSecretKey))
}

func stsGetRawAuth(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	return stsDo(t, req)
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

func stsPost(t *testing.T, rawurl, formBody string, at time.Time) (string, int) {
	t.Helper()
	return stsDo(t, stsSignedReq(t, "POST", rawurl, []byte(formBody), at, awsStyleAccessKey, awsStyleSecretKey))
}
