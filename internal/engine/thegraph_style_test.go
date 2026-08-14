package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestTheGraphStyleAdapter exercises the The Graph subgraph GraphQL mock
// end-to-end:
//
//   - POST pools query → {data:{pools:[...]}} with correct fields
//   - POST tokens query → {data:{tokens:[...]}}
//   - POST domains query → {data:{domains:[...]}}
//   - GET schema/introspection → SDL string
//   - Nested token fields (token0{symbol})
func TestTheGraphStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "thegraph-style")
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
			"graph": {Adapter: absAdapterDir},
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

	base := addrs["graph"]

	// ===== POST pools query → {data:{pools:[...]}} =====

	poolsQuery := `{
  pools(first: 2, orderBy: volumeUSD, orderDirection: desc) {
    id
    token0 {
      symbol
    }
    token1 {
      symbol
    }
    totalValueLockedUSD
    volumeUSD
    feeTier
  }
}`
	body, status := graphQL(t, base, "/subgraphs/id/5zvR82QoaXYxfyKOCH8Qfl6pUCWd7YFXq56Y3ZSDXx2W", poolsQuery)
	if status != 200 {
		t.Fatalf("pools query -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	json.Unmarshal([]byte(body), &resp)
	if resp["errors"] != nil {
		t.Fatalf("pools query errors: %v", resp["errors"])
	}
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v, want object", resp["data"])
	}
	pools, ok := data["pools"].([]any)
	if !ok {
		t.Fatalf("pools = %v, want array", data["pools"])
	}
	if len(pools) == 0 {
		t.Fatal("pools empty, want at least 1")
	}
	firstPool := pools[0].(map[string]any)
	if firstPool["id"] == nil || firstPool["id"] == "" {
		t.Fatalf("pool id = %v, want non-empty", firstPool["id"])
	}
	// Check totalValueLockedUSD is present.
	if firstPool["totalValueLockedUSD"] == nil {
		t.Fatal("pool missing totalValueLockedUSD")
	}
	// Check token0 nested object.
	token0, ok := firstPool["token0"].(map[string]any)
	if !ok {
		t.Fatalf("token0 = %v, want object", firstPool["token0"])
	}
	if token0["symbol"] == nil || token0["symbol"] == "" {
		t.Fatalf("token0.symbol = %v, want non-empty", token0["symbol"])
	}
	// Check token1 nested object.
	token1, ok := firstPool["token1"].(map[string]any)
	if !ok {
		t.Fatalf("token1 = %v, want object", firstPool["token1"])
	}
	if token1["symbol"] == nil || token1["symbol"] == "" {
		t.Fatalf("token1.symbol = %v, want non-empty", token1["symbol"])
	}

	// ===== POST tokens query → {data:{tokens:[...]}} =====

	tokensQuery := `{
  tokens(first: 10) {
    id
    symbol
    name
    decimals
  }
}`
	body, status = graphQL(t, base, "/subgraphs/id/5zvR82QoaXYxfyKOCH8Qfl6pUCWd7YFXq56Y3ZSDXx2W", tokensQuery)
	if status != 200 {
		t.Fatalf("tokens query -> status %d, want 200; body %s", status, body)
	}
	var resp2 map[string]any
	json.Unmarshal([]byte(body), &resp2)
	data2 := resp2["data"].(map[string]any)
	tokens := data2["tokens"].([]any)
	if len(tokens) == 0 {
		t.Fatal("tokens empty, want at least 1")
	}
	firstToken := tokens[0].(map[string]any)
	if firstToken["symbol"] == nil {
		t.Fatal("token missing symbol")
	}
	if firstToken["name"] == nil {
		t.Fatal("token missing name")
	}
	if firstToken["decimals"] == nil {
		t.Fatal("token missing decimals")
	}

	// ===== POST domains query → {data:{domains:[...]}} =====

	domainsQuery := `{
  domains(first: 5) {
    id
    name
    labelName
    owner
    resolvedAddress
  }
}`
	body, status = graphQL(t, base, "/subgraphs/id/5XqPmWe6gZyrTtFjASCbxgykJ7KbAA8puFezV8vsJoEB", domainsQuery)
	if status != 200 {
		t.Fatalf("domains query -> status %d, want 200; body %s", status, body)
	}
	var resp3 map[string]any
	json.Unmarshal([]byte(body), &resp3)
	data3 := resp3["data"].(map[string]any)
	domains := data3["domains"].([]any)
	if len(domains) == 0 {
		t.Fatal("domains empty, want at least 1")
	}
	firstDomain := domains[0].(map[string]any)
	if firstDomain["name"] == nil || firstDomain["name"] == "" {
		t.Fatalf("domain name = %v, want non-empty", firstDomain["name"])
	}
	if firstDomain["owner"] == nil || firstDomain["owner"] == "" {
		t.Fatalf("domain owner = %v, want non-empty", firstDomain["owner"])
	}

	// ===== GET schema/introspection → SDL string =====

	req, err := http.NewRequest("GET", base+"/subgraphs/id/5zvR82QoaXYxfyKOCH8Qfl6pUCWd7YFXq56Y3ZSDXx2W/graphql", nil)
	if err != nil {
		t.Fatal(err)
	}
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer httpResp.Body.Close()
	b, _ := io.ReadAll(httpResp.Body)
	if httpResp.StatusCode != 200 {
		t.Fatalf("schema -> status %d, want 200", httpResp.StatusCode)
	}
	var schemaResp map[string]any
	json.Unmarshal(b, &schemaResp)
	schema, ok := schemaResp["data"].(string)
	if !ok || schema == "" {
		t.Fatalf("schema = %v, want non-empty string", schemaResp["data"])
	}
	if !containsStr(schema, "type Pool") {
		t.Fatalf("schema missing 'type Pool': %s", schema[:min(200, len(schema))])
	}
	if !containsStr(schema, "type Token") {
		t.Fatalf("schema missing 'type Token'")
	}

	// ===== GET ENS schema =====

	req2, err := http.NewRequest("GET", base+"/subgraphs/id/5XqPmWe6gZyrTtFjASCbxgykJ7KbAA8puFezV8vsJoEB/graphql", nil)
	if err != nil {
		t.Fatal(err)
	}
	httpResp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer httpResp2.Body.Close()
	b2, _ := io.ReadAll(httpResp2.Body)
	var schemaResp2 map[string]any
	json.Unmarshal(b2, &schemaResp2)
	ensSchema := schemaResp2["data"].(string)
	if !containsStr(ensSchema, "type Domain") {
		t.Fatalf("ENS schema missing 'type Domain'")
	}

	// ===== Combined query (pools + tokens) =====

	combinedQuery := `{
  pools(first: 1) {
    id
  }
  tokens(first: 1) {
    id
    symbol
  }
}`
	body, status = graphQL(t, base, "/subgraphs/id/5zvR82QoaXYxfyKOCH8Qfl6pUCWd7YFXq56Y3ZSDXx2W", combinedQuery)
	if status != 200 {
		t.Fatalf("combined query -> status %d, want 200; body %s", status, body)
	}
	var respC map[string]any
	json.Unmarshal([]byte(body), &respC)
	dataC := respC["data"].(map[string]any)
	if dataC["pools"] == nil {
		t.Fatal("combined query missing pools")
	}
	if dataC["tokens"] == nil {
		t.Fatal("combined query missing tokens")
	}
}

// TestTheGraphStyleWhereInFilters covers where-clause list filters:
// token0_not_in: [...] must EXCLUDE the listed token, and token0_in: [...]
// must match only the listed token (regression: _not_in used to be inverted
// into a positive in filter, and list _in kept its suffix on the field name).
func TestTheGraphStyleWhereInFilters(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "thegraph-style")
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
			"graph": {Adapter: absAdapterDir},
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

	base := addrs["graph"]
	endpoint := "/subgraphs/id/5zvR82QoaXYxfyKOCH8Qfl6pUCWd7YFXq56Y3ZSDXx2W"

	// USDC is token0 of two seeded pools; WBTC of one.
	usdc := "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"
	wbtcPool := "0x11b815efb8f581194ae79006d24e0d814b7697f6"

	// ===== token0_not_in: [USDC] → only the WBTC pool =====

	notInQuery := `{
  pools(first: 10, where: { token0_not_in: ["` + usdc + `"] }) {
    id
  }
}`
	body, status := graphQL(t, base, endpoint, notInQuery)
	if status != 200 {
		t.Fatalf("not_in query -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	json.Unmarshal([]byte(body), &resp)
	if resp["errors"] != nil {
		t.Fatalf("not_in query errors: %v", resp["errors"])
	}
	pools := resp["data"].(map[string]any)["pools"].([]any)
	if len(pools) != 1 {
		t.Fatalf("not_in pools = %v, want exactly the WBTC pool", pools)
	}
	if got := pools[0].(map[string]any)["id"]; got != wbtcPool {
		t.Fatalf("not_in pool id = %v, want %s", got, wbtcPool)
	}

	// ===== token0_in: [USDC] → the two USDC pools, not the WBTC pool =====

	inQuery := `{
  pools(first: 10, where: { token0_in: ["` + usdc + `"] }) {
    id
  }
}`
	body, status = graphQL(t, base, endpoint, inQuery)
	if status != 200 {
		t.Fatalf("in query -> status %d, want 200; body %s", status, body)
	}
	var resp2 map[string]any
	json.Unmarshal([]byte(body), &resp2)
	pools2 := resp2["data"].(map[string]any)["pools"].([]any)
	if len(pools2) != 2 {
		t.Fatalf("in pools = %v, want the two USDC pools", pools2)
	}
	for _, p := range pools2 {
		if got := p.(map[string]any)["id"]; got == wbtcPool {
			t.Fatalf("in pools contained the WBTC pool %v; want only USDC pools", pools2)
		}
	}

	// ===== scalar token0_not: WBTC → the two USDC pools =====

	notQuery := `{
  pools(first: 10, where: { token0_not: "0x2260fac5e5542a773aa44fbcfedf7c193bc2b5f0" }) {
    id
  }
}`
	body, status = graphQL(t, base, endpoint, notQuery)
	if status != 200 {
		t.Fatalf("not query -> status %d, want 200; body %s", status, body)
	}
	var resp3 map[string]any
	json.Unmarshal([]byte(body), &resp3)
	pools3 := resp3["data"].(map[string]any)["pools"].([]any)
	if len(pools3) != 2 {
		t.Fatalf("not pools = %v, want the two USDC pools", pools3)
	}
}

// === helpers ===

func graphQL(t *testing.T, base, path, query string) (string, int) {
	t.Helper()
	bodyObj := map[string]any{"query": query}
	data, _ := json.Marshal(bodyObj)
	req, err := http.NewRequest("POST", base+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(sub) > 0 && indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
