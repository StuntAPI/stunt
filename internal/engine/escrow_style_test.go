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

// TestEscrowStyleAdapter exercises the 2017-09-01 transaction lifecycle:
//
//   - create requires a buyer and a seller
//   - both parties agree via PATCH, one email each
//   - not secured until funded; the /sim fund affordance secures it
//   - lookup by numeric id and by caller reference
//   - webhook registration round-trip
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

	// ===== create: buyer and seller are both required =====
	_, status := escPost(t, base, "/2017-09-01/transaction", map[string]any{
		"description": "No parties",
		"parties":     []map[string]any{{"role": "buyer", "customer": "buyer@sim.invalid"}},
		"items":       []map[string]any{},
	})
	if status != 400 {
		t.Fatalf("create without seller -> %d, want 400", status)
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
	json.Unmarshal([]byte(created), &tx)
	txID := fmt.Sprint(tx["id"])

	if tx["status"].(map[string]any)["secured"] == true {
		t.Fatal("a fresh transaction must not be secured")
	}

	// ===== agree: buyer, then seller =====
	for _, email := range []string{"buyer@sim.invalid", "seller@sim.invalid"} {
		_, status := escPatch(t, base, "/2017-09-01/transaction/"+txID, map[string]any{
			"action": "agree", "customer": email,
		})
		if status != 200 {
			t.Fatalf("agree as %s -> %d, want 200", email, status)
		}
	}

	// Agreed but unfunded: still not secured.
	got, status := escGet(t, base, "/2017-09-01/transaction/"+txID)
	if status != 200 {
		t.Fatalf("get -> %d", status)
	}
	if got["status"].(map[string]any)["secured"] == true {
		t.Fatal("agreement alone must not secure funds")
	}

	// ===== fund via the simulator affordance, then it is secured =====
	if _, status := escPost(t, base, "/sim/transaction/"+txID+"/fund", map[string]any{}); status != 200 {
		t.Fatalf("sim fund -> %d, want 200", status)
	}
	got, _ = escGet(t, base, "/2017-09-01/transaction/"+txID)
	if got["status"].(map[string]any)["secured"] != true {
		t.Fatal("funded transaction must be secured")
	}

	// ===== lookup by reference =====
	byRef, status := escGet(t, base, "/2017-09-01/transaction/reference/my-ref-001")
	if status != 200 || fmt.Sprint(byRef["id"]) != txID {
		t.Fatalf("by reference -> %d (id %v), want 200 / %s", status, byRef["id"], txID)
	}

	// ===== webhook registration =====
	if _, status := escPost(t, base, "/2017-09-01/customer/me/webhook", map[string]any{
		"url": "https://sink.test/hook",
	}); status != 201 {
		t.Fatalf("webhook create -> %d, want 201", status)
	}
	list, status := escGet(t, base, "/2017-09-01/customer/me/webhook")
	listJSON, _ := json.Marshal(list)
	if status != 200 || !strings.Contains(string(listJSON), "sink.test") {
		t.Fatalf("webhook list -> %d: %s", status, listJSON)
	}
}

func escPost(t *testing.T, base, path string, body map[string]any) (string, int) {
	t.Helper()
	buf, _ := json.Marshal(body)
	resp, err := http.Post(base+path, "application/json", bytes.NewReader(buf))
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
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out, resp.StatusCode
}
