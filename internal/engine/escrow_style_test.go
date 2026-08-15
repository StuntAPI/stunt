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

// escrowBasic is the adapter's documented synthetic basic-auth credential.
func escrowBasic() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("escrow-test:escrow-test-api-key"))
}

// TestEscrowStyleAdapter exercises the 2017-09-01 transaction lifecycle:
//
//   - basic auth enforced (401 without it)
//   - create requires a buyer and a seller, with the real nested error shape
//   - the initiating party auto-agrees; the second party agrees via PATCH
//   - funding state lives on items[].schedule[].status.secured only
//   - amounts render as decimal strings
//   - lookup by numeric id, by caller reference, and via the list endpoint
//   - webhook registration round-trip with stable int ids
//   - malformed JSON -> 400, not 500
func TestEscrowStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "escrow-style")
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
			"escrow": {Adapter: absAdapterDir},
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

	base := addrs["escrow"]

	// ===== 401 without basic auth =====
	buf, _ := json.Marshal(map[string]any{
		"description": "No parties", "parties": []map[string]any{}, "items": []map[string]any{},
	})
	noAuthResp, err := http.Post(base+"/2017-09-01/transaction", "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	noAuthResp.Body.Close()
	if noAuthResp.StatusCode != 401 {
		t.Fatalf("create without auth -> %d, want 401", noAuthResp.StatusCode)
	}
	var _ = err

	// ===== create: buyer and seller are both required (nested error shape) =====
	body, status := escPost(t, base, "/2017-09-01/transaction", map[string]any{
		"description": "No parties",
		"parties":     []map[string]any{{"role": "buyer", "customer": "buyer@sim.invalid"}},
		"items":       []map[string]any{},
	})
	if status != 400 {
		t.Fatalf("create without seller -> %d, want 400; %s", status, body)
	}
	var verr map[string]any
	_ = json.Unmarshal([]byte(body), &verr)
	if _, ok := verr["errors"].(map[string]any); !ok {
		t.Fatalf("validation error shape = %v, want nested {errors:{parties:...}}", verr)
	}

	// ===== malformed JSON -> 400, never a 500 =====
	req, _ := http.NewRequest("POST", base+"/2017-09-01/transaction", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", escrowBasic())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("malformed JSON -> %d, want 400", resp.StatusCode)
	}

	// ===== create the real one =====
	created, status := escPost(t, base, "/2017-09-01/transaction", map[string]any{
		"description": "Website sale",
		"currency":    "usd",
		"reference":   "my-ref-001",
		"parties": []map[string]any{
			{"role": "buyer", "customer": "buyer@sim.invalid"},
			{"role": "seller", "customer": "seller@sim.invalid"},
		},
		"items": []map[string]any{{
			"title":       "Website",
			"description": "The site",
			"schedule": []map[string]any{{
				"amount":               1000.0,
				"payer_customer":       "buyer@sim.invalid",
				"beneficiary_customer": "seller@sim.invalid",
			}},
		}},
	})
	if status != 201 {
		t.Fatalf("create -> %d, want 201; body %s", status, created)
	}
	var tx map[string]any
	_ = json.Unmarshal([]byte(created), &tx)
	txID := fmt.Sprint(tx["id"])

	// No top-level status object on the real Transaction shape.
	if _, exists := tx["status"]; exists {
		t.Fatal("transaction must not carry a top-level status object")
	}

	// Amounts render as decimal strings like the real API.
	item0 := tx["items"].([]any)[0].(map[string]any)
	sched0 := item0["schedule"].([]any)[0].(map[string]any)
	if amt, _ := sched0["amount"].(string); amt != "1000.00" {
		t.Fatalf("schedule amount = %#v, want \"1000.00\"", sched0["amount"])
	}
	if fee := item0["fees"].([]any)[0].(map[string]any)["amount"]; fee != "32.50" {
		t.Fatalf("escrow fee = %#v, want \"32.50\" (3.25%% of 1000)", fee)
	}

	// The initiating (first) party is auto-agreed.
	parties := tx["parties"].([]any)
	if parties[0].(map[string]any)["agreed"] != true {
		t.Fatal("initiating party must be auto-agreed at creation")
	}
	if parties[1].(map[string]any)["agreed"] != false {
		t.Fatal("counterparty must not be agreed yet")
	}

	// ===== agree without a customer -> 400 =====
	_, status = escPatch(t, base, "/2017-09-01/transaction/"+txID, map[string]any{"action": "agree"})
	if status != 400 {
		t.Fatalf("agree without customer -> %d, want 400", status)
	}

	// ===== the seller agrees =====
	if _, status := escPatch(t, base, "/2017-09-01/transaction/"+txID, map[string]any{
		"action": "agree", "customer": "seller@sim.invalid",
	}); status != 200 {
		t.Fatalf("agree as seller -> %d, want 200", status)
	}

	// Agreed but unfunded: schedule not secured.
	got, status := escGet(t, base, "/2017-09-01/transaction/"+txID)
	if status != 200 {
		t.Fatalf("get -> %d", status)
	}
	sg := got["items"].([]any)[0].(map[string]any)["schedule"].([]any)[0].(map[string]any)["status"].(map[string]any)
	if sg["secured"] == true {
		t.Fatal("agreement alone must not secure funds")
	}

	// ===== fund via the simulator affordance, then the schedule is secured =====
	if _, status := escPost(t, base, "/sim/transaction/"+txID+"/fund", map[string]any{}); status != 200 {
		t.Fatalf("sim fund -> %d, want 200", status)
	}
	got, _ = escGet(t, base, "/2017-09-01/transaction/"+txID)
	sg = got["items"].([]any)[0].(map[string]any)["schedule"].([]any)[0].(map[string]any)["status"].(map[string]any)
	if sg["secured"] != true {
		t.Fatal("funded transaction must be secured")
	}

	// ===== accept closes it =====
	if _, status := escPatch(t, base, "/2017-09-01/transaction/"+txID, map[string]any{"action": "accept"}); status != 200 {
		t.Fatalf("accept -> %d, want 200", status)
	}
	got, _ = escGet(t, base, "/2017-09-01/transaction/"+txID)
	if got["close_date"] == nil {
		t.Fatal("accept must set close_date")
	}

	// ===== lookup by reference =====
	byRef, status := escGet(t, base, "/2017-09-01/transaction/reference/my-ref-001")
	if status != 200 || fmt.Sprint(byRef["id"]) != txID {
		t.Fatalf("by reference -> %d (id %v), want 200 / %s", status, byRef["id"], txID)
	}

	// ===== list endpoint =====
	lst, status := escGet(t, base, "/2017-09-01/transaction")
	if status != 200 {
		t.Fatalf("list -> %d", status)
	}
	if n := len(lst["transactions"].([]any)); n != 1 {
		t.Fatalf("list has %d transactions, want 1", n)
	}

	// ===== webhook registration: ids stable between create and list =====
	wb, status := escPost(t, base, "/2017-09-01/customer/me/webhook", map[string]any{
		"url": "https://sink.test/hook",
	})
	if status != 201 {
		t.Fatalf("webhook create -> %d, want 201; %s", status, wb)
	}
	var hook map[string]any
	_ = json.Unmarshal([]byte(wb), &hook)
	hookID, _ := hook["id"].(float64)
	list, status := escGet(t, base, "/2017-09-01/customer/me/webhook")
	listJSON, _ := json.Marshal(list)
	if status != 200 || !strings.Contains(string(listJSON), "sink.test") {
		t.Fatalf("webhook list -> %d: %s", status, listJSON)
	}
	listed := list["webhooks"].([]any)[0].(map[string]any)
	if listed["id"].(float64) != hookID {
		t.Fatalf("webhook id drifted: created %v, listed %v", hookID, listed["id"])
	}
}

func escPost(t *testing.T, base, path string, body map[string]any) (string, int) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", base+path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", escrowBasic())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw), resp.StatusCode
}

func escPatch(t *testing.T, base, path string, body map[string]any) (string, int) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", base+path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", escrowBasic())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw), resp.StatusCode
}

func escGet(t *testing.T, base, path string) (map[string]any, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("Authorization", escrowBasic())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out, resp.StatusCode
}
