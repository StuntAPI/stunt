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
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
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

// TestSalesforceStyleSOQL exercises the general SOQL engine: WHERE with
// comparators (=, >, >=), IN, LIKE, AND/OR, plus ORDER BY + LIMIT/OFFSET.
// All queries are scoped to the test's own accounts (Name LIKE 'Zq%') so the
// seeded fixture doesn't affect counts.
func TestSalesforceStyleSOQL(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "salesforce-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"salesforce": {Adapter: adapterDir},
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

	tb, status := sfPostForm(t, base+"/services/oauth2/token", url.Values{
		"grant_type": {"password"}, "client_id": {"test-consumer-key"},
		"client_secret": {"test-consumer-secret"}, "username": {"test@example.com"}, "password": {"testpassword"},
	})
	if status != 200 {
		t.Fatalf("token -> %d; body %s", status, tb)
	}
	var tok map[string]any
	json.Unmarshal([]byte(tb), &tok)
	accessToken, _ := tok["access_token"].(string)

	soql := func(q string) []string {
		b, st := sfAuthGet(t, base+"/services/data/v60.0/query/?q="+url.QueryEscape(q), accessToken)
		if st != 200 {
			t.Fatalf("SOQL %q -> %d; body %s", q, st, b)
		}
		var r map[string]any
		json.Unmarshal([]byte(b), &r)
		recs, _ := r["records"].([]any)
		out := []string{}
		for _, rec := range recs {
			out = append(out, rec.(map[string]any)["Name"].(string))
		}
		return out
	}

	accts := []map[string]any{
		{"Name": "ZqAcme", "Industry": "Tech", "AnnualRevenue": 1000000},
		{"Name": "ZqGlobex", "Industry": "Tech", "AnnualRevenue": 5000000},
		{"Name": "ZqInitech", "Industry": "Retail", "AnnualRevenue": 500000},
		{"Name": "ZqHooli", "Industry": "Tech", "AnnualRevenue": 10000000},
		{"Name": "ZqPiedPiper", "Industry": "Retail", "AnnualRevenue": 200000},
	}
	for _, a := range accts {
		if b, st := sfAuthPostJSON(t, base+"/services/data/v60.0/sobjects/Account", accessToken, a); st != 201 {
			t.Fatalf("create %v -> %d; body %s", a["Name"], st, b)
		}
	}

	must := func(got []string, want []string, q string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("SOQL %q -> %v, want %v", q, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("SOQL %q -> %v, want %v", q, got, want)
			}
		}
	}

	// = comparator + AND + LIKE scoping.
	got := soql("SELECT Name FROM Account WHERE Name LIKE 'Zq%' AND Industry = 'Tech'")
	must(got, []string{"ZqAcme", "ZqGlobex", "ZqHooli"}, "Tech AND like")

	// Numeric > comparator.
	got = soql("SELECT Name FROM Account WHERE Name LIKE 'Zq%' AND AnnualRevenue > 1000000")
	must(got, []string{"ZqGlobex", "ZqHooli"}, "revenue>1M")

	// IN.
	got = soql("SELECT Name FROM Account WHERE Name LIKE 'Zq%' AND Industry IN ('Retail')")
	must(got, []string{"ZqInitech", "ZqPiedPiper"}, "IN Retail")

	// AND with >= (numeric) — Acme (1M) excluded, Globex+Hooli included.
	got = soql("SELECT Name FROM Account WHERE Name LIKE 'Zq%' AND Industry = 'Tech' AND AnnualRevenue >= 5000000")
	must(got, []string{"ZqGlobex", "ZqHooli"}, "Tech AND revenue>=5M")

	// OR.
	got = soql("SELECT Name FROM Account WHERE Name = 'ZqHooli' OR Name = 'ZqPiedPiper'")
	must(got, []string{"ZqHooli", "ZqPiedPiper"}, "OR")

	// ORDER BY DESC + LIMIT.
	got = soql("SELECT Name FROM Account WHERE Name LIKE 'Zq%' ORDER BY AnnualRevenue DESC LIMIT 2")
	must(got, []string{"ZqHooli", "ZqGlobex"}, "order desc limit")

	// ORDER BY ASC + LIMIT + OFFSET.
	got = soql("SELECT Name FROM Account WHERE Name LIKE 'Zq%' ORDER BY Name LIMIT 2 OFFSET 1")
	must(got, []string{"ZqGlobex", "ZqHooli"}, "order name limit offset")
}

// TestSalesforceStyleLiveTimestamps verifies record CreatedDate and OAuth
// issued_at are live clock values rather than fixed synthetic dates.
func TestSalesforceStyleLiveTimestamps(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "salesforce-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"salesforce": {Adapter: adapterDir},
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

	// OAuth issued_at: epoch milliseconds within a minute of the test clock.
	start := time.Now().UTC()
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
	issuedAt, err := strconv.ParseInt(tokenResp["issued_at"].(string), 10, 64)
	if err != nil {
		t.Fatalf("issued_at %v not epoch millis: %v", tokenResp["issued_at"], err)
	}
	issued := time.UnixMilli(issuedAt)
	if issued.Before(start.Add(-time.Minute)) || issued.After(time.Now().Add(time.Minute)) {
		t.Fatalf("issued_at %v not live (start %v)", issued, start)
	}
	accessToken := tokenResp["access_token"].(string)

	// Record CreatedDate: RFC 3339 within a minute of the test clock.
	body, status = sfAuthPostJSON(t, base+"/services/data/v60.0/sobjects/Account", accessToken, map[string]any{
		"Name": "Clock Test Account",
	})
	if status != 201 {
		t.Fatalf("create account -> %d, want 201; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal create: %v (body %s)", err, body)
	}
	accountID, _ := createResp["id"].(string)
	if accountID == "" {
		t.Fatalf("account id = %v, want non-empty", createResp["id"])
	}

	body, status = sfAuthGet(t, base+"/services/data/v60.0/sobjects/Account/"+accountID, accessToken)
	if status != 200 {
		t.Fatalf("get account -> %d, want 200; body %s", status, body)
	}
	var account map[string]any
	if err := json.Unmarshal([]byte(body), &account); err != nil {
		t.Fatalf("unmarshal account: %v (body %s)", err, body)
	}
	createdDate, _ := account["CreatedDate"].(string)
	ts, err := time.Parse(time.RFC3339, createdDate)
	if err != nil {
		t.Fatalf("CreatedDate %q is not RFC 3339: %v", createdDate, err)
	}
	if ts.Before(start.Add(-time.Minute)) || ts.After(time.Now().Add(time.Minute)) {
		t.Fatalf("CreatedDate %v not live (start %v)", ts, start)
	}
}

// TestSalesforceStyleSoftDeleteQueryAll proves the recycle-bin model: DELETE
// flags the row IsDeleted instead of destroying it — plain retrieve/PATCH
// 404 (ENTITY_IS_DELETED for mutations), /query excludes the row, and
// /queryAll surfaces it with IsDeleted true until it is wanted no more.
func TestSalesforceStyleSoftDeleteQueryAll(t *testing.T) {
	adapterDir := err2(filepath.Abs(filepath.Join("..", "..", "adapters", "salesforce-style")))
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"salesforce": {Adapter: adapterDir},
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

	tb, status := sfPostForm(t, base+"/services/oauth2/token", url.Values{
		"grant_type": {"password"}, "client_id": {"k"}, "client_secret": {"s"},
		"username": {"u@example.com"}, "password": {"p"},
	})
	if status != 200 {
		t.Fatalf("token -> %d; body %s", status, tb)
	}
	var tok map[string]any
	json.Unmarshal([]byte(tb), &tok)
	accessToken, _ := tok["access_token"].(string)

	mk := func(name string) string {
		t.Helper()
		b, st := sfAuthPostJSON(t, base+"/services/data/v60.0/sobjects/Account", accessToken,
			map[string]any{"Name": name})
		if st != 201 {
			t.Fatalf("create %s -> %d; body %s", name, st, b)
		}
		var cr map[string]any
		json.Unmarshal([]byte(b), &cr)
		return cr["id"].(string)
	}
	gone := mk("Bin Me")
	kept := mk("Keep Me")

	// ===== DELETE -> soft delete (204) =====
	if b, st := sfAuthDelete(t, base+"/services/data/v60.0/sobjects/Account/"+gone, accessToken); st != 204 {
		t.Fatalf("DELETE -> %d, want 204; body %s", st, b)
	}

	// Retrieve 404s like a missing record.
	if _, st := sfAuthGet(t, base+"/services/data/v60.0/sobjects/Account/"+gone, accessToken); st != 404 {
		t.Fatalf("retrieve deleted -> %d, want 404", st)
	}
	// Kept record is untouched.
	if _, st := sfAuthGet(t, base+"/services/data/v60.0/sobjects/Account/"+kept, accessToken); st != 200 {
		t.Fatalf("retrieve live -> %d, want 200", st)
	}

	// PATCH on the deleted record -> 404 ENTITY_IS_DELETED.
	body, st := sfAuthPatchJSON(t, base+"/services/data/v60.0/sobjects/Account/"+gone, accessToken,
		map[string]any{"Name": "nope"})
	if st != 404 {
		t.Fatalf("PATCH deleted -> %d, want 404; body %s", st, body)
	}
	if cat := sfErrCode(t, body); cat != "ENTITY_IS_DELETED" {
		t.Fatalf("PATCH deleted errorCode = %v, want ENTITY_IS_DELETED", cat)
	}
	// Double DELETE -> same error.
	if body, st = sfAuthDelete(t, base+"/services/data/v60.0/sobjects/Account/"+gone, accessToken); st != 404 {
		t.Fatalf("double DELETE -> %d, want 404; body %s", st, body)
	} else if cat := sfErrCode(t, body); cat != "ENTITY_IS_DELETED" {
		t.Fatalf("double DELETE errorCode = %v, want ENTITY_IS_DELETED", cat)
	}

	soql := func(endpoint, q string) []map[string]any {
		t.Helper()
		b, st2 := sfAuthGet(t, base+"/services/data/v60.0/"+endpoint+"?q="+url.QueryEscape(q), accessToken)
		if st2 != 200 {
			t.Fatalf("%s %q -> %d; body %s", endpoint, q, st2, b)
		}
		var r map[string]any
		json.Unmarshal([]byte(b), &r)
		recs, _ := r["records"].([]any)
		out := []map[string]any{}
		for _, rec := range recs {
			out = append(out, rec.(map[string]any))
		}
		return out
	}

	// /query excludes the deleted row but keeps live + seeded accounts.
	for _, rec := range soql("query", "SELECT Id, IsDeleted FROM Account") {
		if rec["Id"] == gone {
			t.Fatalf("deleted record %s leaked into /query", gone)
		}
		if rec["IsDeleted"] == true {
			t.Fatalf("record %v from /query has IsDeleted=true", rec["Id"])
		}
	}

	// /queryAll includes it, flagged IsDeleted=true.
	qa := soql("queryAll", "SELECT Id, IsDeleted FROM Account WHERE IsDeleted = true")
	if len(qa) != 1 || qa[0]["Id"] != gone {
		t.Fatalf("queryAll IsDeleted=true -> %d records, want exactly [%s]", len(qa), gone)
	}

	// queryAll's default view still contains live rows; IsDeleted=false works.
	live := soql("queryAll", "SELECT Id FROM Account WHERE IsDeleted = false AND Name = 'Keep Me'")
	if len(live) != 1 || live[0]["Id"] != kept {
		t.Fatalf("queryAll IsDeleted=false -> %v, want [%s]", live, kept)
	}

	// /query with the same WHERE cannot see the bin (empty).
	if recs := soql("query", "SELECT Id FROM Account WHERE IsDeleted = true"); len(recs) != 0 {
		t.Fatalf("query IsDeleted=true -> %d records, want 0 (bin invisible to /query)", len(recs))
	}

	// DeletedDate is stamped and IsDeleted selectable on the bin row.
	dt := soql("queryAll", "SELECT Id, IsDeleted, DeletedDate FROM Account WHERE Id = '"+gone+"'")
	if len(dt) != 1 {
		t.Fatalf("queryAll by Id -> %d records, want 1", len(dt))
	}
	if dd, _ := dt[0]["DeletedDate"].(string); dd == "" {
		t.Fatalf("deleted record DeletedDate = %v, want a timestamp", dt[0]["DeletedDate"])
	}

	// Failure path: DELETE unknown id -> 404 NOT_FOUND envelope.
	body, st = sfAuthDelete(t, base+"/services/data/v60.0/sobjects/Account/001doesnotexist", accessToken)
	if st != 404 {
		t.Fatalf("DELETE unknown -> %d, want 404; body %s", st, body)
	}
	if cat := sfErrCode(t, body); cat != "NOT_FOUND" {
		t.Fatalf("DELETE unknown errorCode = %v, want NOT_FOUND", cat)
	}
}

// sfErrCode digs errorCode out of a Salesforce array error envelope.
func sfErrCode(t *testing.T, body string) string {
	t.Helper()
	var arr []any
	if err := json.Unmarshal([]byte(body), &arr); err != nil || len(arr) == 0 {
		t.Fatalf("unmarshal SF error %q: %v", body, err)
	}
	code, _ := arr[0].(map[string]any)["errorCode"].(string)
	return code
}

// err2 fails the test on a non-nil error and returns the wrapped value.
func err2(v string, err error) string {
	if err != nil {
		panic(err)
	}
	return v
}

// === P3 additions: refresh reuse, upsert, composite refs, queryMore, collections ===

// sfStart boots a one-service engine over the salesforce-style adapter and
// returns its base URL. Cleanup is registered on t.
func sfStart(t *testing.T) string {
	t.Helper()
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "salesforce-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"salesforce": {Adapter: adapterDir},
		},
	}
	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		e.Close()
		t.Fatalf("ServeForTest: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		e.Close()
	})
	time.Sleep(50 * time.Millisecond)
	return addrs["salesforce"]
}

// sfToken mints a bearer token via the password grant.
func sfToken(t *testing.T, base string) string {
	t.Helper()
	body, status := sfPostForm(t, base+"/services/oauth2/token", url.Values{
		"grant_type": {"password"}, "client_id": {"test-consumer-key"},
		"client_secret": {"test-consumer-secret"}, "username": {"test@example.com"},
		"password": {"testpassword"},
	})
	if status != 200 {
		t.Fatalf("oauth token -> %d, want 200; body %s", status, body)
	}
	var tok map[string]any
	if err := json.Unmarshal([]byte(body), &tok); err != nil {
		t.Fatalf("unmarshal token resp: %v (body %s)", err, body)
	}
	token, _ := tok["access_token"].(string)
	if token == "" {
		t.Fatalf("access_token = %v, want non-empty", tok["access_token"])
	}
	return token
}

// sfAuthGetHeaders is sfAuthGet with extra request headers.
func sfAuthGetHeaders(t *testing.T, rawurl, token string, headers map[string]string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// sfQuery runs a SOQL query and returns the parsed response envelope.
func sfQuery(t *testing.T, base, token, endpoint, soql string, headers map[string]string) map[string]any {
	t.Helper()
	b, st := sfAuthGetHeaders(t, base+"/services/data/v60.0/"+endpoint+"?q="+url.QueryEscape(soql), token, headers)
	if st != 200 {
		t.Fatalf("%s %q -> %d; body %s", endpoint, soql, st, b)
	}
	var r map[string]any
	if err := json.Unmarshal([]byte(b), &r); err != nil {
		t.Fatalf("unmarshal %s %q: %v (body %s)", endpoint, soql, err, b)
	}
	return r
}

// TestSalesforceStyleOAuthRefreshReuse proves real-Salesforce refresh
// semantics: refresh tokens are long-lived and reusable (redemption never
// invalidates them), the refresh response omits refresh_token, access tokens
// rotate per grant, and old access tokens stay valid until their own expiry.
// Bogus refresh tokens are rejected with invalid_grant.
func TestSalesforceStyleOAuthRefreshReuse(t *testing.T) {
	base := sfStart(t)

	tb, status := sfPostForm(t, base+"/services/oauth2/token", url.Values{
		"grant_type": {"password"}, "client_id": {"k"}, "client_secret": {"s"},
		"username": {"u@example.com"}, "password": {"p"},
	})
	if status != 200 {
		t.Fatalf("password grant -> %d; body %s", status, tb)
	}
	var first map[string]any
	json.Unmarshal([]byte(tb), &first)
	access1, _ := first["access_token"].(string)
	refresh, _ := first["refresh_token"].(string)
	if access1 == "" || refresh == "" {
		t.Fatalf("token response missing access_token/refresh_token: %v", first)
	}

	refreshGrant := func(rt string) (map[string]any, string, int) {
		b, st := sfPostForm(t, base+"/services/oauth2/token", url.Values{
			"grant_type": {"refresh_token"}, "client_id": {"k"},
			"client_secret": {"s"}, "refresh_token": {rt},
		})
		var m map[string]any
		json.Unmarshal([]byte(b), &m)
		return m, b, st
	}

	// First redemption: new access token, no refresh_token echoed.
	second, raw2, st := refreshGrant(refresh)
	if st != 200 {
		t.Fatalf("refresh grant -> %d; body %s", st, raw2)
	}
	access2, _ := second["access_token"].(string)
	if access2 == "" || access2 == access1 {
		t.Fatalf("refresh access_token = %q, want a fresh token != %q", access2, access1)
	}
	if _, echoed := second["refresh_token"]; echoed {
		t.Fatalf("refresh response echoed refresh_token %v, want it omitted (real API keeps the same token)", second["refresh_token"])
	}

	// Both access tokens are valid sessions (old one lives out its TTL).
	for _, tok := range []string{access1, access2} {
		if _, st := sfAuthGet(t, base+"/services/data/v60.0/sobjects", tok); st != 200 {
			t.Fatalf("describe with access token -> %d, want 200", st)
		}
	}

	// Second redemption with the SAME refresh token: reuse-safe.
	third, raw3, st := refreshGrant(refresh)
	if st != 200 {
		t.Fatalf("refresh grant (reuse) -> %d; body %s", st, raw3)
	}
	access3, _ := third["access_token"].(string)
	if access3 == "" || access3 == access2 {
		t.Fatalf("reuse access_token = %q, want a fresh token != %q", access3, access2)
	}
	if _, st := sfAuthGet(t, base+"/services/data/v60.0/sobjects", access3); st != 200 {
		t.Fatalf("describe with reused-grant token -> %d, want 200", st)
	}

	// Failure path: bogus refresh token -> 400 invalid_grant.
	bad, badRaw, st := refreshGrant("refresh_bogusbogus")
	if st != 400 {
		t.Fatalf("bogus refresh -> %d, want 400; body %s", st, badRaw)
	}
	if bad["error"] != "invalid_grant" {
		t.Fatalf("bogus refresh error = %v, want invalid_grant", bad["error"])
	}
}

// TestSalesforceStyleUpsert covers PATCH /sobjects/{type}/{extIdField}/{extIdValue}:
// insert on no match (external ID stamped from the URL), update on match
// (created:false, same Id), 300 with the matching records when the external
// ID is ambiguous, and REQUIRED_FIELD_MISSING when inserting without Name.
func TestSalesforceStyleUpsert(t *testing.T) {
	base := sfStart(t)
	token := sfToken(t, base)
	upsertURL := base + "/services/data/v60.0/sobjects/Account/ExtKey__c/"

	// No match -> insert.
	body, st := sfAuthPatchJSON(t, upsertURL+"UPS-001", token, map[string]any{
		"Name": "Upsert One", "Industry": "Tech",
	})
	if st != 201 {
		t.Fatalf("upsert insert -> %d, want 201; body %s", st, body)
	}
	var created map[string]any
	json.Unmarshal([]byte(body), &created)
	if created["created"] != true || created["success"] != true {
		t.Fatalf("upsert insert = %v, want created:true success:true", created)
	}
	id1, _ := created["id"].(string)
	if !strings.HasPrefix(id1, "001") {
		t.Fatalf("upsert id = %q, want 001-prefixed", id1)
	}

	// The external ID from the URL landed on the record.
	body, st = sfAuthGet(t, base+"/services/data/v60.0/sobjects/Account/"+id1, token)
	if st != 200 {
		t.Fatalf("retrieve upserted -> %d; body %s", st, body)
	}
	var rec map[string]any
	json.Unmarshal([]byte(body), &rec)
	if rec["ExtKey__c"] != "UPS-001" {
		t.Fatalf("ExtKey__c = %v, want UPS-001", rec["ExtKey__c"])
	}

	// Match -> update, same Id, created:false.
	body, st = sfAuthPatchJSON(t, upsertURL+"UPS-001", token, map[string]any{
		"Name": "Upsert One Renamed",
	})
	if st != 201 {
		t.Fatalf("upsert update -> %d, want 201; body %s", st, body)
	}
	var updated map[string]any
	json.Unmarshal([]byte(body), &updated)
	if updated["created"] != false {
		t.Fatalf("upsert update created = %v, want false", updated["created"])
	}
	if updated["id"] != any(id1) {
		t.Fatalf("upsert update id = %v, want %s (same record)", updated["id"], id1)
	}

	body, st = sfAuthGet(t, base+"/services/data/v60.0/sobjects/Account/"+id1, token)
	if st != 200 {
		t.Fatalf("retrieve updated -> %d; body %s", st, body)
	}
	rec = nil
	json.Unmarshal([]byte(body), &rec)
	if rec["Name"] != "Upsert One Renamed" {
		t.Fatalf("updated Name = %v, want 'Upsert One Renamed'", rec["Name"])
	}
	createdDate, _ := rec["CreatedDate"].(string)
	if createdDate == "" {
		t.Fatal("CreatedDate missing after update path")
	}

	// Distinct external ID -> a different record.
	body, st = sfAuthPatchJSON(t, upsertURL+"UPS-002", token, map[string]any{
		"Name": "Upsert Two",
	})
	if st != 201 {
		t.Fatalf("upsert second insert -> %d; body %s", st, body)
	}
	var created2 map[string]any
	json.Unmarshal([]byte(body), &created2)
	id2, _ := created2["id"].(string)
	if id2 == "" || id2 == id1 {
		t.Fatalf("second upsert id = %q, want a new record", id2)
	}

	// Ambiguous external ID (two live matches) -> 300 with both records.
	for _, name := range []string{"Dup Alpha", "Dup Beta"} {
		if b, s := sfAuthPostJSON(t, base+"/services/data/v60.0/sobjects/Account", token, map[string]any{
			"Name": name, "ExtKey__c": "UPS-DUP",
		}); s != 201 {
			t.Fatalf("create dup %s -> %d; body %s", name, s, b)
		}
	}
	body, st = sfAuthPatchJSON(t, upsertURL+"UPS-DUP", token, map[string]any{
		"Name": "Dup Gamma",
	})
	if st != 300 {
		t.Fatalf("upsert ambiguous -> %d, want 300; body %s", st, body)
	}
	var dups []any
	if err := json.Unmarshal([]byte(body), &dups); err != nil || len(dups) != 2 {
		t.Fatalf("upsert ambiguous body = %s, want array of 2 records", body)
	}

	// Failure path: insert without Name -> 400 REQUIRED_FIELD_MISSING.
	body, st = sfAuthPatchJSON(t, upsertURL+"UPS-003", token, map[string]any{
		"Industry": "Nothing",
	})
	if st != 400 {
		t.Fatalf("upsert missing Name -> %d, want 400; body %s", st, body)
	}
	if cat := sfErrCode(t, body); cat != "REQUIRED_FIELD_MISSING" {
		t.Fatalf("upsert missing Name errorCode = %v, want REQUIRED_FIELD_MISSING", cat)
	}
}

// TestSalesforceStyleCompositeRefs proves referenceId substitution inside
// composite sub-requests: "@{ref}" in URL strings and body values, the bare
// "{ref}" form, dot paths into response fields, and the 400 that an
// unresolvable reference produces for the whole request.
func TestSalesforceStyleCompositeRefs(t *testing.T) {
	base := sfStart(t)
	token := sfToken(t, base)

	payload := map[string]any{
		"compositeRequest": []any{
			map[string]any{
				"method": "POST", "url": "/services/data/v60.0/sobjects/Account",
				"referenceId": "refAcct", "body": map[string]any{"Name": "Ref Corp"},
			},
			map[string]any{
				"method": "POST", "url": "/services/data/v60.0/sobjects/Contact",
				"referenceId": "refContact",
				"body":        map[string]any{"Name": "Ref Person", "AccountId": "@{refAcct}"},
			},
			map[string]any{
				"method":      "GET",
				"url":         "/services/data/v60.0/sobjects/Contact/@{refContact}",
				"referenceId": "refFetchAt",
			},
			map[string]any{
				"method":      "GET",
				"url":         "/services/data/v60.0/sobjects/Account/{refAcct}",
				"referenceId": "refFetchBare",
			},
			map[string]any{
				"method":      "GET",
				"url":         "/services/data/v60.0/sobjects/Account/@{refAcct.id}",
				"referenceId": "refFetchPath",
			},
		},
	}
	body, st := sfAuthPostJSON(t, base+"/services/data/v60.0/composite", token, payload)
	if st != 200 {
		t.Fatalf("composite -> %d, want 200; body %s", st, body)
	}
	var comp map[string]any
	if err := json.Unmarshal([]byte(body), &comp); err != nil {
		t.Fatalf("unmarshal composite: %v (body %s)", err, body)
	}
	responses, ok := comp["compositeResponse"].([]any)
	if !ok || len(responses) != 5 {
		t.Fatalf("compositeResponse = %v, want 5 entries", comp["compositeResponse"])
	}
	sub := func(i int) map[string]any {
		t.Helper()
		m, _ := responses[i].(map[string]any)
		if m == nil {
			t.Fatalf("compositeResponse[%d] is not an object: %v", i, responses[i])
		}
		return m
	}
	subBody := func(i int) map[string]any {
		t.Helper()
		m, _ := sub(i)["body"].(map[string]any)
		if m == nil {
			t.Fatalf("compositeResponse[%d].body is not an object: %v", i, sub(i))
		}
		return m
	}
	subStatus := func(i int) int {
		t.Helper()
		f, _ := sub(i)["httpStatusCode"].(float64)
		return int(f)
	}

	if subStatus(0) != 201 {
		t.Fatalf("create Account sub-request -> %d, want 201", subStatus(0))
	}
	accountId, _ := subBody(0)["id"].(string)
	if accountId == "" {
		t.Fatalf("create Account body.id = %v, want non-empty", subBody(0)["id"])
	}
	if subStatus(1) != 201 {
		t.Fatalf("create Contact sub-request -> %d, want 201", subStatus(1))
	}
	contactId, _ := subBody(1)["id"].(string)

	// Body-value reference: the Contact's AccountId is the created Account.
	// URL reference (@{...}): fetch the Contact back.
	if subStatus(2) != 200 {
		t.Fatalf("fetch Contact via @{ref} -> %d, want 200", subStatus(2))
	}
	fetchedContact := subBody(2)
	if fetchedContact["Id"] != any(contactId) {
		t.Fatalf("fetched Contact Id = %v, want %s", fetchedContact["Id"], contactId)
	}
	if fetchedContact["AccountId"] != any(accountId) {
		t.Fatalf("Contact AccountId = %v, want %s (@{refAcct} substituted)", fetchedContact["AccountId"], accountId)
	}

	// Bare {ref} and dot-path @{ref.id} forms resolve the same Account.
	if subStatus(3) != 200 || subStatus(4) != 200 {
		t.Fatalf("fetch via {ref}/@{ref.id} -> %d/%d, want 200/200", subStatus(3), subStatus(4))
	}
	if subBody(3)["Id"] != any(accountId) || subBody(4)["Id"] != any(accountId) {
		t.Fatalf("bare/path fetch Ids = %v/%v, want %s", subBody(3)["Id"], subBody(4)["Id"], accountId)
	}

	// Failure path: an unresolvable reference rejects the whole composite
	// (real API behavior) with the standard error envelope.
	body, st = sfAuthPostJSON(t, base+"/services/data/v60.0/composite", token, map[string]any{
		"compositeRequest": []any{
			map[string]any{
				"method": "POST", "url": "/services/data/v60.0/sobjects/Account",
				"referenceId": "refOk", "body": map[string]any{"Name": "Ref Two"},
			},
			map[string]any{
				"method":      "GET",
				"url":         "/services/data/v60.0/sobjects/Account/@{doesNotExist}",
				"referenceId": "refBad",
			},
		},
	})
	if st != 400 {
		t.Fatalf("composite with unknown ref -> %d, want 400; body %s", st, body)
	}
	if cat := sfErrCode(t, body); cat != "INVALID_INPUT" {
		t.Fatalf("unknown ref errorCode = %v, want INVALID_INPUT", cat)
	}
}

// TestSalesforceStyleQueryMore proves nextRecordsUrl pagination on /query:
// results above the 200-record batch size split into done:false pages with a
// usable nextRecordsUrl, the locator is single-use, batchsize can widen the
// page via Sforce-Query-Options, and unknown locators 400 INVALID_QUERY_LOCATOR.
func TestSalesforceStyleQueryMore(t *testing.T) {
	base := sfStart(t)
	token := sfToken(t, base)

	// 205 scoped accounts -> two batches at the default 200 batch size.
	const total = 205
	for i := 0; i < total; i++ {
		name := "Qm Account " + strconv.Itoa(i)
		if b, st := sfAuthPostJSON(t, base+"/services/data/v60.0/sobjects/Account", token,
			map[string]any{"Name": name}); st != 201 {
			t.Fatalf("create %s -> %d; body %s", name, st, b)
		}
	}
	soql := "SELECT Id FROM Account WHERE Name LIKE 'Qm %'"

	// Sforce-Query-Options batchsize=2000: one page, done:true.
	wide := sfQuery(t, base, token, "query", soql, map[string]string{"Sforce-Query-Options": "batchsize=2000"})
	if wide["done"] != true {
		t.Fatalf("batchsize=2000 done = %v, want true", wide["done"])
	}
	if n := len(wide["records"].([]any)); n != total {
		t.Fatalf("batchsize=2000 records = %d, want %d", n, total)
	}

	// Default batch size 200: first page done:false + nextRecordsUrl.
	page1 := sfQuery(t, base, token, "query", soql, nil)
	if page1["done"] != false {
		t.Fatalf("first page done = %v, want false", page1["done"])
	}
	if n := len(page1["records"].([]any)); n != 200 {
		t.Fatalf("first page records = %d, want 200 (default batch size)", n)
	}
	if ts, _ := page1["totalSize"].(float64); int(ts) != 200 {
		t.Fatalf("first page totalSize = %v, want 200 (records in this batch)", page1["totalSize"])
	}
	next, _ := page1["nextRecordsUrl"].(string)
	if next == "" || !strings.HasPrefix(next, "/services/data/v60.0/query/") {
		t.Fatalf("nextRecordsUrl = %q, want /services/data/v60.0/query/<locator>", next)
	}

	// Follow the cursor -> the remaining 5 records, done:true.
	body, st := sfAuthGet(t, base+next, token)
	if st != 200 {
		t.Fatalf("queryMore -> %d, want 200; body %s", st, body)
	}
	var page2 map[string]any
	if err := json.Unmarshal([]byte(body), &page2); err != nil {
		t.Fatalf("unmarshal queryMore: %v (body %s)", err, body)
	}
	if page2["done"] != true {
		t.Fatalf("second page done = %v, want true", page2["done"])
	}
	if _, has := page2["nextRecordsUrl"]; has {
		t.Fatalf("second page nextRecordsUrl = %v, want absent", page2["nextRecordsUrl"])
	}
	if n := len(page2["records"].([]any)); n != 5 {
		t.Fatalf("second page records = %d, want 5", n)
	}

	// The two pages partition the result set (no overlap, nothing missing).
	ids := map[string]bool{}
	for _, p := range []map[string]any{page1, page2} {
		for _, r := range p["records"].([]any) {
			id, _ := r.(map[string]any)["Id"].(string)
			ids[id] = true
		}
	}
	if len(ids) != total {
		t.Fatalf("paged results contain %d distinct Ids, want %d", len(ids), total)
	}

	// Locators are single-use: replaying the consumed cursor 400s.
	if body, st = sfAuthGet(t, base+next, token); st != 400 {
		t.Fatalf("replay locator -> %d, want 400; body %s", st, body)
	} else if cat := sfErrCode(t, body); cat != "INVALID_QUERY_LOCATOR" {
		t.Fatalf("replay locator errorCode = %v, want INVALID_QUERY_LOCATOR", cat)
	}

	// Unknown locator -> 400 INVALID_QUERY_LOCATOR.
	body, st = sfAuthGet(t, base+"/services/data/v60.0/query/01gbogusbogus-200", token)
	if st != 400 {
		t.Fatalf("unknown locator -> %d, want 400; body %s", st, body)
	}
	if cat := sfErrCode(t, body); cat != "INVALID_QUERY_LOCATOR" {
		t.Fatalf("unknown locator errorCode = %v, want INVALID_QUERY_LOCATOR", cat)
	}
}

// TestSalesforceStyleCollectionsBulk covers SObject Collections bulk DML on
// /composite/sobjects: insert/update/delete with per-record lenient results,
// allOrNone=true atomic failure with rollback, and the 400 failure paths.
func TestSalesforceStyleCollectionsBulk(t *testing.T) {
	base := sfStart(t)
	token := sfToken(t, base)
	soql := "SELECT Id, Name FROM Account WHERE Name LIKE 'ColBulk%'"
	collected := func() []map[string]any {
		t.Helper()
		r := sfQuery(t, base, token, "query", soql, nil)
		out := []map[string]any{}
		for _, rec := range r["records"].([]any) {
			out = append(out, rec.(map[string]any))
		}
		return out
	}

	// Lenient insert: one good record, one missing Name.
	body, st := sfAuthPostJSON(t, base+"/services/data/v60.0/composite/sobjects", token, map[string]any{
		"allOrNone": false,
		"records": []any{
			map[string]any{"attributes": map[string]any{"type": "Account"}, "Name": "ColBulk A"},
			map[string]any{"attributes": map[string]any{"type": "Account"}, "Industry": "NoName"},
		},
	})
	if st != 200 {
		t.Fatalf("lenient insert -> %d, want 200; body %s", st, body)
	}
	var results []any
	if err := json.Unmarshal([]byte(body), &results); err != nil || len(results) != 2 {
		t.Fatalf("lenient insert body = %s, want array of 2", body)
	}
	r0 := results[0].(map[string]any)
	r1 := results[1].(map[string]any)
	if r0["success"] != true || r0["created"] != true {
		t.Fatalf("good insert result = %v, want success+created", r0)
	}
	if id, _ := r0["id"].(string); !strings.HasPrefix(id, "001") {
		t.Fatalf("inserted id = %q, want 001-prefixed", id)
	}
	if r1["success"] != false || r1["id"] != "" {
		t.Fatalf("bad insert result = %v, want success:false id:\"\"", r1)
	}
	errs1, _ := r1["errors"].([]any)
	if len(errs1) != 1 {
		t.Fatalf("bad insert errors = %v, want 1 entry", r1["errors"])
	}
	if errs1[0].(map[string]any)["statusCode"] != "REQUIRED_FIELD_MISSING" {
		t.Fatalf("bad insert statusCode = %v, want REQUIRED_FIELD_MISSING", errs1[0])
	}

	// allOrNone insert with a failure: 400, and the good record is rolled back.
	body, st = sfAuthPostJSON(t, base+"/services/data/v60.0/composite/sobjects", token, map[string]any{
		"allOrNone": true,
		"records": []any{
			map[string]any{"attributes": map[string]any{"type": "Account"}, "Name": "ColBulk B"},
			map[string]any{"attributes": map[string]any{"type": "Account"}, "Industry": "NoName"},
		},
	})
	if st != 400 {
		t.Fatalf("strict insert -> %d, want 400; body %s", st, body)
	}
	if cat := sfErrCode(t, body); cat != "REQUIRED_FIELD_MISSING" {
		t.Fatalf("strict insert errorCode = %v, want REQUIRED_FIELD_MISSING", cat)
	}
	if got := len(collected()); got != 1 {
		t.Fatalf("after strict rollback %d ColBulk records, want 1 (only the lenient insert)", got)
	}

	// Bulk update: rename the surviving record; a missing Id is reported.
	target := collected()[0]
	targetId, _ := target["Id"].(string)
	body, st = sfAuthPatchJSON(t, base+"/services/data/v60.0/composite/sobjects", token, map[string]any{
		"allOrNone": false,
		"records": []any{
			map[string]any{"attributes": map[string]any{"type": "Account"}, "Id": targetId, "Name": "ColBulk A Renamed"},
			map[string]any{"attributes": map[string]any{"type": "Account"}, "Name": "No Id"},
		},
	})
	if st != 200 {
		t.Fatalf("bulk update -> %d, want 200; body %s", st, body)
	}
	results = nil
	if err := json.Unmarshal([]byte(body), &results); err != nil || len(results) != 2 {
		t.Fatalf("bulk update body = %s, want array of 2", body)
	}
	u0 := results[0].(map[string]any)
	u1 := results[1].(map[string]any)
	if u0["success"] != true || u0["id"] != any(targetId) {
		t.Fatalf("update result = %v, want success for %s", u0, targetId)
	}
	if u1["success"] != false {
		t.Fatalf("no-Id update result = %v, want success:false", u1)
	}
	if u1errs := u1["errors"].([]any); u1errs[0].(map[string]any)["statusCode"] != "MISSING_ARGUMENT" {
		t.Fatalf("no-Id update statusCode = %v, want MISSING_ARGUMENT", u1errs[0])
	}
	if got := collected(); len(got) != 1 || got[0]["Name"] != "ColBulk A Renamed" {
		t.Fatalf("after bulk update = %v, want [ColBulk A Renamed]", got)
	}

	// Bulk delete, then the strict unknown-Id failure path.
	body, st = sfAuthDeleteWithQuery(t, base+"/services/data/v60.0/composite/sobjects?ids="+targetId, token)
	if st != 200 {
		t.Fatalf("bulk delete -> %d, want 200; body %s", st, body)
	}
	results = nil
	json.Unmarshal([]byte(body), &results)
	d0, _ := results[0].(map[string]any)
	if d0["success"] != true || d0["id"] != any(targetId) {
		t.Fatalf("delete result = %v, want success for %s", d0, targetId)
	}
	if got := len(collected()); got != 0 {
		t.Fatalf("after bulk delete %d ColBulk records, want 0", got)
	}

	body, st = sfAuthDeleteWithQuery(t, base+"/services/data/v60.0/composite/sobjects?ids=001doesnotexist&allOrNone=true", token)
	if st != 400 {
		t.Fatalf("strict delete unknown Id -> %d, want 400; body %s", st, body)
	}
	if cat := sfErrCode(t, body); cat != "NOT_FOUND" {
		t.Fatalf("strict delete errorCode = %v, want NOT_FOUND", cat)
	}

	// Failure path: empty records array.
	body, st = sfAuthPostJSON(t, base+"/services/data/v60.0/composite/sobjects", token, map[string]any{
		"allOrNone": false, "records": []any{},
	})
	if st != 400 {
		t.Fatalf("empty records -> %d, want 400; body %s", st, body)
	}
	if cat := sfErrCode(t, body); cat != "INVALID_INPUT" {
		t.Fatalf("empty records errorCode = %v, want INVALID_INPUT", cat)
	}
}

// sfAuthDeleteWithQuery issues a DELETE with no body (query-string-only
// requests like Collections delete).
func sfAuthDeleteWithQuery(t *testing.T, rawurl, token string) (string, int) {
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

// sfPrivateKey parses the fixed mock keypair the JWT adapters share: the
// private half lives here in tests (google_iam_style_test.go carries the
// PEM), the Salesforce adapter verifies assertions against its public
// half — the "connected-app certificate" a real client registers out of
// band.
func sfPrivateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	block, _ := pem.Decode([]byte(googleIAMPrivateKeyPEM))
	if block == nil {
		t.Fatal("bad test key PEM")
	}
	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	return priv
}

// sfSignAssertion builds a real RS256 jwt-bearer assertion with
// Salesforce's claim set: iss = the connected app's consumer key,
// sub = the user the session is for, aud = a Salesforce login host.
func sfSignAssertion(t *testing.T, key *rsa.PrivateKey, iss, sub, aud string, iat, exp int64) string {
	t.Helper()
	header := `{"alg":"RS256","typ":"JWT"}`
	payload := `{"iss":"` + iss + `","sub":"` + sub + `","aud":"` + aud + `",` +
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

// TestSalesforceStyleJWTBearerGrant exercises the RFC 7523 flow: a valid
// RS256 assertion mints a working session (no refresh token — the client
// mints a fresh assertion instead), and forged, expired, wrong-audience,
// and malformed assertions get the real invalid_grant 400.
func TestSalesforceStyleJWTBearerGrant(t *testing.T) {
	base := sfStart(t)
	priv := sfPrivateKey(t)
	now := time.Now().Unix()

	postGrant := func(assertion string) (string, int) {
		v := url.Values{
			"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		}
		if assertion != "" {
			v.Set("assertion", assertion)
		}
		return sfPostForm(t, base+"/services/oauth2/token", v)
	}

	// ===== Valid assertion → 200, access_token, no refresh_token =====

	assertion := sfSignAssertion(t, priv,
		"3MVG9mockConsumerKey", "jwt-user@example.com",
		"https://login.salesforce.com", now, now+300)
	body, status := postGrant(assertion)
	if status != 200 {
		t.Fatalf("jwt-bearer grant -> %d, want 200; body %s", status, body)
	}
	var tok map[string]any
	if err := json.Unmarshal([]byte(body), &tok); err != nil {
		t.Fatalf("unmarshal token resp: %v (body %s)", err, body)
	}
	accessToken, _ := tok["access_token"].(string)
	if accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty", tok["access_token"])
	}
	if tok["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", tok["token_type"])
	}
	if _, has := tok["refresh_token"]; has {
		t.Fatalf("jwt-bearer response has refresh_token = %v, want absent", tok["refresh_token"])
	}

	// The minted token is a real session — it authorizes API calls.
	res := sfQuery(t, base, accessToken, "query", "SELECT Id FROM Account LIMIT 1", nil)
	if _, ok := res["totalSize"]; !ok {
		t.Fatalf("query with jwt-bearer token = %v, want a result envelope", res)
	}

	// ===== Forged assertion (wrong signing key) → 400 invalid_grant =====

	forgedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	forged := sfSignAssertion(t, forgedKey,
		"3MVG9mockConsumerKey", "jwt-user@example.com",
		"https://login.salesforce.com", now, now+300)
	body, status = postGrant(forged)
	if status != 400 {
		t.Fatalf("forged assertion -> %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "invalid_grant") {
		t.Fatalf("forged assertion -> body %q, want invalid_grant", body)
	}

	// ===== Properly signed but expired assertion → 400 invalid_grant =====

	expired := sfSignAssertion(t, priv,
		"3MVG9mockConsumerKey", "jwt-user@example.com",
		"https://login.salesforce.com", now-600, now-300)
	body, status = postGrant(expired)
	if status != 400 {
		t.Fatalf("expired assertion -> %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "invalid_grant") {
		t.Fatalf("expired assertion -> body %q, want invalid_grant", body)
	}

	// ===== Wrong audience → 400 invalid_grant =====

	wrongAud := sfSignAssertion(t, priv,
		"3MVG9mockConsumerKey", "jwt-user@example.com",
		"https://evil.example.com/token", now, now+300)
	body, status = postGrant(wrongAud)
	if status != 400 {
		t.Fatalf("wrong aud -> %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "invalid_grant") {
		t.Fatalf("wrong aud -> body %q, want invalid_grant", body)
	}

	// ===== Garbage assertion (not a JWT) → 400 invalid_grant =====

	body, status = postGrant("not-a-jwt-at-all")
	if status != 400 {
		t.Fatalf("garbage assertion -> %d, want 400; body %s", status, body)
	}

	// ===== Missing assertion → 400 invalid_request =====

	body, status = postGrant("")
	if status != 400 {
		t.Fatalf("missing assertion -> %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "invalid_request") {
		t.Fatalf("missing assertion -> body %q, want invalid_request", body)
	}
}
