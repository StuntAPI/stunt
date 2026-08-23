package adapters

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	sk "go.starlark.net/starlark"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/graphqlsim"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// Drives the thegraph-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: real GraphQL documents executed by
// the engine's GraphQL executor against the seeded subgraph (collection
// arguments, aliases, where-filter operators, relational joins, _meta), the
// spec-shaped errors[] surface for validation failures and the first cap,
// and the REST SDL endpoint's public/known-key/unknown-key auth triad.
const (
	tgDeploy    = "5zvR82QoaXYxfyKOCH8Qfl6p" // Uniswap V3-style deployment (the graphql path)
	tgEnsDeploy = "5XqPmWe6gZyrTtFjASCbxgykJ7KbAA8puFezV8vsJoEB"
	tgUSDC      = "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"
	tgWETH      = "0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2"
	tgUsdcWeth  = "0x88e6a0c2ddd26feeb64f039a2c41296fcb3f5640"
	tgWbtcWeth  = "0x11b815efb8f581194ae79006d24e0d814b7697f6"
)

type theGraphFixture struct {
	t      *testing.T
	vc     *clock.Clock
	vms    map[string]*starlark.VM
	schema *ast.Schema
	host   string
}

func newTheGraphFixture(t *testing.T, start time.Time) *theGraphFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "thegraph-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	sdl, err := os.ReadFile(filepath.Join(root, "schemas", "schema.graphql"))
	if err != nil {
		t.Fatalf("read schema.graphql: %v", err)
	}
	schema, err := graphqlsim.LoadSchema(sdl)
	if err != nil {
		t.Fatalf("load graphql schema: %v", err)
	}
	tmp := t.TempDir()
	store, _ := primitives.Open(filepath.Join(tmp, "s.db"))
	t.Cleanup(func() { store.Close() })
	kvStore, _ := kv.Open(filepath.Join(tmp, "s.kv.db"))
	t.Cleanup(func() { kvStore.Close() })
	blobStore, _ := blob.Open(filepath.Join(tmp, "blobs"))
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: events.NewEmitter(),
	})
	load := func(script string) *starlark.VM {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(root, "scripts", script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
		if err != nil {
			t.Fatalf("LoadWithLib %s: %v", script, err)
		}
		return vm
	}
	return &theGraphFixture{t: t, vc: vc, schema: schema, host: "api.thegraph.test", vms: map[string]*starlark.VM{
		"gql": load("resolvers.star"), "sdl": load("graphql.star"),
	}}
}

// --- GraphQL harness (mirrors the engine's convention-named dispatch) ---

// tgResolverSet maps on_<field> / resolve_<Type>_<field> onto the resolvers
// VM, exactly as the engine's graphql transport does.
type tgResolverSet struct{ f *theGraphFixture }

func (rs *tgResolverSet) Lookup(parentType, field string) (graphqlsim.Resolver, bool) {
	fn := "on_" + field
	if parentType != "Query" && parentType != "Mutation" {
		fn = "resolve_" + parentType + "_" + field
	}
	vm := rs.f.vms["gql"]
	if !vm.Has(fn) {
		return nil, false
	}
	return func(_ context.Context, parent map[string]any, args map[string]any) (any, error) {
		callArg, err := starlark.GoToStarlark(map[string]any{"parent": parent, "args": args})
		if err != nil {
			return nil, err
		}
		raw, err := vm.CallRaw(fn, callArg)
		if err != nil {
			return nil, err
		}
		return tgResultToGo(raw)
	}, true
}

// tgResultToGo unwraps a respond(...) dict like the engine does; a None body
// is a null GraphQL value.
func tgResultToGo(v sk.Value) (any, error) {
	if d, ok := v.(*sk.Dict); ok {
		if body, found, _ := d.Get(sk.String("body")); found {
			if _, none := body.(sk.NoneType); none {
				return nil, nil
			}
			return starlark.ValueToGo(body)
		}
	}
	if _, none := v.(sk.NoneType); none {
		return nil, nil
	}
	return starlark.ValueToGo(v)
}

// gql executes a document; callers decide whether an error is expected.
func (f *theGraphFixture) gql(query string, vars map[string]any) (*graphqlsim.Result, error) {
	return graphqlsim.Execute(context.Background(), f.schema, query, vars, "", &tgResolverSet{f}, graphqlsim.Options{})
}

// gqlOK executes and fatals on validation or execution errors.
func (f *theGraphFixture) gqlOK(query string, vars map[string]any) map[string]any {
	f.t.Helper()
	res, err := f.gql(query, vars)
	if err != nil {
		f.t.Fatalf("graphql: %v (query %s)", err, query)
	}
	if len(res.Errors) > 0 {
		f.t.Fatalf("graphql errors: %v (query %s)", res.Errors, query)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		f.t.Fatalf("data = %v (%T), want object (query %s)", res.Data, res.Data, query)
	}
	return data
}

// gqlErrs fatals when the document does NOT produce errors[] and returns
// the joined messages.
func (f *theGraphFixture) gqlErrs(query string, vars map[string]any) (*graphqlsim.Result, string) {
	f.t.Helper()
	res, err := f.gql(query, vars)
	if err != nil {
		f.t.Fatalf("graphql: %v (query %s)", err, query)
	}
	if len(res.Errors) == 0 {
		f.t.Fatalf("want errors[], got clean data %v (query %s)", res.Data, query)
	}
	msgs := make([]string, len(res.Errors))
	for i, e := range res.Errors {
		msgs[i] = e.Message
	}
	return res, strings.Join(msgs, "; ")
}

// tgObj fetches key as a non-nil object (test-local).
func tgObj(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %v (%T), want object", key, m[key], m[key])
	}
	return v
}

// tgNum reads a GraphQL Int whether the resolver produced a fresh Starlark
// int or a value that round-tripped the JSON document store.
func tgNum(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return -1
}

// tgPools runs a pools query and returns the rows.
func (f *theGraphFixture) tgPools(where string) []map[string]any {
	f.t.Helper()
	q := `{ pools(first: 10` + where + `) { id } }`
	data := f.gqlOK(q, nil)
	rows, ok := data["pools"].([]any)
	if !ok {
		f.t.Fatalf("pools = %v, want a list (query %s)", data["pools"], q)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.(map[string]any))
	}
	return out
}

// TestTheGraphSubgraphQueries: the executor-backed subgraph contract.
func TestTheGraphSubgraphQueries(t *testing.T) {
	f := newTheGraphFixture(t, time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC))

	// ===== pools collection arguments sort by volume and join token0/token1 =====
	data := f.gqlOK(`{
		top: pools(first: 2, orderBy: volumeUSD, orderDirection: desc) {
			id token0 { id symbol decimals } token1 { symbol } feeTier volumeUSD
		}
	}`, nil)
	top, ok := data["top"].([]any)
	if !ok || len(top) != 2 {
		t.Fatalf("pools(first: 2) = %v, want 2 rows", data["top"])
	}
	first := top[0].(map[string]any)
	if first["id"] != tgUsdcWeth {
		t.Fatalf("pools[0].id = %v, want the USDC/WETH pool (highest volumeUSD)", first["id"])
	}
	token0 := tgObj(t, first, "token0")
	if token0["symbol"] != "USDC" || token0["id"] != tgUSDC {
		t.Fatalf("token0 = %v, want the joined USDC entity", token0)
	}
	if got := tgNum(token0["decimals"]); got != 6 {
		t.Fatalf("token0.decimals = %v (%T), want Int 6 (not the stored \"6\" string)", token0["decimals"], token0["decimals"])
	}
	if tgObj(t, first, "token1")["symbol"] != "WETH" {
		t.Fatalf("token1.symbol = %v, want the joined WETH", first["token1"])
	}
	// BigInt/BigDecimal serialize as decimal strings (graph-node wire form).
	if first["feeTier"] != "500" || first["volumeUSD"] != "8912345678.901234" {
		t.Fatalf("pool scalars = %v/%v, want the decimal-string wire form", first["feeTier"], first["volumeUSD"])
	}

	// ===== where filters map the graph-node suffix operators =====
	// token0_not_in excludes the USDC pools; token0_in and _not keep their
	// complements; numeric suffixes compare numerically on decimal strings.
	notIn := f.tgPools(`, where: { token0_not_in: ["` + tgUSDC + `"] }`)
	if len(notIn) != 1 || notIn[0]["id"] != tgWbtcWeth {
		t.Fatalf("token0_not_in=[USDC] pools = %v, want exactly the WBTC pool", notIn)
	}
	in := f.tgPools(`, where: { token0_in: ["` + tgUSDC + `"] }`)
	if len(in) != 2 {
		t.Fatalf("token0_in=[USDC] pools = %v, want the two USDC pools", in)
	}
	notScalar := f.tgPools(`, where: { token0_not: "0x2260fac5e5542a773aa44fbcfedf7c193bc2b5f0" }`)
	if len(notScalar) != 2 {
		t.Fatalf("token0_not=WBTC pools = %v, want the two USDC pools", notScalar)
	}
	if got := len(f.tgPools(`, where: { txCount_gt: "700000" } `)); got != 2 {
		t.Fatalf("txCount_gt=700000 pools = %d, want 2 (1234567, 890123)", got)
	}
	// Variables resolve into where clauses like literals do.
	data = f.gqlOK(`query($sym: String) { tokens(first: 5, where: {symbol: $sym}) { id symbol } }`,
		map[string]any{"sym": "WETH"})
	weth, ok := data["tokens"].([]any)
	if !ok || len(weth) != 1 || weth[0].(map[string]any)["symbol"] != "WETH" {
		t.Fatalf("where {symbol: $sym} = %v, want the single WETH token", data["tokens"])
	}
	// skip paginates past the first page.
	data = f.gqlOK(`{ page2: tokens(first: 2, skip: 2) { symbol } }`, nil)
	if page2, ok := data["page2"].([]any); !ok || len(page2) != 2 {
		t.Fatalf("tokens(first: 2, skip: 2) = %v, want the remaining 2 tokens", data["page2"])
	}

	// ===== validation failures and the first cap surface as GraphQL errors =====
	// Unknown fields/filters are rejected before execution; the graph-node
	// first cap fails the field, nulling data through the non-null list.
	if _, err := f.gql(`{ pools(first: 1) { id sqrtPrice } }`, nil); err == nil || !strings.Contains(err.Error(), "sqrtPrice") {
		t.Fatalf("unknown Pool field error = %v, want it to name sqrtPrice", err)
	}
	if _, err := f.gql(`{ swaps(first: 5) { id } }`, nil); err == nil || !strings.Contains(err.Error(), "swaps") {
		t.Fatalf("unknown root field error = %v, want it to name swaps", err)
	}
	if _, err := f.gql(`{ pools(first: 1, where: { unknownThing: "x" }) { id } }`, nil); err == nil {
		t.Fatal("unknown where filter accepted; want a validation error")
	}
	if _, err := f.gql(`{ pools(first: 1, orderBy: fakeField) { id } }`, nil); err == nil {
		t.Fatal("unknown orderBy enum accepted; want a validation error")
	}
	res, msgs := f.gqlErrs(`{ pools(first: 2000) { id } }`, nil)
	if !strings.Contains(msgs, "first parameter cannot exceed 1000") {
		t.Fatalf("first=2000 errors = %q, want the graph-node cap message", msgs)
	}
	if res.Data != nil {
		t.Fatalf("first=2000 data = %v, want null ([Pool!]! failed and propagated)", res.Data)
	}

	// ===== domains join owner/resolvedAddress; lookups miss as null =====
	data = f.gqlOK(`{
		domains(first: 10, orderBy: createdAt, orderDirection: asc) {
			id name labelName owner { id } resolvedAddress { id } createdAt
		}
	}`, nil)
	domains, ok := data["domains"].([]any)
	if !ok || len(domains) != 3 {
		t.Fatalf("domains = %v, want the 3 seeded ENS-style domains", data["domains"])
	}
	d0 := domains[0].(map[string]any)
	if d0["name"] != "vitalik.eth" || d0["labelName"] != "vitalik" {
		t.Fatalf("domains[0] = %v, want vitalik.eth first (createdAt asc)", d0)
	}
	owner := tgObj(t, d0, "owner")
	if ownerID, _ := owner["id"].(string); len(ownerID) != 42 || !strings.HasPrefix(ownerID, "0x") {
		t.Fatalf("domain.owner.id = %v, want the joined 0x… account", owner["id"])
	}
	single := f.gqlOK(`query($id: ID!) { domain(id: $id) { name owner { id } } }`,
		map[string]any{"id": d0["id"]})
	if tgObj(t, single, "domain")["name"] != "vitalik.eth" {
		t.Fatalf("domain(id) = %v, want the same entity back", single["domain"])
	}
	miss := f.gqlOK(`query($id: ID!) { domain(id: $id) { name } }`, map[string]any{"id": "0xmissing"})
	if v, ok := miss["domain"]; !ok || v != nil {
		t.Fatalf("domain(0xmissing) = %v, want null without errors[]", miss["domain"])
	}

	// ===== _meta reports the deployment head; Token.pools joins in reverse =====
	data = f.gqlOK(`{
		_meta { deployment network block { number } hasIndexingErrors genesis { number } }
		weth: token(id: "`+tgWETH+`") { symbol pools { id } }
	}`, nil)
	meta := tgObj(t, data, "_meta")
	if meta["deployment"] != tgDeploy || meta["network"] != "mainnet" || meta["hasIndexingErrors"] != false {
		t.Fatalf("_meta = %v, want the seeded deployment on mainnet without indexing errors", meta)
	}
	if got := tgNum(tgObj(t, meta, "block")["number"]); got <= 0 {
		t.Fatalf("_meta.block.number = %v, want a positive head number", tgObj(t, meta, "block")["number"])
	}
	if got := tgNum(tgObj(t, meta, "genesis")["number"]); got != 1 {
		t.Fatalf("_meta.genesis.number = %v, want 1", got)
	}
	wethTok := tgObj(t, data, "weth")
	pools, ok := wethTok["pools"].([]any)
	if !ok || len(pools) != 2 {
		t.Fatalf("WETH pools reverse join = %v, want the 2 pools holding WETH as token1", wethTok["pools"])
	}

	// ===== the REST SDL surface is public and rejects unknown bearer keys =====
	// GET /subgraphs/id/{id}/graphql: anonymous is fine (hosted-service
	// semantics); a presented key must be the known one (gateway semantics).
	sdlGet := func(auth string) starlark.Response {
		t.Helper()
		headers := map[string]string{}
		if auth != "" {
			headers["Authorization"] = auth
		}
		r, err := f.vms["sdl"].Call("on_schema", starlark.Request{
			Method: "GET", Path: "/subgraphs/id/" + tgEnsDeploy + "/graphql", Host: f.host,
			Headers: headers, Params: map[string]string{"subgraphId": tgEnsDeploy}, Query: map[string]string{},
		})
		if err != nil {
			t.Fatalf("on_schema: %v", err)
		}
		return r
	}
	anon := sdlGet("")
	sdlText, _ := anon.Body["data"].(string)
	if anon.Status != 200 || !strings.Contains(sdlText, "type Domain") {
		t.Fatalf("anonymous SDL -> %d %q, want the ENS SDL string", anon.Status, sdlText)
	}
	if ct := anon.Headers["Content-Type"]; ct != "application/graphql" {
		t.Fatalf("SDL Content-Type = %q, want application/graphql", ct)
	}
	bad := sdlGet("Bearer not-a-known-key")
	if bad.Status != 401 {
		t.Fatalf("unknown bearer SDL -> %d, want 401", bad.Status)
	}
	if errs, ok := bad.Body["errors"].([]any); !ok || len(errs) != 1 {
		t.Fatalf("unknown bearer body = %v, want the GraphQL errors[] envelope", bad.Body)
	} else if msg := errs[0].(map[string]any)["message"]; msg != "valid API key expected" {
		t.Fatalf("unknown bearer error message = %v, want the gateway phrasing", msg)
	}
	if known := sdlGet("Bearer mock-graph-api-key"); known.Status != 200 {
		t.Fatalf("known bearer SDL -> %d, want 200 (the well-known test key)", known.Status)
	}
}
