package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// The Graph adapter serves real GraphQL execution from the graphql:
// transport at the seeded deployment id path. Queries below exercise
// arguments, variables, aliases, nested joins, where filters, _meta, and
// REAL GraphQL validation failures (unknown fields → errors[]).
const theGraphEndpoint = "/subgraphs/id/5zvR82QoaXYxfyKOCH8Qfl6p"

// setupTheGraph serves the committed thegraph-style adapter and returns its
// HTTP base URL + cleanup.
func setupTheGraph(t *testing.T) (string, func()) {
	t.Helper()

	absDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "thegraph-style"))
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"graph": {Adapter: absDir},
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
	time.Sleep(50 * time.Millisecond)

	url := addrs["graph"]
	cleanup := func() {
		cancel()
		e.Close()
	}
	return url, cleanup
}

// theGraphPost sends a GraphQL POST (query + optional variables) and
// returns the decoded response + status.
func theGraphPost(t *testing.T, base, query string, variables map[string]any) (map[string]any, int) {
	t.Helper()
	body := map[string]any{"query": query}
	if variables != nil {
		body["variables"] = variables
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", base+theGraphEndpoint, strings.NewReader(string(data)))
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
	var result map[string]any
	json.Unmarshal(b, &result)
	if resp.StatusCode != 200 {
		t.Logf("graphql response (%d): %s", resp.StatusCode, string(b))
	}
	return result, resp.StatusCode
}

// theGraphErrors returns the response errors[] as a display string, or ""
// when there are none.
func theGraphErrors(result map[string]any) string {
	if result == nil {
		return "<nil response>"
	}
	if errs, ok := result["errors"]; ok && errs != nil {
		if arr, ok := errs.([]any); ok && len(arr) > 0 {
			var msgs []string
			for _, e := range arr {
				if m, ok := e.(map[string]any); ok {
					if msg, ok := m["message"].(string); ok {
						msgs = append(msgs, msg)
					}
				}
			}
			if len(msgs) > 0 {
				return strings.Join(msgs, "; ")
			}
		}
		return "<non-empty errors>"
	}
	return ""
}

// TestTheGraphStylePoolsQueryArgsAndJoins verifies a real pools query with
// collection arguments (first/orderBy/orderDirection) plus the Pool.token0
// / token1 relational join against the tokens collection.
func TestTheGraphStylePoolsQueryArgsAndJoins(t *testing.T) {
	base, cleanup := setupTheGraph(t)
	defer cleanup()

	query := `{
		pools(first: 2, orderBy: volumeUSD, orderDirection: desc) {
			id
			token0 { id symbol decimals }
			token1 { symbol }
			totalValueLockedUSD
			volumeUSD
			feeTier
		}
	}`
	result, status := theGraphPost(t, base, query, nil)
	if status != 200 {
		t.Fatalf("pools query -> %d, want 200", status)
	}
	if errs := theGraphErrors(result); errs != "" {
		t.Fatalf("pools query errors: %s", errs)
	}

	data := result["data"].(map[string]any)
	pools := data["pools"].([]any)
	if len(pools) != 2 {
		t.Fatalf("pools length = %d, want 2 (first: 2)", len(pools))
	}
	first := pools[0].(map[string]any)
	// Highest volumeUSD pool first (8912345678.9 > 4567890123.4 > 2345678901.2).
	if got := first["id"]; got != "0x88e6a0c2ddd26feeb64f039a2c41296fcb3f5640" {
		t.Fatalf("pools[0].id = %v, want the USDC/WETH pool", got)
	}
	token0 := first["token0"].(map[string]any)
	if token0["symbol"] != "USDC" {
		t.Fatalf("token0.symbol = %v, want USDC", token0["symbol"])
	}
	if token0["decimals"] != float64(6) {
		t.Fatalf("token0.decimals = %v (%T), want 6 (Int, not the stored string)", token0["decimals"], token0["decimals"])
	}
	token1 := first["token1"].(map[string]any)
	if token1["symbol"] != "WETH" {
		t.Fatalf("token1.symbol = %v, want WETH", token1["symbol"])
	}
	// BigInt/BigDecimal serialize as decimal strings (graph-node wire form).
	if first["feeTier"] != "500" {
		t.Fatalf("feeTier = %v (%T), want \"500\" string", first["feeTier"], first["feeTier"])
	}
}

// TestTheGraphStyleVariablesAndAliases verifies variables, field arguments,
// and aliases in one document, plus skip pagination.
func TestTheGraphStyleVariablesAndAliases(t *testing.T) {
	base, cleanup := setupTheGraph(t)
	defer cleanup()

	query := `query($sym: String) {
		weth: tokens(first: 5, where: {symbol: $sym}) { id symbol }
		usdc: tokens(first: 5, where: {symbol: "USDC"}) { id symbol }
		page2: tokens(first: 2, skip: 2) { symbol }
	}`
	result, status := theGraphPost(t, base, query, map[string]any{"sym": "WETH"})
	if status != 200 {
		t.Fatalf("tokens query -> %d, want 200", status)
	}
	if errs := theGraphErrors(result); errs != "" {
		t.Fatalf("tokens query errors: %s", errs)
	}

	data := result["data"].(map[string]any)
	weth := data["weth"].([]any)
	if len(weth) != 1 || weth[0].(map[string]any)["symbol"] != "WETH" {
		t.Fatalf("weth alias = %v, want the single WETH token", weth)
	}
	usdc := data["usdc"].([]any)
	if len(usdc) != 1 || usdc[0].(map[string]any)["symbol"] != "USDC" {
		t.Fatalf("usdc alias = %v, want the single USDC token", usdc)
	}
	page2 := data["page2"].([]any)
	if len(page2) != 2 {
		t.Fatalf("page2 (first: 2, skip: 2) = %d items, want 2", len(page2))
	}
}

// TestTheGraphStyleDomainsAndMeta verifies the merged ENS-style entity set
// (Domain + Account owner join), single-entity lookup, and _meta.
func TestTheGraphStyleDomainsAndMeta(t *testing.T) {
	base, cleanup := setupTheGraph(t)
	defer cleanup()

	query := `{
		domains(first: 10, orderBy: createdAt, orderDirection: asc) {
			id
			name
			labelName
			owner { id }
			resolvedAddress { id }
			createdAt
		}
		_meta { deployment network block { number } hasIndexingErrors }
	}`
	result, status := theGraphPost(t, base, query, nil)
	if status != 200 {
		t.Fatalf("domains query -> %d, want 200", status)
	}
	if errs := theGraphErrors(result); errs != "" {
		t.Fatalf("domains query errors: %s", errs)
	}

	data := result["data"].(map[string]any)
	domains := data["domains"].([]any)
	if len(domains) != 3 {
		t.Fatalf("domains length = %d, want 3", len(domains))
	}
	first := domains[0].(map[string]any)
	if first["name"] != "vitalik.eth" {
		t.Fatalf("domains[0].name = %v, want vitalik.eth (createdAt asc)", first["name"])
	}
	owner := first["owner"].(map[string]any)
	if !strings.HasPrefix(owner["id"].(string), "0x") || len(owner["id"].(string)) != 42 {
		t.Fatalf("owner.id = %v, want the 0x… account address", owner["id"])
	}

	meta := data["_meta"].(map[string]any)
	if meta["network"] != "mainnet" {
		t.Fatalf("_meta.network = %v, want mainnet", meta["network"])
	}
	block := meta["block"].(map[string]any)
	if _, ok := block["number"].(float64); !ok {
		t.Fatalf("_meta.block.number = %v (%T), want a number", block["number"], block["number"])
	}

	// Single-entity lookup by id, then a miss returning null (not an error).
	single := `query($id: ID!) { domain(id: $id) { name owner { id } } }`
	result, _ = theGraphPost(t, base, single, map[string]any{"id": first["id"].(string)})
	domain := result["data"].(map[string]any)["domain"].(map[string]any)
	if domain["name"] != "vitalik.eth" {
		t.Fatalf("domain(id) name = %v, want vitalik.eth", domain["name"])
	}
	result, _ = theGraphPost(t, base, single, map[string]any{"id": "0xmissing"})
	if d := result["data"].(map[string]any)["domain"]; d != nil {
		t.Fatalf("domain(0xmissing) = %v, want null", d)
	}
}

// TestTheGraphStyleWhereFilters covers where-clause operators mapped onto
// query_select: _not_in exclusion, _in membership, scalar _not, numeric
// equality, and numeric comparison on a decimal-string field.
func TestTheGraphStyleWhereFilters(t *testing.T) {
	base, cleanup := setupTheGraph(t)
	defer cleanup()

	usdc := "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"
	wbtcPool := "0x11b815efb8f581194ae79006d24e0d814b7697f6"

	// token0_not_in: [USDC] → only the WBTC pool.
	result, status := theGraphPost(t, base,
		`{ pools(first: 10, where: { token0_not_in: ["`+usdc+`"] }) { id } }`, nil)
	if status != 200 {
		t.Fatalf("not_in query -> %d, want 200", status)
	}
	if errs := theGraphErrors(result); errs != "" {
		t.Fatalf("not_in query errors: %s", errs)
	}
	pools := result["data"].(map[string]any)["pools"].([]any)
	if len(pools) != 1 || pools[0].(map[string]any)["id"] != wbtcPool {
		t.Fatalf("token0_not_in pools = %v, want exactly the WBTC pool", pools)
	}

	// token0_in: [USDC] → the two USDC pools, not the WBTC pool.
	result, _ = theGraphPost(t, base,
		`{ pools(first: 10, where: { token0_in: ["`+usdc+`"] }) { id } }`, nil)
	pools = result["data"].(map[string]any)["pools"].([]any)
	if len(pools) != 2 {
		t.Fatalf("token0_in pools = %v, want the two USDC pools", pools)
	}
	for _, p := range pools {
		if p.(map[string]any)["id"] == wbtcPool {
			t.Fatalf("token0_in pools contained the WBTC pool: %v", pools)
		}
	}

	// Scalar token0_not: WBTC → the two USDC pools.
	result, _ = theGraphPost(t, base,
		`{ pools(first: 10, where: { token0_not: "0x2260fac5e5542a773aa44fbcfedf7c193bc2b5f0" }) { id } }`, nil)
	pools = result["data"].(map[string]any)["pools"].([]any)
	if len(pools) != 2 {
		t.Fatalf("token0_not pools = %v, want the two USDC pools", pools)
	}

	// Numeric equality on a BigInt-stored-as-string field.
	result, _ = theGraphPost(t, base, `{ pools(first: 10, where: { feeTier: "3000" }) { id feeTier } }`, nil)
	pools = result["data"].(map[string]any)["pools"].([]any)
	if len(pools) != 1 || pools[0].(map[string]any)["id"] != wbtcPool {
		t.Fatalf("feeTier=3000 pools = %v, want the 0.3%% pool", pools)
	}

	// Numeric comparison on a BigDecimal-stored-as-string field: TVL > 100M
	// keeps all three pools; txCount_gt keeps only the two busiest.
	result, _ = theGraphPost(t, base, `{ pools(first: 10, where: { txCount_gt: "700000" }) { id txCount } }`, nil)
	pools = result["data"].(map[string]any)["pools"].([]any)
	if len(pools) != 2 {
		t.Fatalf("txCount_gt pools = %v, want 2 (1234567, 890123)", pools)
	}
}

// TestTheGraphStyleUnknownFieldErrors verifies REAL GraphQL validation:
// an unknown field is rejected with a 400 and errors[] carrying the field
// name — the old substring matcher silently returned {"data": {}}.
func TestTheGraphStyleUnknownFieldErrors(t *testing.T) {
	base, cleanup := setupTheGraph(t)
	defer cleanup()

	// Unknown root field.
	result, status := theGraphPost(t, base, `{ swaps(first: 5) { id } }`, nil)
	if status != 400 {
		t.Fatalf("unknown root field -> %d, want 400", status)
	}
	if errs := theGraphErrors(result); !strings.Contains(errs, "swaps") {
		t.Fatalf("unknown root field errors = %s, want a message naming swaps", errs)
	}

	// Unknown field on a known type.
	result, status = theGraphPost(t, base, `{ pools(first: 1) { id sqrtPrice } }`, nil)
	if status != 400 {
		t.Fatalf("unknown Pool field -> %d, want 400", status)
	}
	if errs := theGraphErrors(result); !strings.Contains(errs, "sqrtPrice") {
		t.Fatalf("unknown Pool field errors = %q, want a message naming sqrtPrice", errs)
	}

	// Unknown where filter field is rejected at validation too.
	result, status = theGraphPost(t, base, `{ pools(first: 1, where: { unknownThing: "x" }) { id } }`, nil)
	if status != 400 {
		t.Fatalf("unknown where field -> %d, want 400", status)
	}

	// Unknown enum value for orderBy.
	result, status = theGraphPost(t, base, `{ pools(first: 1, orderBy: fakeField) { id } }`, nil)
	if status != 400 {
		t.Fatalf("unknown orderBy enum -> %d, want 400", status)
	}
}

// TestTheGraphStyleFirstLimit verifies the graph-node first cap surfaces as
// a real GraphQL error (the resolver fails the field, not the whole parse).
func TestTheGraphStyleFirstLimit(t *testing.T) {
	base, cleanup := setupTheGraph(t)
	defer cleanup()

	result, status := theGraphPost(t, base, `{ pools(first: 2000) { id } }`, nil)
	if status != 200 {
		t.Fatalf("first=2000 -> %d, want 200 (field error, not parse error)", status)
	}
	if errs := theGraphErrors(result); !strings.Contains(errs, "first parameter") {
		t.Fatalf("first=2000 errors = %q, want the first-parameter cap message", errs)
	}
	if result["data"] != nil {
		t.Fatalf("first=2000 data = %v, want null (non-null list field failed)", result["data"])
	}
}

// TestTheGraphStyleIntrospectionAndSDL verifies __schema introspection on
// the transport and the untouched REST SDL surface.
func TestTheGraphStyleIntrospectionAndSDL(t *testing.T) {
	base, cleanup := setupTheGraph(t)
	defer cleanup()

	result, status := theGraphPost(t, base, `{ __schema { queryType { name } types { name } } }`, nil)
	if status != 200 {
		t.Fatalf("introspection -> %d, want 200", status)
	}
	if errs := theGraphErrors(result); errs != "" {
		t.Fatalf("introspection errors: %s", errs)
	}
	schema := result["data"].(map[string]any)["__schema"].(map[string]any)
	if schema["queryType"].(map[string]any)["name"] != "Query" {
		t.Fatalf("queryType.name = %v, want Query", schema["queryType"])
	}
	typeNames := map[string]bool{}
	for _, ty := range schema["types"].([]any) {
		typeNames[ty.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"Pool", "Token", "Domain", "Account", "Pool_filter", "OrderDirection"} {
		if !typeNames[want] {
			t.Errorf("expected type %s in __schema types", want)
		}
	}

	// REST SDL surface is untouched: GET /subgraphs/id/{id}/graphql serves
	// the per-subgraph SDL string (the ENS deployment carries Domain).
	resp, err := http.Get(base + "/subgraphs/id/5XqPmWe6gZyrTtFjASCbxgykJ7KbAA8puFezV8vsJoEB/graphql")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("SDL -> %d, want 200", resp.StatusCode)
	}
	var sdlResp map[string]any
	json.Unmarshal(b, &sdlResp)
	sdl, _ := sdlResp["data"].(string)
	if !strings.Contains(sdl, "type Domain") {
		t.Fatalf("ENS SDL missing 'type Domain': %s", sdl)
	}
}
