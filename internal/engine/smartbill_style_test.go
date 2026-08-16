package engine

import (
	"bytes"
	"context"
	"encoding/base64"
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

// sbillAuth is a valid Basic header for the adapter (any user:token pair).
func sbillAuth() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("sbuser:sbtoken"))
}

// TestSmartBillStyleAdapter exercises the real SmartBill surface (version-free
// paths, client/supplier objects, numeric amounts, errorText errors):
//
//   - 401 without Basic credentials (and for malformed base64 / non-Basic)
//   - company bootstrap via /sim/company; unknown cif is a 404
//   - invoice create (empty body) + GET by seriesname/number with the real
//     client object, numeric products, and computed totals
//   - paymentstatus: unpaid -> partial -> paid as payments land; payment
//     delete by paymentId un-pays
//   - invoice cancel/restore; estimate create/get/cancel; purchase create/get
//   - stocks GET grouped by warehouse; document/send records the message
//   - tax/series metadata lists
//   - malformed JSON -> 400 (errorText), never a 500
func TestSmartBillStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "smartbill-style")
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
			"smartbill": {Adapter: absAdapterDir},
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

	base := addrs["smartbill"]
	const cif = "RO12345678"

	// ===== auth negatives =====
	r, _ := http.Get(base + "/tax")
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatalf("no-auth /tax -> %d, want 401", r.StatusCode)
	}
	reqBad, _ := http.NewRequest("GET", base+"/tax", nil)
	reqBad.Header.Set("Authorization", "Bearer nope")
	rBad, _ := http.DefaultClient.Do(reqBad)
	var eBody map[string]any
	_ = json.NewDecoder(rBad.Body).Decode(&eBody)
	rBad.Body.Close()
	if rBad.StatusCode != 401 {
		t.Fatalf("non-Basic -> %d, want 401", rBad.StatusCode)
	}
	if _, ok := eBody["errorText"].(string); !ok {
		t.Fatalf("401 body = %v, want errorText", eBody)
	}
	// Malformed base64 must not 5xx.
	reqMB, _ := http.NewRequest("GET", base+"/tax", nil)
	reqMB.Header.Set("Authorization", "Basic !!!not-base64!!!")
	rMB, err := http.DefaultClient.Do(reqMB)
	if err != nil {
		t.Fatal(err)
	}
	rMB.Body.Close()
	if rMB.StatusCode >= 500 {
		t.Fatalf("malformed base64 -> %d, want 4xx", rMB.StatusCode)
	}

	// ===== company bootstrap (simulator affordance) =====
	if _, st := sbillPost(t, base, "/sim/company", map[string]any{"cif": cif, "name": "Acme SRL"}, 201); st != 201 {
		t.Fatalf("company bootstrap -> %d", st)
	}

	// ===== malformed JSON -> 400 with errorText =====
	reqBad2, _ := http.NewRequest("POST", base+"/invoice", strings.NewReader(`{"seriesName": "FCT"`))
	reqBad2.Header.Set("Authorization", sbillAuth())
	reqBad2.Header.Set("Content-Type", "application/json")
	rBad2, _ := http.DefaultClient.Do(reqBad2)
	var eBad map[string]any
	_ = json.NewDecoder(rBad2.Body).Decode(&eBad)
	rBad2.Body.Close()
	if rBad2.StatusCode != 400 {
		t.Fatalf("malformed JSON -> %d, want 400", rBad2.StatusCode)
	}
	if _, ok := eBad["errorText"].(string); !ok {
		t.Fatalf("400 body = %v, want errorText", eBad)
	}

	// ===== invoice create: empty body, real field names, numeric amounts =====
	if _, st := sbillPost(t, base, "/invoice", map[string]any{
		"companyVatCode": cif,
		"seriesName":     "FCT",
		"currency":       "RON",
		"client":         map[string]any{"name": "Beta Corp", "vatCode": "RO98765432", "city": "Cluj"},
		"products": []map[string]any{
			{"name": "Consultanta", "price": 100.0, "quantity": 2.0, "taxPercentage": 19.0},
			{"name": "Suport", "price": 50.0, "quantity": 1.0, "taxPercentage": 0.0},
		},
	}, 200); st != 200 {
		t.Fatalf("invoice create -> %d", st)
	}

	inv := sbillGet(t, base, "/invoice?cif="+cif+"&seriesname=FCT&number=1")
	if inv["seriesName"] != "FCT" || fmt.Sprint(inv["number"]) != "1" {
		t.Fatalf("invoice round-trip: %v", inv)
	}
	client, _ := inv["client"].(map[string]any)
	if client["name"] != "Beta Corp" || client["vatCode"] != "RO98765432" {
		t.Fatalf("client object = %v", inv["client"])
	}
	// Numeric amounts: totals computed as JSON numbers.
	if inv["totalNet"].(float64) != 250.0 || inv["totalVAT"].(float64) != 38.0 || inv["invoiceTotalAmount"].(float64) != 288.0 {
		t.Fatalf("totals = net %v vat %v total %v, want 250/38/288", inv["totalNet"], inv["totalVAT"], inv["invoiceTotalAmount"])
	}
	if _, hasID := inv["id"]; hasID {
		t.Fatal("internal id must not leak")
	}

	// ===== paymentstatus: unpaid -> paid =====
	ps := sbillGet(t, base, "/invoice/paymentstatus?cif="+cif+"&seriesname=FCT&number=1")
	if ps["paid"] != false || ps["unpaidAmount"].(float64) != 288.0 {
		t.Fatalf("initial paymentstatus = %v", ps)
	}
	payBody := map[string]any{"payment": map[string]any{
		"companyVatCode": cif,
		"value":          288.0,
		"type":           "CHITANTA",
		"isCash":         true,
		"invoicesList":   []map[string]any{{"seriesName": "FCT", "number": "1"}},
	}}
	payResp, st := sbillPost(t, base, "/payment", payBody, 200)
	if st != 200 {
		t.Fatalf("payment add -> %d", st)
	}
	var payOut map[string]any
	_ = json.Unmarshal([]byte(payResp), &payOut)
	payID := fmt.Sprint(payOut["paymentId"])
	if payID == "" || payID == "<nil>" {
		t.Fatalf("payment create must disclose paymentId: %s", payResp)
	}
	ps = sbillGet(t, base, "/invoice/paymentstatus?cif="+cif+"&seriesname=FCT&number=1")
	if ps["paid"] != true || ps["paidAmount"].(float64) != 288.0 || ps["unpaidAmount"].(float64) != 0 {
		t.Fatalf("paid paymentstatus = %v", ps)
	}

	// Payment delete un-pays.
	sbillDelete(t, base, "/payment/v2", map[string]any{"companyVatCode": cif, "paymentId": payID}, 200)
	ps = sbillGet(t, base, "/invoice/paymentstatus?cif="+cif+"&seriesname=FCT&number=1")
	if ps["paid"] != false {
		t.Fatalf("after delete paymentstatus = %v", ps)
	}
	sbillDelete(t, base, "/payment/v2", map[string]any{"companyVatCode": cif, "paymentId": "999"}, 404)

	// ===== cancel + restore =====
	sbillPut(t, base, "/invoice/cancel", map[string]any{"companyVatCode": cif, "seriesName": "FCT", "number": "1", "cancellationTax": 0.0}, 200)
	inv = sbillGet(t, base, "/invoice?cif="+cif+"&seriesname=FCT&number=1")
	if inv["status"] != "canceled" {
		t.Fatalf("canceled status = %v", inv["status"])
	}
	sbillPut(t, base, "/invoice/restore", map[string]any{"companyVatCode": cif, "seriesName": "FCT", "number": "1"}, 200)
	inv = sbillGet(t, base, "/invoice?cif="+cif+"&seriesname=FCT&number=1")
	if inv["status"] != "active" {
		t.Fatalf("restored status = %v", inv["status"])
	}

	// ===== estimate create/get/cancel =====
	sbillPost(t, base, "/estimate", map[string]any{
		"companyVatCode": cif, "seriesName": "PRO",
		"client":   map[string]any{"name": "Gamma"},
		"products": []map[string]any{{"name": "Work", "price": 10.0, "quantity": 1.0, "taxPercentage": 19.0}},
	}, 200)
	est := sbillGet(t, base, "/estimate?cif="+cif+"&seriesname=PRO&number=1")
	if est["invoiceTotalAmount"].(float64) != 11.9 {
		t.Fatalf("estimate total = %v, want 11.9", est["invoiceTotalAmount"])
	}
	sbillPut(t, base, "/estimate/cancel", map[string]any{"companyVatCode": cif, "seriesName": "PRO", "number": "1"}, 200)
	est = sbillGet(t, base, "/estimate?cif="+cif+"&seriesname=PRO&number=1")
	if est["status"] != "canceled" {
		t.Fatalf("estimate canceled = %v", est["status"])
	}

	// ===== purchase (spend side) =====
	sbillPost(t, base, "/purchase", map[string]any{
		"companyVatCode": cif, "seriesName": "ACH",
		"supplier": map[string]any{"name": "Metro", "vatCode": "RO11112222"},
		"products": []map[string]any{{"name": "Birocuri", "price": 5.0, "quantity": 4.0, "taxPercentage": 19.0}},
	}, 200)
	pur := sbillGet(t, base, "/purchase?cif="+cif+"&seriesname=ACH&number=1")
	sup, _ := pur["supplier"].(map[string]any)
	if sup["name"] != "Metro" || pur["invoiceTotalAmount"].(float64) != 23.8 {
		t.Fatalf("purchase = %v", pur)
	}

	// ===== stocks: seed via sim movement, then the real grouped read =====
	sbillPost(t, base, "/sim/stocks/movement", map[string]any{
		"cif": cif, "productCode": "SKU-1", "productName": "Birocuri", "quantity": 10.0, "warehouseName": "Depot A",
	}, 200)
	stocks := sbillGet(t, base, "/stocks?cif="+cif)
	list, _ := stocks["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("stocks list = %v", stocks)
	}
	group := list[0].(map[string]any)
	wh, _ := group["warehouse"].(map[string]any)
	if wh["warehouseName"] != "Depot A" {
		t.Fatalf("warehouse group = %v", group)
	}
	prods, _ := group["products"].([]any)
	if prods[0].(map[string]any)["quantity"].(float64) != 10.0 {
		t.Fatalf("stock quantity = %v", prods[0])
	}
	filtered := sbillGet(t, base, "/stocks?cif="+cif+"&warehouseName=Nowhere")
	if n := len(filtered["list"].([]any)); n != 0 {
		t.Fatalf("warehouse filter -> %d groups, want 0", n)
	}

	// ===== document/send =====
	sbillPost(t, base, "/document/send", map[string]any{
		"sendDocumentRequest": map[string]any{
			"companyVatCode": cif, "seriesName": "FCT", "number": "1",
			"type": "factura", "subject": "Factura ta", "to": "client@example.test",
		},
	}, 200)

	// ===== metadata =====
	tax := sbillGet(t, base, "/tax")
	if n := len(tax["list"].([]any)); n < 3 {
		t.Fatalf("tax list = %v", tax)
	}
	ser := sbillGet(t, base, "/series")
	if n := len(ser["list"].([]any)); n < 2 {
		t.Fatalf("series list = %v", ser)
	}
}

func sbillPost(t *testing.T, base, path string, body map[string]any, want int) (string, int) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", base+path, bytes.NewReader(buf))
	req.Header.Set("Authorization", sbillAuth())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("POST %s -> %d (want %d): %s", path, resp.StatusCode, want, raw)
	}
	return string(raw), resp.StatusCode
}

func sbillPut(t *testing.T, base, path string, body map[string]any, want int) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", base+path, bytes.NewReader(buf))
	req.Header.Set("Authorization", sbillAuth())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("PUT %s -> %d (want %d): %s", path, resp.StatusCode, want, raw)
	}
}

func sbillDelete(t *testing.T, base, path string, body map[string]any, want int) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("DELETE", base+path, bytes.NewReader(buf))
	req.Header.Set("Authorization", sbillAuth())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("DELETE %s -> %d (want %d): %s", path, resp.StatusCode, want, raw)
	}
}

func sbillGet(t *testing.T, base, path string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("Authorization", sbillAuth())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s -> %d: %s", path, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
