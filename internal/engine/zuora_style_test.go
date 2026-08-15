package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestZuoraStyleAdapter exercises the Zuora Billing REST API adapter end-to-end:
//
//   - get account by accountKey
//   - list accounts
//   - create subscription (complex billing data model)
//   - get subscription
//   - record usage (metered billing)
//   - list usage
//   - ZOQL query (select Id from Account)
//   - apiAccessKeyId/apiSecretAccessKey legacy auth
//   - {success:false, reasons:[{code, message}]} error envelope
func TestZuoraStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "zuora-style")
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
			"zuora": {Adapter: absAdapterDir},
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

	base := addrs["zuora"]

	// Zuora auth: Bearer token (OAuth) or legacy apiAccessKeyId/apiSecretAccessKey.
	bearerToken := "Bearer zuora-bearer-token"

	// ===== get account by accountKey =====

	body, status := zuoraAuthGet(t, base+"/v1/accounts/ACC-A", bearerToken)
	if status != 200 {
		t.Fatalf("get account -> %d, want 200; body %s", status, body)
	}
	var acctResp map[string]any
	if err := json.Unmarshal([]byte(body), &acctResp); err != nil {
		t.Fatalf("unmarshal account: %v (body %s)", err, body)
	}
	if acctResp["accountId"] != "ACC-A" {
		t.Fatalf("accountId = %v, want ACC-A", acctResp["accountId"])
	}
	if _, ok := acctResp["accountNumber"].(string); !ok {
		t.Fatalf("accountNumber = %v, want string", acctResp["accountNumber"])
	}
	if _, ok := acctResp["name"].(string); !ok {
		t.Fatalf("name = %v, want string", acctResp["name"])
	}
	if _, ok := acctResp["currency"].(string); !ok {
		t.Fatalf("currency = %v, want string", acctResp["currency"])
	}
	if _, ok := acctResp["status"].(string); !ok {
		t.Fatalf("status = %v, want string", acctResp["status"])
	}
	if _, ok := acctResp["billTo"].(map[string]any); !ok {
		t.Fatalf("billTo = %v, want object", acctResp["billTo"])
	}

	// ===== list accounts =====

	body, status = zuoraAuthGet(t, base+"/v1/accounts", bearerToken)
	if status != 200 {
		t.Fatalf("list accounts -> %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal accounts: %v (body %s)", err, body)
	}
	if listResp["success"] != true {
		t.Fatalf("success = %v, want true", listResp["success"])
	}
	accounts, ok := listResp["accounts"].([]any)
	if !ok || len(accounts) == 0 {
		t.Fatalf("accounts = %v, want non-empty", listResp["accounts"])
	}

	// ===== create subscription (complex billing data model) =====

	body, status = zuoraAuthPostJSON(t, base+"/v1/subscriptions", bearerToken, map[string]any{
		"accountKey":            "ACC-A",
		"termType":              "TERMED",
		"contractEffectiveDate": "2024-02-01",
		"subscribeToRatePlans": []map[string]any{
			{
				"productRatePlanId":   "rateplan-standard",
				"productRatePlanName": "Standard Plan",
			},
		},
	})
	if status != 200 {
		t.Fatalf("create subscription -> %d, want 200; body %s", status, body)
	}
	var subResp map[string]any
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal subscription: %v (body %s)", err, body)
	}
	if subResp["success"] != true {
		t.Fatalf("subscription success = %v, want true", subResp["success"])
	}
	subID, ok := subResp["subscriptionId"].(string)
	if !ok || subID == "" {
		t.Fatalf("subscriptionId = %v, want non-empty string", subResp["subscriptionId"])
	}

	// ===== get subscription =====

	body, status = zuoraAuthGet(t, base+"/v1/subscriptions/"+subID, bearerToken)
	if status != 200 {
		t.Fatalf("get subscription -> %d, want 200; body %s", status, body)
	}
	var getSub map[string]any
	if err := json.Unmarshal([]byte(body), &getSub); err != nil {
		t.Fatalf("unmarshal get subscription: %v (body %s)", err, body)
	}
	if getSub["subscriptionId"] != subID {
		t.Fatalf("retrieved subscriptionId = %v, want %s", getSub["subscriptionId"], subID)
	}
	if _, ok := getSub["subscriptionPlans"].([]any); !ok {
		t.Fatalf("subscriptionPlans = %v, want array", getSub["subscriptionPlans"])
	}
	if _, ok := getSub["termType"].(string); !ok {
		t.Fatalf("termType = %v, want string", getSub["termType"])
	}

	// ===== record usage (metered billing) =====

	body, status = zuoraAuthPostJSON(t, base+"/v1/usage", bearerToken, map[string]any{
		"AccountId":     "ACC-A",
		"Quantity":      1500,
		"StartDateTime": "2024-02-01T00:00:00Z",
		"UOM":           "Each",
	})
	if status != 200 {
		t.Fatalf("record usage -> %d, want 200; body %s", status, body)
	}
	var usageResp map[string]any
	if err := json.Unmarshal([]byte(body), &usageResp); err != nil {
		t.Fatalf("unmarshal usage: %v (body %s)", err, body)
	}
	if usageResp["success"] != true {
		t.Fatalf("usage success = %v, want true", usageResp["success"])
	}

	// ===== list usage (verify it was recorded — STATEFUL) =====

	body, status = zuoraAuthGet(t, base+"/v1/usage?AccountId=ACC-A", bearerToken)
	if status != 200 {
		t.Fatalf("list usage -> %d, want 200; body %s", status, body)
	}
	var listUsage map[string]any
	if err := json.Unmarshal([]byte(body), &listUsage); err != nil {
		t.Fatalf("unmarshal list usage: %v (body %s)", err, body)
	}
	usage, ok := listUsage["usage"].([]any)
	if !ok || len(usage) == 0 {
		t.Fatalf("usage = %v, want non-empty", listUsage["usage"])
	}

	// ===== ZOQL query (select Id from Account) =====

	body, status = zuoraAuthPostJSON(t, base+"/v1/action/query", bearerToken, map[string]any{
		"queryString": "select Id, Name from Account",
	})
	if status != 200 {
		t.Fatalf("ZOQL query -> %d, want 200; body %s", status, body)
	}
	var queryResp map[string]any
	if err := json.Unmarshal([]byte(body), &queryResp); err != nil {
		t.Fatalf("unmarshal query: %v (body %s)", err, body)
	}
	if queryResp["success"] != true {
		t.Fatalf("query success = %v, want true", queryResp["success"])
	}
	if _, ok := queryResp["size"].(float64); !ok {
		t.Fatalf("size = %v, want number", queryResp["size"])
	}
	records, ok := queryResp["records"].([]any)
	if !ok || len(records) == 0 {
		t.Fatalf("records = %v, want non-empty", queryResp["records"])
	}
	rec0 := records[0].(map[string]any)
	if _, ok := rec0["Id"].(string); !ok {
		t.Fatalf("record Id = %v, want string", rec0["Id"])
	}

	// ===== ZOQL query with WHERE clause =====

	body, status = zuoraAuthPostJSON(t, base+"/v1/action/query", bearerToken, map[string]any{
		"queryString": "select Id from Account where Id = 'ACC-A'",
	})
	if status != 200 {
		t.Fatalf("ZOQL query (WHERE) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &queryResp); err != nil {
		t.Fatalf("unmarshal WHERE query: %v (body %s)", err, body)
	}
	records, ok = queryResp["records"].([]any)
	if !ok || len(records) != 1 {
		t.Fatalf("WHERE records = %v, want exactly 1", queryResp["records"])
	}

	// ===== bogus bearer token → 401 =====

	_, status = zuoraAuthGet(t, base+"/v1/accounts", "Bearer zuora-bogus-token")
	if status != 401 {
		t.Fatalf("bogus-token accounts -> %d, want 401", status)
	}

	// ===== bogus legacy secret → 401 =====

	_, status = zuoraLegacyGet(t, base+"/v1/accounts", "zuora-access-key", "zuora-wrong-secret")
	if status != 401 {
		t.Fatalf("bogus-legacy-secret accounts -> %d, want 401", status)
	}

	// ===== apiAccessKeyId legacy auth (via headers) =====

	body, status = zuoraLegacyGet(t, base+"/v1/accounts", "zuora-access-key", "zuora-secret-key")
	if status != 200 {
		t.Fatalf("legacy auth list accounts -> %d, want 200; body %s", status, body)
	}

	// ===== apiAccessKeyId in body (POST with body fields) =====

	body, status = zuoraLegacyPostJSON(t, base+"/v1/action/query", "zuora-access-key", "zuora-secret-key", map[string]any{
		"queryString":        "select Id from Account",
		"apiAccessKeyId":     "zuora-access-key",
		"apiSecretAccessKey": "zuora-secret-key",
	})
	if status != 200 {
		t.Fatalf("legacy body auth ZOQL -> %d, want 200; body %s", status, body)
	}

	// ===== 401 without auth → Zuora error envelope =====

	body, status = zuoraNoAuthGet(t, base+"/v1/accounts")
	if status != 401 {
		t.Fatalf("no-auth accounts -> %d, want 401; body %s", status, body)
	}
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal error resp: %v (body %s)", err, body)
	}
	if errResp["success"] != false {
		t.Fatalf("error success = %v, want false", errResp["success"])
	}
	reasons, ok := errResp["reasons"].([]any)
	if !ok || len(reasons) == 0 {
		t.Fatalf("reasons = %v, want non-empty array", errResp["reasons"])
	}
	reason0 := reasons[0].(map[string]any)
	if _, ok := reason0["code"].(string); !ok {
		t.Fatalf("reason code = %v, want string", reason0["code"])
	}
	if _, ok := reason0["message"].(string); !ok {
		t.Fatalf("reason message = %v, want string", reason0["message"])
	}
}

// TestZuoraStyleBillingSemantics exercises the billing-fidelity surface:
//
//   - subscription create generates the first invoice from the rate plan
//     charges (chargeOverrides pricing, 10% tax, Net-30 due date, Posted)
//   - unknown productRatePlanId -> 400
//   - subscription update: add/remove rate plans, term recompute, deep-merged
//     custom fields
//   - billing preview computed from the rate plans (not hardcoded)
//   - payments: partial application (Processed-Partially, invoice balance
//     decremented), over-application rejected, full application (Processed),
//     unapply restoring balances
//   - cancel: Immediate (Canceled + prorated credit memo) and EndOfTerm
//     (stays Active; derive-on-read flip to Canceled once the term ends)
func TestZuoraStyleBillingSemantics(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "zuora-style")
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
			"zuora": {Adapter: absAdapterDir},
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

	base := addrs["zuora"]
	bearerToken := "Bearer zuora-bearer-token"

	// ===== unknown product rate plan -> 400 =====

	_, status := zuoraAuthPostJSON(t, base+"/v1/subscriptions", bearerToken, map[string]any{
		"accountKey": "ACC-A",
		"subscribeToRatePlans": []map[string]any{
			{"productRatePlanId": "rateplan-bogus"},
		},
	})
	if status != 400 {
		t.Fatalf("create subscription with bogus plan -> %d, want 400", status)
	}

	// ===== create subscription: rate plan + chargeOverrides -> first invoice =====
	// price 50 * qty 2 = 100 charge, 10 tax -> invoice amount 110.

	body, status := zuoraAuthPostJSON(t, base+"/v1/subscriptions", bearerToken, map[string]any{
		"accountKey":            "ACC-A",
		"termType":              "TERMED",
		"initialTerm":           12,
		"contractEffectiveDate": "2024-02-01",
		"subscribeToRatePlans": []map[string]any{
			{
				"productRatePlanId":   "rateplan-standard",
				"productRatePlanName": "Standard Plan",
				"chargeOverrides": []map[string]any{
					{"pricing": []map[string]any{{"currency": "USD", "price": 50}}, "quantity": 2},
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("create subscription -> %d, want 200; body %s", status, body)
	}
	var subResp map[string]any
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal subscription: %v (body %s)", err, body)
	}
	subID, _ := subResp["subscriptionId"].(string)
	if subID == "" {
		t.Fatalf("subscriptionId = %v, want non-empty", subResp["subscriptionId"])
	}
	invoiceNumber, _ := subResp["invoiceNumber"].(string)
	if invoiceNumber == "" {
		t.Fatalf("invoiceNumber = %v, want non-empty (first invoice generated)", subResp["invoiceNumber"])
	}
	if got := subResp["invoiceAmount"].(float64); !zuoraAlmostEq(got, 110) {
		t.Fatalf("invoiceAmount = %v, want 110 (50x2 + 10%% tax)", subResp["invoiceAmount"])
	}

	// GET the generated invoice: Posted, balance == amount, Net-30 due date.

	body, status = zuoraAuthGet(t, base+"/v1/invoices/"+invoiceNumber, bearerToken)
	if status != 200 {
		t.Fatalf("get invoice -> %d, want 200; body %s", status, body)
	}
	var invResp map[string]any
	if err := json.Unmarshal([]byte(body), &invResp); err != nil {
		t.Fatalf("unmarshal invoice: %v (body %s)", err, body)
	}
	if invResp["status"] != "Posted" {
		t.Fatalf("invoice status = %v, want Posted", invResp["status"])
	}
	if got := invResp["balance"].(float64); !zuoraAlmostEq(got, 110) {
		t.Fatalf("invoice balance = %v, want 110", invResp["balance"])
	}
	if invResp["dueDate"] != "2024-03-02" {
		t.Fatalf("invoice dueDate = %v, want 2024-03-02 (invoice date + Net-30)", invResp["dueDate"])
	}
	items, ok := invResp["invoiceItems"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("invoiceItems = %v, want exactly 1", invResp["invoiceItems"])
	}
	item0 := items[0].(map[string]any)
	if got := item0["chargeAmount"].(float64); !zuoraAlmostEq(got, 100) {
		t.Fatalf("invoiceItem chargeAmount = %v, want 100", item0["chargeAmount"])
	}
	if got := item0["taxAmount"].(float64); !zuoraAlmostEq(got, 10) {
		t.Fatalf("invoiceItem taxAmount = %v, want 10", item0["taxAmount"])
	}

	// ===== subscription update: swap rate plans, recompute term, deep-merge =====

	body, status = zuoraAuthPutJSON(t, base+"/v1/subscriptions/"+subID, bearerToken, map[string]any{
		"removeRatePlans": []string{"rateplan-standard"},
		"addRatePlans": []map[string]any{
			{"productRatePlanId": "rateplan-growth"},
		},
		"initialTerm":  24,
		"customFields": map[string]any{"crm": map[string]any{"id": "acct-42"}},
	})
	if status != 200 {
		t.Fatalf("update subscription -> %d, want 200; body %s", status, body)
	}
	var updResp map[string]any
	if err := json.Unmarshal([]byte(body), &updResp); err != nil {
		t.Fatalf("unmarshal update: %v (body %s)", err, body)
	}
	if added, ok := updResp["addedRatePlans"].([]any); !ok || len(added) != 1 {
		t.Fatalf("addedRatePlans = %v, want exactly 1", updResp["addedRatePlans"])
	}

	body, status = zuoraAuthGet(t, base+"/v1/subscriptions/"+subID, bearerToken)
	if status != 200 {
		t.Fatalf("get updated subscription -> %d, want 200; body %s", status, body)
	}
	var getSub map[string]any
	if err := json.Unmarshal([]byte(body), &getSub); err != nil {
		t.Fatalf("unmarshal get subscription: %v (body %s)", err, body)
	}
	plans, ok := getSub["subscriptionPlans"].([]any)
	if !ok || len(plans) != 1 {
		t.Fatalf("subscriptionPlans = %v, want exactly 1 after swap", getSub["subscriptionPlans"])
	}
	plan0 := plans[0].(map[string]any)
	if plan0["productRatePlanId"] != "rateplan-growth" {
		t.Fatalf("plan productRatePlanId = %v, want rateplan-growth", plan0["productRatePlanId"])
	}
	if plan0["productRatePlanName"] != "Growth Plan" {
		t.Fatalf("plan productRatePlanName = %v, want Growth Plan (from catalog)", plan0["productRatePlanName"])
	}
	wantEnd := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 24*30).Format("2006-01-02")
	if getSub["endDate"] != wantEnd {
		t.Fatalf("endDate = %v, want %s (initialTerm 24 recomputed)", getSub["endDate"], wantEnd)
	}
	custom, ok := getSub["customFields"].(map[string]any)
	if !ok {
		t.Fatalf("customFields = %v, want object", getSub["customFields"])
	}
	crm, ok := custom["crm"].(map[string]any)
	if !ok || crm["id"] != "acct-42" {
		t.Fatalf("customFields.crm = %v, want deep-merged {id: acct-42}", custom["crm"])
	}

	// ===== billing preview computed from the rate plans =====
	// Seeded SUB-B (ACC-B) uses rateplan-enterprise: 499 + 10% tax = 548.9.

	body, status = zuoraAuthPostJSON(t, base+"/v1/transactions/billing/preview", bearerToken, map[string]any{
		"accountKey":      "ACC-B",
		"subscriptionKey": "SUB-B",
	})
	if status != 200 {
		t.Fatalf("billing preview -> %d, want 200; body %s", status, body)
	}
	var preview map[string]any
	if err := json.Unmarshal([]byte(body), &preview); err != nil {
		t.Fatalf("unmarshal preview: %v (body %s)", err, body)
	}
	if preview["currency"] != "EUR" {
		t.Fatalf("preview currency = %v, want EUR (account currency)", preview["currency"])
	}
	inv, ok := preview["invoice"].(map[string]any)
	if !ok {
		t.Fatalf("preview invoice = %v, want object", preview["invoice"])
	}
	if got := inv["amount"].(float64); !zuoraAlmostEq(got, 548.9) {
		t.Fatalf("preview amount = %v, want 548.9 (499 + 10%% tax)", inv["amount"])
	}
	pItems, ok := inv["invoiceItems"].([]any)
	if !ok || len(pItems) != 1 {
		t.Fatalf("preview invoiceItems = %v, want exactly 1", inv["invoiceItems"])
	}
	if got := pItems[0].(map[string]any)["chargeAmount"].(float64); !zuoraAlmostEq(got, 499) {
		t.Fatalf("preview chargeAmount = %v, want 499 (catalog price)", pItems[0])
	}

	// ===== partial payment: Processed-Partially, invoice balance decremented =====
	// Seeded INV-B (ACC-B): amount 890.50, balance 890.50.

	body, status = zuoraAuthPostJSON(t, base+"/v1/payments", bearerToken, map[string]any{
		"accountKey": "ACC-B",
		"amount":     400,
		"type":       "External",
		"appliedTo": []map[string]any{
			{"invoiceId": "INV-B", "appliedAmount": 250},
		},
	})
	if status != 200 {
		t.Fatalf("create partial payment -> %d, want 200; body %s", status, body)
	}
	var payResp map[string]any
	if err := json.Unmarshal([]byte(body), &payResp); err != nil {
		t.Fatalf("unmarshal payment: %v (body %s)", err, body)
	}
	if payResp["status"] != "Processed-Partially" {
		t.Fatalf("payment status = %v, want Processed-Partially", payResp["status"])
	}
	if got := payResp["appliedAmount"].(float64); !zuoraAlmostEq(got, 250) {
		t.Fatalf("payment appliedAmount = %v, want 250", payResp["appliedAmount"])
	}
	if got := payResp["unappliedAmount"].(float64); !zuoraAlmostEq(got, 150) {
		t.Fatalf("payment unappliedAmount = %v, want 150", payResp["unappliedAmount"])
	}
	paymentID, _ := payResp["paymentId"].(string)
	if paymentID == "" {
		t.Fatalf("paymentId = %v, want non-empty", payResp["paymentId"])
	}

	body, status = zuoraAuthGet(t, base+"/v1/invoices/INV-B", bearerToken)
	if status != 200 {
		t.Fatalf("get invoice after partial payment -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &invResp); err != nil {
		t.Fatalf("unmarshal invoice after payment: %v (body %s)", err, body)
	}
	if got := invResp["balance"].(float64); !zuoraAlmostEq(got, 640.5) {
		t.Fatalf("invoice balance after partial payment = %v, want 640.50", invResp["balance"])
	}
	appliedPayments, ok := invResp["appliedPayments"].([]any)
	if !ok || len(appliedPayments) != 1 {
		t.Fatalf("appliedPayments = %v, want exactly 1", invResp["appliedPayments"])
	}

	// ===== over-application -> 400 (failure path) =====

	body, status = zuoraAuthPostJSON(t, base+"/v1/payments", bearerToken, map[string]any{
		"accountKey": "ACC-B",
		"amount":     10000,
		"appliedTo": []map[string]any{
			{"invoiceId": "INV-B", "appliedAmount": 10000},
		},
	})
	if status != 400 {
		t.Fatalf("over-applied payment -> %d, want 400; body %s", status, body)
	}

	// ===== full payment: Processed, invoice balance 0 =====

	body, status = zuoraAuthPostJSON(t, base+"/v1/payments", bearerToken, map[string]any{
		"accountKey": "ACC-B",
		"amount":     640.5,
		"appliedTo": []map[string]any{
			{"invoiceId": "INV-B", "appliedAmount": 640.5},
		},
	})
	if status != 200 {
		t.Fatalf("create full payment -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &payResp); err != nil {
		t.Fatalf("unmarshal full payment: %v (body %s)", err, body)
	}
	if payResp["status"] != "Processed" {
		t.Fatalf("full payment status = %v, want Processed", payResp["status"])
	}

	body, status = zuoraAuthGet(t, base+"/v1/invoices/INV-B", bearerToken)
	if status != 200 {
		t.Fatalf("get invoice after full payment -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &invResp); err != nil {
		t.Fatalf("unmarshal invoice after full payment: %v (body %s)", err, body)
	}
	if got := invResp["balance"].(float64); !zuoraAlmostEq(got, 0) {
		t.Fatalf("invoice balance after full payment = %v, want 0", invResp["balance"])
	}

	// ===== unapply: restores invoice + payment applied amount =====

	body, status = zuoraAuthPostJSON(t, base+"/v1/payments/"+paymentID+"/unapply", bearerToken, map[string]any{
		"amount": 100,
	})
	if status != 200 {
		t.Fatalf("unapply payment -> %d, want 200; body %s", status, body)
	}
	var unapplyResp map[string]any
	if err := json.Unmarshal([]byte(body), &unapplyResp); err != nil {
		t.Fatalf("unmarshal unapply: %v (body %s)", err, body)
	}
	if got := unapplyResp["appliedAmount"].(float64); !zuoraAlmostEq(got, 150) {
		t.Fatalf("unapplied payment appliedAmount = %v, want 150", unapplyResp["appliedAmount"])
	}

	body, status = zuoraAuthGet(t, base+"/v1/invoices/INV-B", bearerToken)
	if status != 200 {
		t.Fatalf("get invoice after unapply -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &invResp); err != nil {
		t.Fatalf("unmarshal invoice after unapply: %v (body %s)", err, body)
	}
	if got := invResp["balance"].(float64); !zuoraAlmostEq(got, 100) {
		t.Fatalf("invoice balance after unapply = %v, want 100", invResp["balance"])
	}

	// ===== cancel Immediate: Canceled now + prorated credit memo =====
	// Fresh TERMED subscription effective today: the credit memo covers the
	// full charge total (99 + 9.9 tax = 108.9 invoice, credit 99 pre-tax).

	body, status = zuoraAuthPostJSON(t, base+"/v1/subscriptions", bearerToken, map[string]any{
		"accountKey":            "ACC-A",
		"termType":              "TERMED",
		"initialTerm":           12,
		"contractEffectiveDate": "2024-01-01",
		"subscribeToRatePlans": []map[string]any{
			{"productRatePlanId": "rateplan-standard"},
		},
	})
	if status != 200 {
		t.Fatalf("create subscription for cancel -> %d, want 200; body %s", status, body)
	}
	var sub2 map[string]any
	if err := json.Unmarshal([]byte(body), &sub2); err != nil {
		t.Fatalf("unmarshal subscription 2: %v (body %s)", err, body)
	}
	sub2ID, _ := sub2["subscriptionId"].(string)

	body, status = zuoraAuthPostJSON(t, base+"/v1/subscriptions/"+sub2ID+"/cancel", bearerToken, map[string]any{
		"cancellationPolicy": "Immediate",
	})
	if status != 200 {
		t.Fatalf("cancel immediate -> %d, want 200; body %s", status, body)
	}
	var cancelResp map[string]any
	if err := json.Unmarshal([]byte(body), &cancelResp); err != nil {
		t.Fatalf("unmarshal cancel: %v (body %s)", err, body)
	}
	if cancelResp["status"] != "Canceled" {
		t.Fatalf("immediate cancel status = %v, want Canceled", cancelResp["status"])
	}
	creditNumber, _ := cancelResp["creditMemoNumber"].(string)
	if creditNumber == "" {
		t.Fatalf("creditMemoNumber = %v, want non-empty (prorated credit issued)", cancelResp["creditMemoNumber"])
	}
	if got := cancelResp["creditMemoAmount"].(float64); !zuoraAlmostEq(got, 99) {
		t.Fatalf("creditMemoAmount = %v, want 99 (full-term credit)", cancelResp["creditMemoAmount"])
	}

	body, status = zuoraAuthGet(t, base+"/v1/invoices/"+creditNumber, bearerToken)
	if status != 200 {
		t.Fatalf("get credit memo -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &invResp); err != nil {
		t.Fatalf("unmarshal credit memo: %v (body %s)", err, body)
	}
	if got := invResp["amount"].(float64); !zuoraAlmostEq(got, -99) {
		t.Fatalf("credit memo amount = %v, want -99", invResp["amount"])
	}

	// ===== cancel EndOfTerm: stays Active, flips to Canceled on read =====
	// TERMED subscription whose term ends today: the pending cancellation is
	// derived on the next GET.

	body, status = zuoraAuthPostJSON(t, base+"/v1/subscriptions", bearerToken, map[string]any{
		"accountKey":            "ACC-A",
		"termType":              "TERMED",
		"contractEffectiveDate": "2024-01-01",
		"termEndDate":           "2024-01-01",
		"subscribeToRatePlans": []map[string]any{
			{"productRatePlanId": "rateplan-standard"},
		},
	})
	if status != 200 {
		t.Fatalf("create subscription for eot cancel -> %d, want 200; body %s", status, body)
	}
	var sub3 map[string]any
	if err := json.Unmarshal([]byte(body), &sub3); err != nil {
		t.Fatalf("unmarshal subscription 3: %v (body %s)", err, body)
	}
	sub3ID, _ := sub3["subscriptionId"].(string)

	body, status = zuoraAuthPostJSON(t, base+"/v1/subscriptions/"+sub3ID+"/cancel", bearerToken, map[string]any{
		"cancellationPolicy": "EndOfTerm",
	})
	if status != 200 {
		t.Fatalf("cancel end-of-term -> %d, want 200; body %s", status, body)
	}
	var eotResp map[string]any
	if err := json.Unmarshal([]byte(body), &eotResp); err != nil {
		t.Fatalf("unmarshal eot cancel: %v (body %s)", err, body)
	}
	if eotResp["status"] != "Active" {
		t.Fatalf("end-of-term cancel status = %v, want Active until term end", eotResp["status"])
	}
	if eotResp["cancellationEffectiveDate"] != "2024-01-01" {
		t.Fatalf("cancellationEffectiveDate = %v, want 2024-01-01 (term end)", eotResp["cancellationEffectiveDate"])
	}

	body, status = zuoraAuthGet(t, base+"/v1/subscriptions/"+sub3ID, bearerToken)
	if status != 200 {
		t.Fatalf("get eot-cancelled subscription -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &getSub); err != nil {
		t.Fatalf("unmarshal eot get: %v (body %s)", err, body)
	}
	if getSub["status"] != "Canceled" {
		t.Fatalf("eot subscription status on read = %v, want Canceled (term ended on the synthetic calendar)", getSub["status"])
	}
}

// zuoraAlmostEq compares money amounts at cent precision.
func zuoraAlmostEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 0.005
}

// zuoraAuthPutJSON sends an authenticated JSON PUT.
func zuoraAuthPutJSON(t *testing.T, rawurl, authHeader string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("PUT", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// === Zuora test helpers ===

func zuoraAuthGet(t *testing.T, rawurl, authHeader string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", authHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func zuoraNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func zuoraLegacyGet(t *testing.T, rawurl, apiKey, apiSecret string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("apiAccessKeyId", apiKey)
	req.Header.Set("apiSecretAccessKey", apiSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func zuoraAuthPostJSON(t *testing.T, rawurl, authHeader string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func zuoraLegacyPostJSON(t *testing.T, rawurl, apiKey, apiSecret string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Zuora legacy auth via headers.
	req.Header.Set("apiAccessKeyId", apiKey)
	req.Header.Set("apiSecretAccessKey", apiSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// Guard: suppress unused imports.
var _ = fmt.Sprintf
var _ = strings.Contains

// TestZuoraStyleClockDefaultDates verifies that a subscription created
// without a contractEffectiveDate defaults to the clock-anchored synthetic
// "today", and the generated invoice's dueDate is invoiceDate + Net-30.
func TestZuoraStyleClockDefaultDates(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "zuora-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"zuora": {Adapter: adapterDir},
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
	base := addrs["zuora"]
	bearerToken := "Bearer zuora-bearer-token"

	// No contractEffectiveDate: the simulator derives it from the clock.
	body, status := zuoraAuthPostJSON(t, base+"/v1/subscriptions", bearerToken, map[string]any{
		"accountKey":  "ACC-A",
		"termType":    "TERMED",
		"initialTerm": 12,
		"subscribeToRatePlans": []map[string]any{
			{"productRatePlanId": "rateplan-standard"},
		},
	})
	if status != 200 {
		t.Fatalf("create subscription (default CED) -> %d, want 200; body %s", status, body)
	}
	var subResp map[string]any
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal subscription: %v (body %s)", err, body)
	}
	invoiceNumber, _ := subResp["invoiceNumber"].(string)
	if invoiceNumber == "" {
		t.Fatalf("invoiceNumber = %v, want non-empty (first invoice generated)", subResp["invoiceNumber"])
	}

	body, status = zuoraAuthGet(t, base+"/v1/invoices/"+invoiceNumber, bearerToken)
	if status != 200 {
		t.Fatalf("get invoice -> %d, want 200; body %s", status, body)
	}
	var invResp map[string]any
	if err := json.Unmarshal([]byte(body), &invResp); err != nil {
		t.Fatalf("unmarshal invoice: %v (body %s)", err, body)
	}
	invoiceDate, _ := invResp["invoiceDate"].(string)
	if invoiceDate == "" {
		t.Fatalf("invoiceDate = %v, want non-empty", invResp["invoiceDate"])
	}
	// The synthetic calendar is anchored at first use; the default CED must
	// land on that anchor day (clock-derived, not a fixed 2024-01-01 string
	// buried in the handler — the anchor advances with real elapsed time).
	if len(invoiceDate) != len("2006-01-02") {
		t.Fatalf("invoiceDate = %q, want YYYY-MM-DD", invoiceDate)
	}
	invDate, err := time.Parse("2006-01-02", invoiceDate)
	if err != nil {
		t.Fatalf("invoiceDate %q unparseable: %v", invoiceDate, err)
	}
	wantDue := invDate.AddDate(0, 0, 30).Format("2006-01-02")
	if invResp["dueDate"] != wantDue {
		t.Fatalf("dueDate = %v, want %s (invoiceDate + Net-30)", invResp["dueDate"], wantDue)
	}
}
