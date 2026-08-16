package engine

import (
	"bytes"
	"context"
	"encoding/json"
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
