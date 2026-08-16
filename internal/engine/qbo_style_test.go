package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestQBOStyleAdapter exercises the QBO-style adapter end-to-end:
//
//   - authorize → 302 redirect with code+state+realmId
//   - token exchange (auth code) → access_token + refresh_token
//   - query Customer → seeded customers
//   - create invoice → invoice with computed TotalAmt
//   - refresh → NEW refresh_token (the churn); old refresh_token invalidated
//   - query Invoice → invoices (seeded + created)
//   - 401 without bearer → Fault envelope with code 32001
func TestQBOStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "qbo-style")
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
			"qbo": {Adapter: absAdapterDir},
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

	base := addrs["qbo"]

	const clientID = "test-client-id"
	const clientSecret = "test-client-secret"
	const redirectURI = "http://localhost:3000/callback"
	const state = "random-state-qbo"

	// ===== authorize → 302 redirect =====

	resp := qboGetNoRedirect(t, base+"/oauth/v2/authorize?"+
		"client_id="+clientID+
		"&redirect_uri="+url.QueryEscape(redirectURI)+
		"&state="+state+
		"&response_type=code&scope=com.intuit.quickbooks.accounting")
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> %d, want 302", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("authorize: missing Location header")
	}
	authCode := qboExtractParam(location, "code")
	if authCode == "" {
		t.Fatalf("authorize: no code in Location %q", location)
	}
	if qboExtractParam(location, "state") != state {
		t.Fatalf("authorize: state mismatch in %q", location)
	}
	realmID := qboExtractParam(location, "realmId")
	if realmID == "" {
		t.Fatalf("authorize: no realmId in %q", location)
	}

	// ===== token exchange (authorization_code) =====

	body, status := qboPostForm(t, base+"/oauth/v2/tokens/bearer", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if status != 200 {
		t.Fatalf("token (auth code) -> %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token resp: %v (body %s)", err, body)
	}
	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty", tokenResp["access_token"])
	}
	refreshToken1, ok := tokenResp["refresh_token"].(string)
	if !ok || refreshToken1 == "" {
		t.Fatalf("refresh_token = %v, want non-empty", tokenResp["refresh_token"])
	}
	if tokenResp["token_type"] != "bearer" {
		t.Fatalf("token_type = %v, want bearer", tokenResp["token_type"])
	}
	if tokenResp["expires_in"] != float64(3600) {
		t.Fatalf("expires_in = %v, want 3600", tokenResp["expires_in"])
	}
	if _, ok := tokenResp["x_refresh_token_expires_in"].(float64); !ok {
		t.Fatalf("x_refresh_token_expires_in = %v, want number", tokenResp["x_refresh_token_expires_in"])
	}

	// ===== query Customer (GET) =====

	body, status = qboAuthGet(t, base+"/v3/company/"+realmID+"/query?query="+
		url.QueryEscape("select * from Customer"), accessToken)
	if status != 200 {
		t.Fatalf("query Customer -> %d, want 200; body %s", status, body)
	}
	var custQueryResp map[string]any
	if err := json.Unmarshal([]byte(body), &custQueryResp); err != nil {
		t.Fatalf("unmarshal query resp: %v (body %s)", err, body)
	}
	qr, ok := custQueryResp["QueryResponse"].(map[string]any)
	if !ok {
		t.Fatalf("QueryResponse = %v, want object", custQueryResp["QueryResponse"])
	}
	customers, ok := qr["Customer"].([]any)
	if !ok {
		t.Fatalf("Customer = %v, want array", qr["Customer"])
	}
	if len(customers) < 1 {
		t.Fatalf("customers count = %d, want >= 1", len(customers))
	}
	cust0 := customers[0].(map[string]any)
	if _, ok := cust0["Id"].(string); !ok {
		t.Fatalf("customer Id = %v, want string", cust0["Id"])
	}
	if _, ok := cust0["DisplayName"].(string); !ok {
		t.Fatalf("customer DisplayName = %v, want string", cust0["DisplayName"])
	}

	// ===== create invoice =====

	custID, _ := cust0["Id"].(string)
	body, status = qboAuthPostJSON(t, base+"/v3/company/"+realmID+"/invoice", accessToken, map[string]any{
		"Line": []map[string]any{
			{
				"Id":          "1",
				"LineNum":     1,
				"Description": "Consulting services",
				"Amount":      2500.00,
				"DetailType":  "SalesItemLineDetail",
				"SalesItemLineDetail": map[string]any{
					"ItemRef":   map[string]any{"value": "1", "name": "Services"},
					"UnitPrice": 2500,
					"Qty":       1,
				},
			},
		},
		"CustomerRef": map[string]any{"value": custID, "name": cust0["DisplayName"]},
		"TxnDate":     "2024-02-01",
	})
	if status != 200 {
		t.Fatalf("create invoice -> %d, want 200; body %s", status, body)
	}
	var invResp map[string]any
	if err := json.Unmarshal([]byte(body), &invResp); err != nil {
		t.Fatalf("unmarshal invoice resp: %v (body %s)", err, body)
	}
	invObj, ok := invResp["Invoice"].(map[string]any)
	if !ok {
		t.Fatalf("Invoice = %v, want object", invResp["Invoice"])
	}
	if invObj["TotalAmt"] != float64(2500) {
		t.Fatalf("TotalAmt = %v, want 2500", invObj["TotalAmt"])
	}
	newInvID, _ := invObj["Id"].(string)
	if newInvID == "" {
		t.Fatal("new invoice Id is empty")
	}

	// ===== query Invoice → shows created invoice (STATEFUL) =====

	body, status = qboAuthGet(t, base+"/v3/company/"+realmID+"/query?query="+
		url.QueryEscape("select * from Invoice"), accessToken)
	if status != 200 {
		t.Fatalf("query Invoice -> %d, want 200; body %s", status, body)
	}
	var invQueryResp map[string]any
	if err := json.Unmarshal([]byte(body), &invQueryResp); err != nil {
		t.Fatalf("unmarshal query Invoice resp: %v (body %s)", err, body)
	}
	invQR, ok := invQueryResp["QueryResponse"].(map[string]any)
	if !ok {
		t.Fatalf("QueryResponse = %v, want object", invQueryResp["QueryResponse"])
	}
	invoices, ok := invQR["Invoice"].([]any)
	if !ok {
		t.Fatalf("Invoice = %v, want array", invQR["Invoice"])
	}
	if len(invoices) < 2 {
		t.Fatalf("invoices count = %d, want >= 2 (1 seed + 1 created)", len(invoices))
	}

	// ===== get invoice by ID =====

	body, status = qboAuthGet(t, base+"/v3/company/"+realmID+"/invoice/"+newInvID, accessToken)
	if status != 200 {
		t.Fatalf("get invoice -> %d, want 200; body %s", status, body)
	}

	// ===== refresh → NEW refresh_token (the churn) =====

	body, status = qboPostForm(t, base+"/oauth/v2/tokens/bearer", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken1},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
	if status != 200 {
		t.Fatalf("refresh -> %d, want 200; body %s", status, body)
	}
	var refreshResp map[string]any
	if err := json.Unmarshal([]byte(body), &refreshResp); err != nil {
		t.Fatalf("unmarshal refresh resp: %v (body %s)", err, body)
	}
	newAccess, ok := refreshResp["access_token"].(string)
	if !ok || newAccess == "" {
		t.Fatalf("refreshed access_token = %v, want non-empty", refreshResp["access_token"])
	}
	if newAccess == accessToken {
		t.Fatal("refresh: access_token did not change")
	}
	newRefresh, ok := refreshResp["refresh_token"].(string)
	if !ok || newRefresh == "" {
		t.Fatalf("refreshed refresh_token = %v, want non-empty", refreshResp["refresh_token"])
	}
	if newRefresh == refreshToken1 {
		t.Fatal("refresh: refresh_token did not change (QBO churn: should get a NEW refresh_token)")
	}

	// ===== OLD refresh_token is invalidated (the pain) =====

	_, status = qboPostForm(t, base+"/oauth/v2/tokens/bearer", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken1},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
	if status != 400 {
		t.Fatalf("old refresh_token -> %d, want 400 (invalidated)", status)
	}

	// ===== new access_token works =====

	body, status = qboAuthGet(t, base+"/v3/company/"+realmID+"/query?query="+
		url.QueryEscape("select * from Customer"), newAccess)
	if status != 200 {
		t.Fatalf("query with refreshed token -> %d, want 200; body %s", status, body)
	}

	// ===== 401 without bearer → Fault envelope =====

	body, status = qboNoAuthGet(t, base+"/v3/company/"+realmID+"/query?query="+
		url.QueryEscape("select * from Customer"))
	if status != 401 {
		t.Fatalf("no-auth query -> %d, want 401; body %s", status, body)
	}
	var faultResp map[string]any
	if err := json.Unmarshal([]byte(body), &faultResp); err != nil {
		t.Fatalf("unmarshal fault resp: %v (body %s)", err, body)
	}
	fault, ok := faultResp["Fault"].(map[string]any)
	if !ok {
		t.Fatalf("Fault = %v, want object", faultResp["Fault"])
	}
	errs, ok := fault["Error"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("Error array = %v, want non-empty", fault["Error"])
	}
	err0 := errs[0].(map[string]any)
	if err0["code"] != "32001" {
		t.Fatalf("Error code = %v, want 32001", err0["code"])
	}

	// ===== 401 with an unknown (never-minted) bearer → same Fault envelope =====

	body, status = qboAuthGet(t, base+"/v3/company/"+realmID+"/query?query="+
		url.QueryEscape("select * from Customer"), "Q0_bogus_never_minted")
	if status != 401 {
		t.Fatalf("bogus-token query -> %d, want 401; body %s", status, body)
	}
}

// === QBO test helpers ===

func qboGetNoRedirect(t *testing.T, rawurl string) *http.Response {
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

func qboAuthGet(t *testing.T, rawurl, token string) (string, int) {
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

func qboNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func qboAuthPostJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
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

func qboPostForm(t *testing.T, rawurl string, form url.Values) (string, int) {
	t.Helper()
	resp, err := http.PostForm(rawurl, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func qboExtractParam(rawurl, param string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return u.Query().Get(param)
}

// TestQBOStyleCustomerDeactivation proves QBO's customer soft delete: DELETE
// deactivates (Active=false) instead of destroying — the record stays
// readable by id, disappears from the bare list and the default query, and
// an explicit `WHERE Active = False` surfaces it. Also covers the real-QBO
// update path (POST with Id) and the invoice void operation.
func TestQBOStyleCustomerDeactivation(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "qbo-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"qbo": {Adapter: absAdapterDir},
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
	base := addrs["qbo"]

	// authorize + token (reuse the main flow's helpers).
	resp := qboGetNoRedirect(t, base+"/oauth/v2/authorize?client_id=cid&redirect_uri="+
		url.QueryEscape("http://localhost:3000/cb")+"&state=s&response_type=code&scope=com.intuit.quickbooks.accounting")
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	authCode := qboExtractParam(location, "code")
	realmID := qboExtractParam(location, "realmId")
	body, status := qboPostForm(t, base+"/oauth/v2/tokens/bearer", url.Values{
		"grant_type": {"authorization_code"}, "code": {authCode},
		"client_id": {"cid"}, "client_secret": {"csecret"},
	})
	if status != 200 {
		t.Fatalf("token -> %d; body %s", status, body)
	}
	var tokResp map[string]any
	json.Unmarshal([]byte(body), &tokResp)
	token, _ := tokResp["access_token"].(string)

	// ===== create two customers =====
	var ids []string
	for _, name := range []string{"Deactivate Me", "Keep Me"} {
		body, status = qboAuthPostJSON(t, base+"/v3/company/"+realmID+"/customer", token, map[string]any{
			"DisplayName": name,
		})
		if status != 200 {
			t.Fatalf("create %s -> %d; body %s", name, status, body)
		}
		var cr map[string]any
		json.Unmarshal([]byte(body), &cr)
		cust, _ := cr["Customer"].(map[string]any)
		if cust["Active"] != true {
			t.Fatalf("created customer Active = %v, want true", cust["Active"])
		}
		ids = append(ids, cust["Id"].(string))
	}
	gone, kept := ids[0], ids[1]

	// ===== DELETE deactivates (soft delete), keeps the record =====
	body, status = qboAuthDelete(t, base+"/v3/company/"+realmID+"/customer/"+gone, token)
	if status != 200 {
		t.Fatalf("DELETE customer -> %d, want 200; body %s", status, body)
	}
	var delResp map[string]any
	json.Unmarshal([]byte(body), &delResp)
	delCust, _ := delResp["Customer"].(map[string]any)
	if delCust["status"] != "Deleted" || delCust["Active"] != false {
		t.Fatalf("DELETE response Customer = %v, want status Deleted + Active false", delCust)
	}

	// Read by id STILL returns the deactivated customer.
	body, status = qboAuthGet(t, base+"/v3/company/"+realmID+"/customer/"+gone, token)
	if status != 200 {
		t.Fatalf("GET deactivated customer -> %d, want 200; body %s", status, body)
	}
	var got map[string]any
	json.Unmarshal([]byte(body), &got)
	if got["Customer"].(map[string]any)["Active"] != false {
		t.Fatalf("deactivated customer Active = %v, want false", got["Customer"].(map[string]any)["Active"])
	}

	// Bare list excludes the inactive customer.
	listActive := func() []string {
		t.Helper()
		b, st := qboAuthGet(t, base+"/v3/company/"+realmID+"/customer", token)
		if st != 200 {
			t.Fatalf("GET customer list -> %d; body %s", st, b)
		}
		var lr map[string]any
		json.Unmarshal([]byte(b), &lr)
		qr, _ := lr["QueryResponse"].(map[string]any)
		rows, _ := qr["Customer"].([]any)
		out := []string{}
		for _, r := range rows {
			out = append(out, r.(map[string]any)["Id"].(string))
		}
		return out
	}
	for _, id := range listActive() {
		if id == gone {
			t.Fatalf("deactivated customer %s leaked into default list", gone)
		}
	}

	// Default query also excludes it...
	query := func(q string) []string {
		t.Helper()
		b, st := qboAuthGet(t, base+"/v3/company/"+realmID+"/query?query="+url.QueryEscape(q), token)
		if st != 200 {
			t.Fatalf("query %q -> %d; body %s", q, st, b)
		}
		var qr2 map[string]any
		json.Unmarshal([]byte(b), &qr2)
		resp2, _ := qr2["QueryResponse"].(map[string]any)
		rows, _ := resp2["Customer"].([]any)
		out := []string{}
		for _, r := range rows {
			out = append(out, r.(map[string]any)["Id"].(string))
		}
		return out
	}
	for _, id := range query("select * from Customer") {
		if id == gone {
			t.Fatalf("deactivated customer %s leaked into default query", gone)
		}
	}
	if len(query("select * from Customer")) == 0 {
		t.Fatal("default query excluded everything; the active customer must remain")
	}

	// ...but WHERE Active = False surfaces exactly the deactivated one.
	inactive := query("select * from Customer where Active = False")
	if len(inactive) != 1 || inactive[0] != gone {
		t.Fatalf("WHERE Active = False -> %v, want exactly [%s]", inactive, gone)
	}
	// WHERE Active = True keeps the live ones (seed + created), never the
	// deactivated row.
	active := query("select * from Customer where Active = True")
	activeHasKept, activeHasGone := false, false
	for _, id := range active {
		if id == kept {
			activeHasKept = true
		}
		if id == gone {
			activeHasGone = true
		}
	}
	if !activeHasKept || activeHasGone {
		t.Fatalf("WHERE Active = True -> %v, want %s present and %s absent", active, kept, gone)
	}

	// ===== the real-QBO way: sparse POST with Id deactivates too =====
	body, status = qboAuthPostJSON(t, base+"/v3/company/"+realmID+"/customer", token, map[string]any{
		"Id": kept, "sparse": true, "Active": false,
	})
	if status != 200 {
		t.Fatalf("sparse update -> %d; body %s", status, body)
	}
	var ur map[string]any
	json.Unmarshal([]byte(body), &ur)
	if ur["Customer"].(map[string]any)["Active"] != false {
		t.Fatalf("sparse update Active = %v, want false", ur["Customer"])
	}
	remaining := listActive()
	for _, id := range remaining {
		if id == gone || id == kept {
			t.Fatalf("list after sparse deactivation still shows %s (both created customers are inactive)", id)
		}
	}

	// POST with an unknown Id -> 404 Fault 620.
	body, status = qboAuthPostJSON(t, base+"/v3/company/"+realmID+"/customer", token, map[string]any{
		"Id": "999999", "sparse": true, "Active": false,
	})
	if status != 404 {
		t.Fatalf("update unknown id -> %d, want 404; body %s", status, body)
	}
	if code := qboFaultCode(t, body); code != "620" {
		t.Fatalf("update unknown id code = %v, want 620", code)
	}

	// ===== invoice void (?operation=void) =====
	body, status = qboAuthPostJSON(t, base+"/v3/company/"+realmID+"/invoice", token, map[string]any{
		"Line":        []map[string]any{{"Amount": 900.0, "DetailType": "SalesItemLineDetail"}},
		"CustomerRef": map[string]any{"value": gone, "name": "Deactivate Me"},
	})
	if status != 200 {
		t.Fatalf("create invoice -> %d; body %s", status, body)
	}
	var ir map[string]any
	json.Unmarshal([]byte(body), &ir)
	invID := ir["Invoice"].(map[string]any)["Id"].(string)

	body, status = qboAuthPostJSON(t, base+"/v3/company/"+realmID+"/invoice?operation=void", token, map[string]any{
		"Id": invID, "SyncToken": "0",
	})
	if status != 200 {
		t.Fatalf("void invoice -> %d, want 200; body %s", status, body)
	}
	var vr map[string]any
	json.Unmarshal([]byte(body), &vr)
	voided := vr["Invoice"].(map[string]any)
	if voided["status"] != "Voided" {
		t.Fatalf("voided invoice status = %v, want Voided", voided["status"])
	}
	if voided["Balance"] != float64(0) {
		t.Fatalf("voided invoice Balance = %v, want 0", voided["Balance"])
	}

	// The voided invoice is still readable by id (soft delete).
	body, status = qboAuthGet(t, base+"/v3/company/"+realmID+"/invoice/"+invID, token)
	if status != 200 {
		t.Fatalf("GET voided invoice -> %d, want 200; body %s", status, body)
	}

	// Void failure paths: unknown id -> 404 620; missing Id -> 400 610.
	body, status = qboAuthPostJSON(t, base+"/v3/company/"+realmID+"/invoice?operation=void", token, map[string]any{
		"Id": "999999",
	})
	if status != 404 || qboFaultCode(t, body) != "620" {
		t.Fatalf("void unknown invoice -> %d code %v, want 404 620; body %s", status, qboFaultCode(t, body), body)
	}
	body, status = qboAuthPostJSON(t, base+"/v3/company/"+realmID+"/invoice?operation=void", token, map[string]any{})
	if status != 400 || qboFaultCode(t, body) != "610" {
		t.Fatalf("void missing Id -> %d code %v, want 400 610; body %s", status, qboFaultCode(t, body), body)
	}
}

// qboAuthDelete performs an authorized DELETE and returns body + status.
func qboAuthDelete(t *testing.T, rawurl, token string) (string, int) {
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

// qboFaultCode digs the first Fault.Error[].code out of a QBO error body.
func qboFaultCode(t *testing.T, body string) string {
	t.Helper()
	var f map[string]any
	if err := json.Unmarshal([]byte(body), &f); err != nil {
		t.Fatalf("unmarshal fault %q: %v", body, err)
	}
	fault, _ := f["Fault"].(map[string]any)
	errs, _ := fault["Error"].([]any)
	if len(errs) == 0 {
		return ""
	}
	code, _ := errs[0].(map[string]any)["code"].(string)
	return code
}
