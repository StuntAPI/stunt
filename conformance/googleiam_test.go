package conformance

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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
)

// TestGoogleIAMConformance drives Google's generated IAM client
// (google.golang.org/api/iam/v1) against the google-iam-style adapter.
// The bearer is not static: the suite signs a jwt-bearer assertion with the
// adapter's published mock keypair, exchanges it at the adapter's token
// endpoint, and feeds the minted token to the stock SDK — the real SA flow.
func TestGoogleIAMConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "google-iam-style")
	project := "mock-project"

	// ===== jwt-bearer exchange mints the SDK's access token =====
	assertion := signAssertion(t, base)
	tok := exchangeAssertion(t, base, assertion)
	if tok == "" || !strings.HasPrefix(tok, "ya29.mock-iam-token-") {
		t.Fatalf("token exchange: %q", tok)
	}
	svc, err := iam.NewService(ctx,
		option.WithEndpoint(base),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok})),
	)
	if err != nil {
		t.Fatalf("iam.NewService: %v", err)
	}
	Record(t, "google-api-go-client", "google-iam-style", "jwt-bearer exchange mints the SDK's access token")

	// ===== ServiceAccounts.List returns the seeded default account =====
	sas, err := svc.Projects.ServiceAccounts.List("projects/" + project).Do()
	if err != nil {
		t.Fatalf("serviceAccounts.list: %v", err)
	}
	if len(sas.Accounts) == 0 || !strings.HasSuffix(sas.Accounts[0].Email, ".iam.gserviceaccount.com") {
		t.Fatalf("serviceAccounts.list: %+v", sas.Accounts)
	}
	Record(t, "google-api-go-client", "google-iam-style", "ServiceAccounts.List returns the seeded account")

	// ===== Create + Get round-trip =====
	created, err := svc.Projects.ServiceAccounts.Create("projects/"+project, &iam.CreateServiceAccountRequest{
		AccountId:      "sdk-sa",
		ServiceAccount: &iam.ServiceAccount{DisplayName: "SDK Conformance SA"},
	}).Do()
	if err != nil {
		t.Fatalf("serviceAccounts.create: %v", err)
	}
	if created.Email != "sdk-sa@"+project+".iam.gserviceaccount.com" {
		t.Fatalf("serviceAccounts.create: email=%q", created.Email)
	}
	got, err := svc.Projects.ServiceAccounts.Get(created.Name).Do()
	if err != nil {
		t.Fatalf("serviceAccounts.get: %v", err)
	}
	if got.DisplayName != "SDK Conformance SA" {
		t.Errorf("serviceAccounts.get: displayName=%q", got.DisplayName)
	}
	Record(t, "google-api-go-client", "google-iam-style", "ServiceAccounts.Create + Get round-trip")

	// ===== Keys.List returns the account's user-managed keys =====
	keys, err := svc.Projects.ServiceAccounts.Keys.List(created.Name).Do()
	if err != nil {
		t.Fatalf("serviceAccounts.keys.list: %v", err)
	}
	if len(keys.Keys) == 0 {
		t.Errorf("serviceAccounts.keys.list: empty")
	}
	Record(t, "google-api-go-client", "google-iam-style", "ServiceAccounts.Keys.List returns managed keys")

	// ===== roles.queryGrantableRoles answers on the canonical path =====
	roles, err := svc.Roles.QueryGrantableRoles(&iam.QueryGrantableRolesRequest{
		FullResourceName: "//cloudresourcemanager.googleapis.com/projects/" + project,
	}).Do()
	if err != nil {
		t.Fatalf("roles.queryGrantableRoles: %v", err)
	}
	if len(roles.Roles) == 0 {
		t.Errorf("roles.queryGrantableRoles: no roles")
	}
	Record(t, "google-api-go-client", "google-iam-style", "Roles.QueryGrantableRoles returns grantable roles")

	// ===== Delete removes; Get decodes googleapi 404 =====
	if _, err := svc.Projects.ServiceAccounts.Delete(created.Name).Do(); err != nil {
		t.Fatalf("serviceAccounts.delete: %v", err)
	}
	_, err = svc.Projects.ServiceAccounts.Get(created.Name).Do()
	if gErr, ok := err.(*googleapi.Error); !ok || gErr.Code != 404 {
		t.Errorf("serviceAccounts.get after delete: want googleapi 404, got %v", err)
	}
	Record(t, "google-api-go-client", "google-iam-style", "ServiceAccounts.Delete then Get -> googleapi 404")

	// ===== A garbage bearer is rejected as googleapi 401 =====
	badSvc, err := iam.NewService(ctx,
		option.WithEndpoint(base),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "garbage-token"})),
	)
	if err != nil {
		t.Fatalf("iam.NewService(bad): %v", err)
	}
	_, err = badSvc.Projects.ServiceAccounts.List("projects/" + project).Do()
	if gErr, ok := err.(*googleapi.Error); !ok || gErr.Code != 401 {
		t.Errorf("list with garbage token: want googleapi 401, got %v", err)
	}
	Record(t, "google-api-go-client", "google-iam-style", "Garbage bearer -> googleapi 401")
}

// signAssertion builds the RS256 jwt-bearer assertion Google's SA flow
// sends: iss = the SA email, aud = Google's token endpoint, short-lived exp.
func signAssertion(t *testing.T, base string) string {
	t.Helper()
	key := parseMockKey(t)
	header := b64(map[string]any{"alg": "RS256", "kid": "mock-google-key-1", "typ": "JWT"})
	claims := b64(map[string]any{
		"iss":   "mock-default@mock-project.iam.gserviceaccount.com",
		"scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud":   "https://oauth2.googleapis.com/token",
		"iat":   time.Now().Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	signingInput := header + "." + claims
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func exchangeAssertion(t *testing.T, base, assertion string) string {
	t.Helper()
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	resp, err := http.PostForm(base+"/oauth2/v4/token", form)
	if err != nil {
		t.Fatalf("token exchange: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("token exchange decode: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("token exchange: %d %s %s", resp.StatusCode, body.Error, body.ErrorDescription)
	}
	return body.AccessToken
}

// parseMockKey loads the throwaway RSA keypair the adapter publishes for
// its jwt-bearer flow straight from the adapter source — no duplication.
func parseMockKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "adapters", "google-iam-style", "scripts", "lib.star"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile("(?s)_JWT_PRIVATE_KEY = \"\"\"(-----BEGIN RSA PRIVATE KEY-----.*?-----END RSA PRIVATE KEY-----)\"\"\"")
	m := re.FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("mock private key not found in adapter lib.star")
	}
	block, _ := pem.Decode([]byte(m[1]))
	if block == nil {
		t.Fatal("mock key PEM undecodable")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("mock key parse: %v", err)
	}
	return key
}

func b64(v any) string {
	data, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(data)
}
