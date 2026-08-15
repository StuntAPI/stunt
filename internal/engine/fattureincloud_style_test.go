package engine

import (
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

// TestFattureInCloudStyleAdapter exercises the v2 bookkeeping surface:
//
//   - 401 without a Bearer token (flat OAuth error shape)
//   - company discovery via GET /user/companies; unknown company ids are 404s
//   - received-document create + list with date filtering and the Laravel
//     pagination envelope (last_page must be followed)
//   - amounts are decimal strings
//   - metodata endpoint reports categories in use
//   - suppliers CRUD round-trip
//   - webhooks are account-level and independent of company scoping
func TestFattureInCloudStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "fattureincloud-style")
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
			"fatture": {Adapter: absAdapterDir},
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

	base := addrs["fatture"]
	const token = "Bearer fic-local-test"

	// ===== 401 without Authorization =====
	resp, err := http.Get(base + "/user/companies")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no-auth /user/companies -> %d, want 401", resp.StatusCode)
	}

	// ===== 401 uses the flat OAuth shape =====
	req401, _ := http.NewRequest("GET", base+"/user/companies", nil)
	r401, err := http.DefaultClient.Do(req401)
	if err != nil {
		t.Fatal(err)
	}
	defer r401.Body.Close()
	var e401 map[string]any
	_ = json.NewDecoder(r401.Body).Decode(&e401)
	if _, ok := e401["error"].(string); !ok {
		t.Fatalf("401 shape = %v, want flat {error, error_description}", e401)
	}

	// ===== unknown company 404 uses the nested SCREAMING_SNAKE shape =====
	var e404 map[string]any
	req404b, _ := http.NewRequest("GET", base+"/c/999999/received_documents", nil)
	req404b.Header.Set("Authorization", "Bearer fic-local-test")
	resp404b, err := http.DefaultClient.Do(req404b)
	if err != nil {
		t.Fatal(err)
	}
	defer resp404b.Body.Close()
	_ = json.NewDecoder(resp404b.Body).Decode(&e404)
	if code, _ := e404["error"].(map[string]any)["code"].(string); code != "NOT_FOUND" {
		t.Fatalf("404 error.code = %v, want NOT_FOUND", e404)
	}

	// The v2 API has no create-company endpoint; the seeded company is
	// discovered through the real list surface.
	list := ficGet(t, base, "/user/companies")
	companies := list["data"].(map[string]any)["companies"].([]any)
	if len(companies) < 1 {
		t.Fatal("no seeded company")
	}
	companyID := fmt.Sprint(companies[0].(map[string]any)["id"])
	docPath := fmt.Sprintf("/c/%s/received_documents", companyID)

	// ===== malformed JSON -> 400, never a 500 =====
	reqBad, _ := http.NewRequest("POST", base+"/c/"+companyID+"/suppliers", strings.NewReader("{bad"))
	reqBad.Header.Set("Authorization", "Bearer fic-local-test")
	reqBad.Header.Set("Content-Type", "application/json")
	rBad, err := http.DefaultClient.Do(reqBad)
	if err != nil {
		t.Fatal(err)
	}
	rBad.Body.Close()
	if rBad.StatusCode != 400 {
		t.Fatalf("malformed JSON -> %d, want 400", rBad.StatusCode)
	}

	// ===== unknown company id is a 404 (auth still required) =====
	req404, _ := http.NewRequest("GET", base+"/c/999999/received_documents", nil)
	req404.Header.Set("Authorization", "Bearer fic-local-test")
	resp404, err := http.DefaultClient.Do(req404)
	if err != nil {
		t.Fatal(err)
	}
	resp404.Body.Close()
	if resp404.StatusCode != 404 {
		t.Fatalf("unknown company -> %d, want 404", resp404.StatusCode)
	}

	// ===== five documents across two months (decimal-string amounts) =====
	for i := 1; i <= 5; i++ {
		ficCreate(t, base, docPath, map[string]any{
			"date":       fmt.Sprintf("2026-06-%02d", i),
			"category":   "groceries",
			"amount_net": "100.00",
		})
	}
	ficCreate(t, base, docPath, map[string]any{
		"date": "2026-07-01", "category": "utilities", "amount_net": "1450.40",
	})

	// ===== date filter + pagination envelope =====
	page := ficGet(t, base, docPath+"?per_page=2&page=1&date_start=2026-06-01&date_end=2026-06-30")
	if page["total"].(float64) != 5 || page["last_page"].(float64) != 3 {
		t.Fatalf("june filter: total %v last_page %v, want 5/3", page["total"], page["last_page"])
	}
	if len(page["data"].([]any)) != 2 {
		t.Fatalf("per_page=2 returned %d rows", len(page["data"].([]any)))
	}
	first := page["data"].([]any)[0].(map[string]any)
	if _, ok := first["amount_net"].(string); !ok {
		t.Fatalf("amount_net is %T, want decimal string", first["amount_net"])
	}

	// ===== metodata: categories in use =====
	meta := ficGet(t, base, docPath+"/info")
	cats := meta["data"].(map[string]any)["categories"].([]any)
	joined := fmt.Sprint(cats)
	if !strings.Contains(joined, "groceries") || !strings.Contains(joined, "utilities") {
		t.Fatalf("metodata categories: %v", cats)
	}

	// ===== suppliers CRUD =====
	supplierID := ficCreate(t, base, fmt.Sprintf("/c/%s/suppliers", companyID), map[string]any{"name": "Metro"})
	got := ficGet(t, base, fmt.Sprintf("/c/%s/suppliers/%s", companyID, supplierID))
	if got["data"].(map[string]any)["name"] != "Metro" {
		t.Fatalf("supplier round-trip: %v", got)
	}

	// ===== subscriptions are company-scoped, real shape (data.sink, SUB ids) =====
	sub := ficCreateRaw(t, base, fmt.Sprintf("/c/%s/subscriptions", companyID), map[string]any{
		"data": map[string]any{"sink": "https://sink.test/hook", "types": []string{"it.fattureincloud.entities.supplier.create"}},
	})
	var subDoc struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal([]byte(sub), &subDoc)
	if id, _ := subDoc.Data["id"].(string); !strings.HasPrefix(id, "SUB") {
		t.Fatalf("subscription id = %v, want SUB*", subDoc.Data["id"])
	}
	if subDoc.Data["sink"] != "https://sink.test/hook" {
		t.Fatalf("subscription sink = %v", subDoc.Data["sink"])
	}
	subs := ficGet(t, base, fmt.Sprintf("/c/%s/subscriptions", companyID))
	if subs["total"].(float64) != 1 {
		t.Fatalf("subscription list total %v, want 1", subs["total"])
	}
}

func ficCreate(t *testing.T, base, path string, body map[string]any) string {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(string(buf)))
	req.Header.Set("Authorization", "Bearer fic-local-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("POST %s -> %d, want 201", path, resp.StatusCode)
	}
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(out.Data["id"])
}

func ficCreateRaw(t *testing.T, base, path string, body map[string]any) string {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(string(buf)))
	req.Header.Set("Authorization", "Bearer fic-local-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 {
		t.Fatalf("POST %s -> %d, want 201", path, resp.StatusCode)
	}
	return string(raw)
}

func ficGet(t *testing.T, base, path string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("Authorization", "Bearer fic-local-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s -> %d, want 200", path, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
