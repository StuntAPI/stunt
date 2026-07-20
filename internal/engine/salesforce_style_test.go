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

// TestSalesforceStyleAdapter exercises the Salesforce-style adapter end-to-end:
//
//   - OAuth2 password grant → access_token + instance_url
//   - describe global sobjects
//   - create Account → {id, success:true, errors:[]}
//   - SOQL SELECT ... FROM Account → shows created account (STATEFUL)
//   - retrieve Account by Id
//   - PATCH update Account
//   - DELETE Account
//   - SOQL from Contact / Opportunity
//   - 401 without bearer → SF array error envelope
func TestSalesforceStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "salesforce-style")
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
			"salesforce": {Adapter: absAdapterDir},
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

	base := addrs["salesforce"]

	// ===== OAuth2 password grant → access_token =====

	body, status := sfPostForm(t, base+"/services/oauth2/token", url.Values{
		"grant_type":    {"password"},
		"client_id":     {"test-consumer-key"},
		"client_secret": {"test-consumer-secret"},
		"username":      {"test@example.com"},
		"password":      {"testpassword"},
	})
	if status != 200 {
		t.Fatalf("oauth token -> %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token resp: %v (body %s)", err, body)
	}
	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty", tokenResp["access_token"])
	}
	if !strings.HasPrefix(accessToken, "00D") {
		t.Fatalf("access_token = %q, want 00D-prefixed (Salesforce session id)", accessToken)
	}
	if tokenResp["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", tokenResp["token_type"])
	}
	instanceURL, ok := tokenResp["instance_url"].(string)
	if !ok || instanceURL == "" {
		t.Fatalf("instance_url = %v, want non-empty", tokenResp["instance_url"])
	}

	// ===== describe global sobjects =====

	body, status = sfAuthGet(t, base+"/services/data/v60.0/sobjects", accessToken)
	if status != 200 {
		t.Fatalf("describe global -> %d, want 200; body %s", status, body)
	}
	var describeResp map[string]any
	if err := json.Unmarshal([]byte(body), &describeResp); err != nil {
		t.Fatalf("unmarshal describe global: %v (body %s)", err, body)
	}
	sobjects, ok := describeResp["sobjects"].([]any)
	if !ok || len(sobjects) == 0 {
		t.Fatalf("sobjects = %v, want non-empty array", describeResp["sobjects"])
	}
	// Should contain Account, Contact, etc.
	foundAccount := false
	for _, s := range sobjects {
		obj := s.(map[string]any)
		if obj["name"] == "Account" {
			foundAccount = true
			break
		}
	}
	if !foundAccount {
		t.Fatal("sobjects list does not contain Account")
	}

	// ===== describe Account =====

	body, status = sfAuthGet(t, base+"/services/data/v60.0/sobjects/Account", accessToken)
	if status != 200 {
		t.Fatalf("describe Account -> %d, want 200; body %s", status, body)
	}
	var acctDescribe map[string]any
	if err := json.Unmarshal([]byte(body), &acctDescribe); err != nil {
		t.Fatalf("unmarshal describe Account: %v (body %s)", err, body)
	}
	if acctDescribe["name"] != "Account" {
		t.Fatalf("Account describe name = %v, want Account", acctDescribe["name"])
	}
	if _, ok := acctDescribe["fields"].([]any); !ok {
		t.Fatalf("Account fields = %v, want array", acctDescribe["fields"])
	}

	// ===== create Account =====

	body, status = sfAuthPostJSON(t, base+"/services/data/v60.0/sobjects/Account", accessToken, map[string]any{
		"Name":        "Acme Corporation",
		"Phone":       "+1-555-0100",
		"Website":     "https://acme.example",
		"BillingCity": "Springfield",
	})
	if status != 201 {
		t.Fatalf("create Account -> %d, want 201; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal create resp: %v (body %s)", err, body)
	}
	if createResp["success"] != true {
		t.Fatalf("success = %v, want true", createResp["success"])
	}
	errs, ok := createResp["errors"].([]any)
	if !ok || len(errs) != 0 {
		t.Fatalf("errors = %v, want empty array", createResp["errors"])
	}
	accountID, ok := createResp["id"].(string)
	if !ok || accountID == "" {
		t.Fatalf("id = %v, want non-empty string", createResp["id"])
	}
	if !strings.HasPrefix(accountID, "001") {
		t.Fatalf("Account id = %q, want 001-prefixed", accountID)
	}

	// ===== SOQL SELECT ... FROM Account → shows created account =====

	body, status = sfAuthGet(t, base+"/services/data/v60.0/query/?q="+
		url.QueryEscape("SELECT Id, Name FROM Account"), accessToken)
	if status != 200 {
		t.Fatalf("SOQL query Account -> %d, want 200; body %s", status, body)
	}
	var queryResp map[string]any
	if err := json.Unmarshal([]byte(body), &queryResp); err != nil {
		t.Fatalf("unmarshal SOQL query: %v (body %s)", err, body)
	}
	if queryResp["done"] != true {
		t.Fatalf("done = %v, want true", queryResp["done"])
	}
	records, ok := queryResp["records"].([]any)
	if !ok || len(records) < 1 {
		t.Fatalf("records = %v, want at least 1 (seeded + created)", queryResp["records"])
	}
	totalSize, ok := queryResp["totalSize"].(float64)
	if !ok || int(totalSize) < 1 {
		t.Fatalf("totalSize = %v, want >= 1", queryResp["totalSize"])
	}
	// Verify the first record has attributes.type = "Account"
	rec0 := records[0].(map[string]any)
	attrs, ok := rec0["attributes"].(map[string]any)
	if !ok || attrs["type"] != "Account" {
		t.Fatalf("record attributes.type = %v, want Account", rec0["attributes"])
	}
	if _, ok := rec0["Id"].(string); !ok {
		t.Fatalf("record Id = %v, want string", rec0["Id"])
	}

	// ===== retrieve Account by Id =====

	body, status = sfAuthGet(t, base+"/services/data/v60.0/sobjects/Account/"+accountID, accessToken)
	if status != 200 {
		t.Fatalf("retrieve Account -> %d, want 200; body %s", status, body)
	}
	var acct map[string]any
	if err := json.Unmarshal([]byte(body), &acct); err != nil {
		t.Fatalf("unmarshal retrieve Account: %v (body %s)", err, body)
	}
	if acct["Name"] != "Acme Corporation" {
		t.Fatalf("retrieved Account Name = %v, want 'Acme Corporation'", acct["Name"])
	}
	if acct["Id"] != accountID {
		t.Fatalf("retrieved Account Id = %v, want %s", acct["Id"], accountID)
	}

	// ===== PATCH update Account =====

	body, status = sfAuthPatchJSON(t, base+"/services/data/v60.0/sobjects/Account/"+accountID, accessToken, map[string]any{
		"Name":        "Acme Corp Updated",
		"BillingCity": "Metropolis",
	})
	if status != 204 {
		t.Fatalf("PATCH Account -> %d, want 204; body %s", status, body)
	}

	// Verify the update.
	body, status = sfAuthGet(t, base+"/services/data/v60.0/sobjects/Account/"+accountID, accessToken)
	if status != 200 {
		t.Fatalf("retrieve after PATCH -> %d, want 200; body %s", status, body)
	}
	var updated map[string]any
	if err := json.Unmarshal([]byte(body), &updated); err != nil {
		t.Fatalf("unmarshal updated: %v (body %s)", err, body)
	}
	if updated["Name"] != "Acme Corp Updated" {
		t.Fatalf("updated Name = %v, want 'Acme Corp Updated'", updated["Name"])
	}

	// ===== SOQL WHERE Id = '...' for single record =====

	body, status = sfAuthGet(t, base+"/services/data/v60.0/query/?q="+
		url.QueryEscape("SELECT Id, Name FROM Account WHERE Id = '"+accountID+"'"), accessToken)
	if status != 200 {
		t.Fatalf("SOQL WHERE query -> %d, want 200; body %s", status, body)
	}
	var whereResp map[string]any
	if err := json.Unmarshal([]byte(body), &whereResp); err != nil {
		t.Fatalf("unmarshal SOQL WHERE: %v (body %s)", err, body)
	}
	whereRecords, ok := whereResp["records"].([]any)
	if !ok || len(whereRecords) != 1 {
		t.Fatalf("WHERE records = %v, want exactly 1", whereResp["records"])
	}

	// ===== SOQL from Contact =====

	body, status = sfAuthGet(t, base+"/services/data/v60.0/query/?q="+
		url.QueryEscape("SELECT Id, Name FROM Contact"), accessToken)
	if status != 200 {
		t.Fatalf("SOQL query Contact -> %d, want 200; body %s", status, body)
	}
	var contactResp map[string]any
	if err := json.Unmarshal([]byte(body), &contactResp); err != nil {
		t.Fatalf("unmarshal SOQL Contact: %v (body %s)", err, body)
	}
	contactRecords, ok := contactResp["records"].([]any)
	if !ok || len(contactRecords) < 1 {
		t.Fatalf("Contact records = %v, want at least 1 (seeded)", contactResp["records"])
	}
	contactRec := contactRecords[0].(map[string]any)
	contactAttrs := contactRec["attributes"].(map[string]any)
	if contactAttrs["type"] != "Contact" {
		t.Fatalf("Contact attributes.type = %v, want Contact", contactAttrs["type"])
	}
	contactId, _ := contactRec["Id"].(string)
	if !strings.HasPrefix(contactId, "003") {
		t.Fatalf("Contact Id = %q, want 003-prefixed", contactId)
	}

	// ===== SOQL from Opportunity =====

	body, status = sfAuthGet(t, base+"/services/data/v60.0/query/?q="+
		url.QueryEscape("SELECT Id, Name FROM Opportunity"), accessToken)
	if status != 200 {
		t.Fatalf("SOQL query Opportunity -> %d, want 200; body %s", status, body)
	}
	var oppResp map[string]any
	if err := json.Unmarshal([]byte(body), &oppResp); err != nil {
		t.Fatalf("unmarshal SOQL Opportunity: %v (body %s)", err, body)
	}
	oppRecords, ok := oppResp["records"].([]any)
	if !ok || len(oppRecords) < 1 {
		t.Fatalf("Opportunity records = %v, want at least 1 (seeded)", oppResp["records"])
	}
	oppRec := oppRecords[0].(map[string]any)
	oppAttrs := oppRec["attributes"].(map[string]any)
	if oppAttrs["type"] != "Opportunity" {
		t.Fatalf("Opportunity attributes.type = %v, want Opportunity", oppAttrs["type"])
	}
	oppId, _ := oppRec["Id"].(string)
	if !strings.HasPrefix(oppId, "006") {
		t.Fatalf("Opportunity Id = %q, want 006-prefixed", oppId)
	}

	// ===== DELETE Account =====

	body, status = sfAuthDelete(t, base+"/services/data/v60.0/sobjects/Account/"+accountID, accessToken)
	if status != 204 {
		t.Fatalf("DELETE Account -> %d, want 204; body %s", status, body)
	}

	// Verify deletion — retrieve should 404.
	_, status = sfAuthGet(t, base+"/services/data/v60.0/sobjects/Account/"+accountID, accessToken)
	if status != 404 {
		t.Fatalf("retrieve after DELETE -> %d, want 404", status)
	}

	// ===== 401 without bearer → SF array error envelope =====

	body, status = sfNoAuthGet(t, base+"/services/data/v60.0/query/?q="+
		url.QueryEscape("SELECT Id, Name FROM Account"))
	if status != 401 {
		t.Fatalf("no-auth query -> %d, want 401; body %s", status, body)
	}
	var errResp []any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal error resp: %v (body %s)", err, body)
	}
	if len(errResp) == 0 {
		t.Fatal("error envelope empty, want at least 1 element")
	}
	err0 := errResp[0].(map[string]any)
	if _, ok := err0["message"].(string); !ok {
		t.Fatalf("error message = %v, want string", err0["message"])
	}
	if _, ok := err0["errorCode"].(string); !ok {
		t.Fatalf("errorCode = %v, want string", err0["errorCode"])
	}

	// Sanity check: instance_url from the token response was valid.
	_ = instanceURL
}

// === Salesforce test helpers ===

func sfPostForm(t *testing.T, rawurl string, form url.Values) (string, int) {
	t.Helper()
	resp, err := http.PostForm(rawurl, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sfAuthGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sfNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sfAuthPostJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sfAuthPatchJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("PATCH", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sfAuthDelete(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
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
