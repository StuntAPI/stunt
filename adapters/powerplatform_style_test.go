package adapters

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// Drives the powerplatform-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: the Entra bearer gate, the OData
// {value} environment listing, the Dataverse accounts({accountid}) CRUD
// surface with $filter/$orderby/$select/$count, $top/$skipToken pagination
// whose @odata.nextLink round-trips the caller's OData options, per-
// environment flows with the seeded fallback, and the Microsoft
// {error:{code,message}} envelope.
const (
	pplatAuth          = "Bearer mock-entra-token"
	pplatHost          = "api.stunt.test"
	pplatEnvDefault    = "Default-d3a1d3a1-d3a1-d3a1-d3a1-d3a1d3a1d3a1"
	pplatEnvDev        = "Dev-e4b2e4b2-e4b2-e4b2-e4b2-e4b2e4b2e4b2"
	pplatAcctContoso   = "aaa11111-0000-0000-0000-000000000001"
	pplatAcctAdventure = "aaa11111-0000-0000-0000-000000000002"
)

type pplatFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newPplatFixture(t *testing.T, start time.Time) *pplatFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "powerplatform-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
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
	return &pplatFixture{t: t, vc: vc, host: pplatHost, vms: map[string]*starlark.VM{
		"envs": load("environments.star"), "conns": load("connectors.star"),
		"dv": load("dataverse.star"), "flows": load("flows.star"),
	}}
}

func (f *pplatFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// --- fixture drivers ---

// pplatAccounts builds the env-scoped Dataverse accounts collection route.
func pplatAccounts(env string) (string, map[string]string) {
	return "/v2/environments/" + env + "/api/data/v9.2/accounts", map[string]string{"env": env}
}

// pplatAccount builds the env-scoped accounts({accountid}) route — the
// Dataverse key-in-parens addressing convention.
func pplatAccount(env, id string) (string, map[string]string) {
	path, params := pplatAccounts(env)
	params["accountid"] = id
	return path + "(" + id + ")", params
}

// pplatSubRoute builds any other env-scoped route segment (flows, connectors).
func pplatSubRoute(env, seg string) (string, map[string]string) {
	return "/v2/environments/" + env + "/" + seg, map[string]string{"env": env}
}

// pplatListAccounts runs the account list with OData query options.
func (f *pplatFixture) pplatListAccounts(env string, query map[string]string) starlark.Response {
	f.t.Helper()
	path, params := pplatAccounts(env)
	return f.call("dv", "on_list_accounts", "GET", path, params, query, nil, pplatAuth)
}

// pplatCreateAccount POSTs an account and returns the response.
func (f *pplatFixture) pplatCreateAccount(env string, body map[string]any) starlark.Response {
	f.t.Helper()
	path, params := pplatAccounts(env)
	return f.call("dv", "on_create_account", "POST", path, params, nil, body, pplatAuth)
}

// --- assertion helpers ---

// pplatErr asserts the Microsoft error envelope — {error:{code, message}} —
// and the HTTP status.
func pplatErr(t *testing.T, r starlark.Response, wantStatus int, wantCode string) {
	t.Helper()
	if r.Status != wantStatus {
		t.Fatalf("%s: status -> %d, want %d; body %v", wantCode, r.Status, wantStatus, r.Body)
	}
	e, ok := r.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("%s: error = %v, want {error:{code,message}} object", wantCode, r.Body["error"])
	}
	if e["code"] != wantCode {
		t.Fatalf("error.code = %v, want %s (envelope %v)", e["code"], wantCode, e)
	}
	if msg, _ := e["message"].(string); msg == "" {
		t.Fatalf("%s: error.message is empty", wantCode)
	}
}

// pplatNum compares a JSON number regardless of int64/float64 width (docs
// round-trip through the collection store, where ints come back floats).
func pplatNum(t *testing.T, v any, want float64, what string) {
	t.Helper()
	switch n := v.(type) {
	case int64:
		if float64(n) != want {
			t.Fatalf("%s = %d, want %v", what, n, want)
		}
	case float64:
		if n != want {
			t.Fatalf("%s = %v, want %v", what, n, want)
		}
	default:
		t.Fatalf("%s = %T(%v), want number %v", what, v, v, want)
	}
}

// pplatValue returns the OData {value} array from a 200 response.
func pplatValue(t *testing.T, r starlark.Response) []any {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("list -> %d: %v", r.Status, r.Body)
	}
	v, ok := r.Body["value"].([]any)
	if !ok {
		t.Fatalf("value = %v, want array", r.Body["value"])
	}
	return v
}

// pplatByField indexes an OData {value} array by a string field.
func pplatByField(t *testing.T, r starlark.Response, field string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, e := range pplatValue(t, r) {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("value entry is %T, want object", e)
		}
		k, _ := m[field].(string)
		out[k] = m
	}
	return out
}

// pplatProps returns the nested properties object of an ARM/OData entry.
func pplatProps(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	p, ok := obj["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %v, want object", obj["properties"])
	}
	return p
}

// pplatHrefQuery parses an @odata.nextLink back into a query map, the way a
// client following the link would replay it.
func pplatHrefQuery(t *testing.T, href string) map[string]string {
	t.Helper()
	u, err := url.Parse(href)
	if err != nil {
		t.Fatalf("parse nextLink %q: %v", href, err)
	}
	q := map[string]string{}
	for k, vs := range u.Query() {
		if len(vs) > 0 {
			q[k] = vs[0]
		}
	}
	return q
}

// TestPowerPlatformAuthGateAndEnvironments: the Entra bearer gate (any
// well-formed bearer passes, nothing else does), the OData/ARM environment
// listing, and the two conventions this simulator does NOT model — the ARM
// api-version query param and any clock-driven environment lifecycle.
func TestPowerPlatformAuthGateAndEnvironments(t *testing.T) {
	f := newPplatFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== every route sits behind the entra bearer gate: a missing, non-bearer, or empty token answers 401 in the microsoft error envelope =====
	pplatErr(t, f.call("envs", "on_list_environments", "GET", "/v2/environments", nil, nil, nil, ""), 401, "Unauthorized")
	pplatErr(t, f.call("envs", "on_list_environments", "GET", "/v2/environments", nil, nil, nil, "Basic zzz"), 401, "Unauthorized")
	pplatErr(t, f.call("envs", "on_list_environments", "GET", "/v2/environments", nil, nil, nil, "Bearer "), 401, "Unauthorized")
	// The gate is wired in every script group, not just environments.
	acctPath, acctParams := pplatAccounts(pplatEnvDefault)
	pplatErr(t, f.call("dv", "on_list_accounts", "GET", acctPath, acctParams, nil, nil, ""), 401, "Unauthorized")
	pplatErr(t, f.call("dv", "on_create_account", "POST", acctPath, acctParams, nil, map[string]any{"name": "X"}, ""), 401, "Unauthorized")
	flowPath, flowParams := pplatSubRoute(pplatEnvDefault, "flows")
	pplatErr(t, f.call("flows", "on_list_flows", "GET", flowPath, flowParams, nil, nil, ""), 401, "Unauthorized")
	pplatErr(t, f.call("flows", "on_create_flow", "POST", flowPath, flowParams, nil, map[string]any{}, ""), 401, "Unauthorized")
	connPath, connParams := pplatSubRoute(pplatEnvDefault, "connectors")
	pplatErr(t, f.call("conns", "on_list_connectors", "GET", connPath, connParams, nil, nil, ""), 401, "Unauthorized")

	// ===== the gate does not validate the token: any well-formed bearer passes (as-is) =====
	if r := f.call("envs", "on_list_environments", "GET", "/v2/environments", nil, nil, nil, "Bearer not-an-entra-token"); r.Status != 200 {
		t.Fatalf("arbitrary bearer -> %d, want 200 (simulator never validates the token)", r.Status)
	}

	// ===== environments list as the odata value envelope with arm resource ids and nested properties =====
	byName := pplatByField(t, f.call("envs", "on_list_environments", "GET", "/v2/environments", nil, nil, nil, pplatAuth), "name")
	if len(byName) != 2 {
		t.Fatalf("environments = %d entries, want the 2 seeded", len(byName))
	}
	def := byName[pplatEnvDefault]
	if def == nil {
		t.Fatalf("default environment %s missing from list", pplatEnvDefault)
	}
	if def["id"] != "/providers/Microsoft.PowerPlatform/environments/"+pplatEnvDefault {
		t.Fatalf("default env id = %v", def["id"])
	}
	if def["location"] != "unitedstates" {
		t.Fatalf("default env location = %v, want unitedstates", def["location"])
	}
	defProps := pplatProps(t, def)
	if defProps["displayName"] != "Production" || defProps["environmentSku"] != "Production" || defProps["azureRegion"] != "westus" {
		t.Fatalf("default env properties = %v", defProps)
	}
	if defProps["state"] != "Ready" || defProps["isDefault"] != true {
		t.Fatalf("default env state = %v isDefault = %v, want Ready/true", defProps["state"], defProps["isDefault"])
	}
	dev := byName[pplatEnvDev]
	if dev == nil {
		t.Fatalf("dev environment %s missing from list", pplatEnvDev)
	}
	devProps := pplatProps(t, dev)
	if devProps["displayName"] != "Development" || devProps["environmentSku"] != "Sandbox" || devProps["state"] != "Ready" || devProps["isDefault"] != false {
		t.Fatalf("dev env properties = %v", devProps)
	}
	if dev["location"] != "europe" {
		t.Fatalf("dev env location = %v, want europe", dev["location"])
	}

	// ===== the api-version query convention is not modeled: the param is ignored (version is baked into the /v2 path — reported) =====
	av := f.call("envs", "on_list_environments", "GET", "/v2/environments", nil,
		map[string]string{"api-version": "2023-06-01"}, nil, pplatAuth)
	if av.Status != 200 || len(pplatValue(t, av)) != 2 {
		t.Fatalf("api-version=2023-06-01 -> %d, want 200 with the same 2 environments (param ignored)", av.Status)
	}

	// ===== environment lifecycle is not modeled: states are static and nothing moves on the clock (reported) =====
	f.vc.Advance(2 * time.Hour)
	after := pplatByField(t, f.call("envs", "on_list_environments", "GET", "/v2/environments", nil, nil, nil, pplatAuth), "name")
	if len(after) != 2 {
		t.Fatalf("after 2h on the clock environments = %d entries, want the same static pair", len(after))
	}
	if state := pplatProps(t, after[pplatEnvDefault])["state"]; state != "Ready" {
		t.Fatalf("default env state after 2h = %v, want Ready (no provisioning state machine)", state)
	}
}

// TestPowerPlatformDataverseAccountCRUD: the lazy account seeding, the
// accounts({accountid}) addressing convention across create/retrieve/patch/
// delete, and the Dataverse 0x80040217 not-found envelope.
func TestPowerPlatformDataverseAccountCRUD(t *testing.T) {
	f := newPplatFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== the collection lazily seeds two dataverse accounts on first read =====
	byID := pplatByField(t, f.pplatListAccounts(pplatEnvDefault, nil), "accountid")
	if len(byID) != 2 {
		t.Fatalf("seeded accounts = %d, want 2", len(byID))
	}
	contoso := byID[pplatAcctContoso]
	if contoso == nil || contoso["name"] != "Contoso Ltd." || contoso["emailaddress1"] != "info@contoso.com" ||
		contoso["telephone1"] != "+1-555-0100" || contoso["_primarycontactid_value"] != "bbb11111-0000-0000-0000-000000000001" {
		t.Fatalf("seeded contoso = %v", contoso)
	}
	pplatNum(t, contoso["revenue"], 5000000, "contoso revenue")
	pplatNum(t, contoso["statecode"], 0, "contoso statecode")
	if adventure := byID[pplatAcctAdventure]; adventure == nil || adventure["name"] != "Adventure Works" {
		t.Fatalf("seeded adventure = %v", byID[pplatAcctAdventure])
	}
	// The collection storage key never leaks into the API shape.
	for id, a := range byID {
		if _, has := a["id"]; has {
			t.Fatalf("account %s leaks the storage id field: %v", id, a)
		}
	}

	// ===== create answers 201 with a location entity uri and OData-Version header, echoing the record =====
	created := f.pplatCreateAccount(pplatEnvDefault, map[string]any{"name": "Globex", "revenue": 1000})
	if created.Status != 201 {
		t.Fatalf("create account -> %d: %v", created.Status, created.Body)
	}
	if created.Headers["OData-Version"] != "4.0" {
		t.Fatalf("create headers = %v, want OData-Version: 4.0", created.Headers)
	}
	if created.Headers["Location"] != "/v2/environments/"+pplatEnvDefault+"/api/data/v9.2/accounts(acc-1)" {
		t.Fatalf("create Location = %q", created.Headers["Location"])
	}
	if created.Body["accountid"] != "acc-1" || created.Body["name"] != "Globex" {
		t.Fatalf("created account = %v", created.Body)
	}
	pplatNum(t, created.Body["revenue"], 1000, "created revenue")
	if _, has := created.Body["id"]; has {
		t.Fatalf("created account leaks the storage id field: %v", created.Body)
	}

	// ===== the accounts({accountid}) key convention retrieves and 404s with the dataverse 0x80040217 envelope =====
	gotPath, gotParams := pplatAccount(pplatEnvDefault, "acc-1")
	got := f.call("dv", "on_retrieve_account", "GET", gotPath, gotParams, nil, nil, pplatAuth)
	if got.Status != 200 || got.Body["accountid"] != "acc-1" || got.Body["name"] != "Globex" {
		t.Fatalf("retrieve acc-1 -> %d %v", got.Status, got.Body)
	}
	nopePath, nopeParams := pplatAccount(pplatEnvDefault, "acct-nope")
	nope := f.call("dv", "on_retrieve_account", "GET", nopePath, nopeParams, nil, nil, pplatAuth)
	pplatErr(t, nope, 404, "0x80040217")
	if msg, _ := nope.Body["error"].(map[string]any)["message"].(string); msg != "account With Id = acct-nope Does Not Exist" {
		t.Fatalf("dataverse 404 message = %q", msg)
	}

	// ===== a client-supplied accountid is honored and targeted by later reads =====
	supplied := f.pplatCreateAccount(pplatEnvDefault, map[string]any{
		"accountid": "cccc2222-0000-0000-0000-000000000003", "name": "Tailspin",
	})
	if supplied.Status != 201 || supplied.Body["accountid"] != "cccc2222-0000-0000-0000-000000000003" {
		t.Fatalf("create with supplied id -> %d %v", supplied.Status, supplied.Body)
	}
	if supplied.Headers["Location"] != "/v2/environments/"+pplatEnvDefault+"/api/data/v9.2/accounts(cccc2222-0000-0000-0000-000000000003)" {
		t.Fatalf("supplied id Location = %q", supplied.Headers["Location"])
	}
	tailPath, tailParams := pplatAccount(pplatEnvDefault, "cccc2222-0000-0000-0000-000000000003")
	tail := f.call("dv", "on_retrieve_account", "GET", tailPath, tailParams, nil, nil, pplatAuth)
	if tail.Status != 200 || tail.Body["name"] != "Tailspin" {
		t.Fatalf("retrieve supplied id -> %d %v", tail.Status, tail.Body)
	}

	// ===== patch is a partial merge answering 204; delete answers 204 and a second delete 404s =====
	patchPath, patchParams := pplatAccount(pplatEnvDefault, pplatAcctContoso)
	patch := f.call("dv", "on_update_account", "PATCH", patchPath, patchParams, nil,
		map[string]any{"revenue": 1234, "telephone1": "+1-555-0300"}, pplatAuth)
	if patch.Status != 204 || patch.Body != nil {
		t.Fatalf("patch -> %d %v, want 204 with no body", patch.Status, patch.Body)
	}
	merged := f.call("dv", "on_retrieve_account", "GET", patchPath, patchParams, nil, nil, pplatAuth)
	pplatNum(t, merged.Body["revenue"], 1234, "patched revenue")
	if merged.Body["telephone1"] != "+1-555-0300" || merged.Body["name"] != "Contoso Ltd." || merged.Body["emailaddress1"] != "info@contoso.com" {
		t.Fatalf("patch was not a merge: %v", merged.Body)
	}
	del := f.call("dv", "on_delete_account", "DELETE", gotPath, gotParams, nil, nil, pplatAuth)
	if del.Status != 204 {
		t.Fatalf("delete -> %d, want 204", del.Status)
	}
	pplatErr(t, f.call("dv", "on_retrieve_account", "GET", gotPath, gotParams, nil, nil, pplatAuth), 404, "0x80040217")
	pplatErr(t, f.call("dv", "on_delete_account", "DELETE", gotPath, gotParams, nil, nil, pplatAuth), 404, "0x80040217")
}

// TestPowerPlatformDataverseODataOptions: the Dataverse query options on the
// account list — $filter (relational, functions, AND'ed, unquoted numerics),
// $orderby, $skip, $select projection, and $count=true's post-filter
// pre-paging total.
func TestPowerPlatformDataverseODataOptions(t *testing.T) {
	f := newPplatFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	for _, name := range []string{"OData Alpha", "OData Beta", "OData Gamma"} {
		if r := f.pplatCreateAccount(pplatEnvDefault, map[string]any{"name": name, "revenue": 7500000}); r.Status != 201 {
			t.Fatalf("seed helper create %s -> %d: %v", name, r.Status, r.Body)
		}
	}

	// ===== $filter supports eq/contains with AND and numeric comparisons on unquoted literals =====
	eq := pplatByField(t, f.pplatListAccounts(pplatEnvDefault, map[string]string{"$filter": "name eq 'Contoso Ltd.'"}), "name")
	if len(eq) != 1 || eq["Contoso Ltd."] == nil {
		t.Fatalf("name eq filter -> %d entries, want only Contoso Ltd.", len(eq))
	}
	contains := pplatByField(t, f.pplatListAccounts(pplatEnvDefault, map[string]string{"$filter": "contains(name,'Works')"}), "name")
	if len(contains) != 1 || contains["Adventure Works"] == nil {
		t.Fatalf("contains filter -> %d entries, want only Adventure Works", len(contains))
	}
	gt := pplatByField(t, f.pplatListAccounts(pplatEnvDefault, map[string]string{"$filter": "revenue gt 3000000"}), "name")
	if len(gt) != 4 {
		t.Fatalf("revenue gt 3000000 -> %d entries (%v), want the 3 created + Contoso", len(gt), gt)
	}
	startsWith := pplatByField(t, f.pplatListAccounts(pplatEnvDefault, map[string]string{"$filter": "startswith(name,'OData')"}), "name")
	if len(startsWith) != 3 {
		t.Fatalf("startswith filter -> %d entries, want the 3 created", len(startsWith))
	}
	and := pplatByField(t, f.pplatListAccounts(pplatEnvDefault,
		map[string]string{"$filter": "contains(name,'o') and revenue ge 2500000"}), "name")
	if len(and) != 2 {
		t.Fatalf("AND filter -> %d entries, want Contoso + Adventure (both contain 'o')", len(and))
	}
	none := pplatValue(t, f.pplatListAccounts(pplatEnvDefault, map[string]string{"$filter": "name eq 'Nobody'"}))
	if len(none) != 0 {
		t.Fatalf("no-match filter -> %d entries, want 0", len(none))
	}

	// ===== $orderby sorts asc and desc; $skip applies before paging =====
	desc := pplatValue(t, f.pplatListAccounts(pplatEnvDefault, map[string]string{"$orderby": "name desc"}))
	if first := desc[0].(map[string]any)["name"]; first != "OData Gamma" {
		t.Fatalf("$orderby name desc first = %v, want OData Gamma", first)
	}
	if last := desc[len(desc)-1].(map[string]any)["name"]; last != "Adventure Works" {
		t.Fatalf("$orderby name desc last = %v, want Adventure Works", last)
	}
	asc := pplatValue(t, f.pplatListAccounts(pplatEnvDefault, map[string]string{"$orderby": "name"}))
	if first := asc[0].(map[string]any)["name"]; first != "Adventure Works" {
		t.Fatalf("$orderby name first = %v, want Adventure Works", first)
	}
	skip := pplatValue(t, f.pplatListAccounts(pplatEnvDefault, map[string]string{"$skip": "1"}))
	if len(skip) != 4 || skip[0].(map[string]any)["name"] != "Adventure Works" {
		t.Fatalf("$skip=1 -> %d entries first %v, want 4 starting at Adventure Works", len(skip), skip[0])
	}

	// ===== $select projects fields and $count=true reports the post-filter pre-paging total =====
	sel := pplatValue(t, f.pplatListAccounts(pplatEnvDefault, map[string]string{"$select": "name,revenue"}))
	if len(sel) != 5 {
		t.Fatalf("$select list -> %d entries, want 5", len(sel))
	}
	for i, e := range sel {
		m := e.(map[string]any)
		if len(m) != 2 || m["name"] == nil || m["revenue"] == nil {
			t.Fatalf("$select entry %d = %v, want only {name, revenue}", i, m)
		}
	}
	counted := f.pplatListAccounts(pplatEnvDefault, map[string]string{
		"$count": "true", "$filter": "contains(name,'o')", "$top": "1",
	})
	if page := pplatValue(t, counted); len(page) != 1 {
		t.Fatalf("$count+$top page = %d entries, want 1", len(page))
	}
	pplatNum(t, counted.Body["@odata.count"], 2, "@odata.count")
	if ctx, _ := counted.Body["@odata.context"].(string); !strings.Contains(ctx, "$metadata#accounts") {
		t.Fatalf("@odata.context = %v, want the accounts metadata reference", counted.Body["@odata.context"])
	}
}

// TestPowerPlatformODataPagination: the $top/$skipToken cursor paging — a
// walk that covers every row exactly once, a nextLink that round-trips the
// caller's other OData options so a filtered walk stays filtered, and the
// disabled-without-$top / invalid-token edge cases.
func TestPowerPlatformODataPagination(t *testing.T) {
	f := newPplatFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	for _, name := range []string{"Page One", "Page Two", "Page Three"} {
		if r := f.pplatCreateAccount(pplatEnvDefault, map[string]any{"name": name}); r.Status != 201 {
			t.Fatalf("seed helper create %s -> %d: %v", name, r.Status, r.Body)
		}
	}

	// ===== $top/$skipToken walk every account exactly once with @odata.nextLink only between pages =====
	seen := map[string]int{}
	query := map[string]string{"$top": "2"}
	pages := 0
	for {
		r := f.pplatListAccounts(pplatEnvDefault, query)
		for _, e := range pplatValue(t, r) {
			id, _ := e.(map[string]any)["accountid"].(string)
			seen[id]++
		}
		pages++
		next, _ := r.Body["@odata.nextLink"].(string)
		if next == "" {
			break
		}
		query = pplatHrefQuery(t, next)
	}
	if len(seen) != 5 {
		t.Fatalf("walk covered %d distinct accounts, want all 5", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("account %s appeared %d times across pages, want exactly once", id, n)
		}
	}
	if pages != 3 {
		t.Fatalf("walk took %d pages, want 3 (2+2+1)", pages)
	}
	// The environments list pages with the same convention and a stable href.
	envPage := f.call("envs", "on_list_environments", "GET", "/v2/environments", nil,
		map[string]string{"$top": "1"}, nil, pplatAuth)
	if envNext := envPage.Body["@odata.nextLink"]; envNext != "/v2/environments?$top=1&$skipToken=1" {
		t.Fatalf("environments nextLink = %v, want the exact /v2/environments?$top=1&$skipToken=1", envNext)
	}
	envLast := f.call("envs", "on_list_environments", "GET", "/v2/environments", nil,
		pplatHrefQuery(t, "/v2/environments?$top=1&$skipToken=1"), nil, pplatAuth)
	envNames := pplatByField(t, envLast, "name")
	if len(envNames) != 1 || envNames[pplatEnvDev] == nil {
		t.Fatalf("environments page 2 = %v, want only the Dev environment", envNames)
	}
	if _, has := envLast.Body["@odata.nextLink"]; has {
		t.Fatal("last environments page still advertises a nextLink")
	}

	// ===== a nextLink round-trips the caller's $filter and $select so a paged walk stays filtered =====
	filtered := f.pplatListAccounts(pplatEnvDefault, map[string]string{
		"$filter": "contains(name,'Page')", "$select": "name", "$top": "2",
	})
	filteredNames := map[string]int{}
	for _, e := range pplatValue(t, filtered) {
		m := e.(map[string]any)
		if len(m) != 1 {
			t.Fatalf("filtered page entry = %v, want $select to survive into the page (only name)", m)
		}
		filteredNames[m["name"].(string)]++
	}
	nextLink, _ := filtered.Body["@odata.nextLink"].(string)
	if !strings.Contains(nextLink, "$filter=contains(name,'Page')") {
		t.Fatalf("nextLink %q drops $filter — following it would page the unfiltered list", nextLink)
	}
	if !strings.Contains(nextLink, "$select=name") {
		t.Fatalf("nextLink %q drops $select", nextLink)
	}
	last := f.pplatListAccounts(pplatEnvDefault, pplatHrefQuery(t, nextLink))
	for _, e := range pplatValue(t, last) {
		m := e.(map[string]any)
		if len(m) != 1 {
			t.Fatalf("followed page entry = %v, want the $select shape preserved", m)
		}
		filteredNames[m["name"].(string)]++
	}
	if len(filteredNames) != 3 {
		t.Fatalf("filtered walk covered %v, want exactly Page One/Two/Three", filteredNames)
	}
	for name, n := range filteredNames {
		if n != 1 {
			t.Fatalf("filtered account %s appeared %d times, want exactly once", name, n)
		}
	}
	if _, has := last.Body["@odata.nextLink"]; has {
		t.Fatal("filtered walk's last page still advertises a nextLink")
	}

	// ===== paging is disabled without $top and an invalid skipToken answers 400 badrequest =====
	whole := f.pplatListAccounts(pplatEnvDefault, nil)
	if n := len(pplatValue(t, whole)); n != 5 {
		t.Fatalf("no $top -> %d entries, want the whole list of 5", n)
	}
	if _, has := whole.Body["@odata.nextLink"]; has {
		t.Fatal("unpaged list advertises a nextLink")
	}
	pplatErr(t, f.pplatListAccounts(pplatEnvDefault, map[string]string{"$top": "1", "$skipToken": "abc"}), 400, "BadRequest")
}

// TestPowerPlatformFlowsAndConnectors: per-environment Power Automate flows
// with the seeded fallback, flow creation behind ARM flow ids, and the static
// connector catalog.
func TestPowerPlatformFlowsAndConnectors(t *testing.T) {
	f := newPplatFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	flowPath, flowParams := pplatSubRoute(pplatEnvDefault, "flows")

	// ===== an environment with no flows falls back to the seeded welcome flow =====
	fallback := pplatByField(t, f.call("flows", "on_list_flows", "GET", flowPath, flowParams, nil, nil, pplatAuth), "name")
	if len(fallback) != 1 || fallback["seeded-flow-001"] == nil {
		t.Fatalf("empty flows list = %d entries, want only the seeded fallback", len(fallback))
	}
	seeded := fallback["seeded-flow-001"]
	if seeded["id"] != "/providers/Microsoft.Flow/flows/seeded-flow-001" || seeded["type"] != "Microsoft.Flow/flows" {
		t.Fatalf("seeded flow = %v", seeded)
	}
	seededProps := pplatProps(t, seeded)
	if seededProps["displayName"] != "Welcome Email Flow" || seededProps["state"] != "Enabled" {
		t.Fatalf("seeded flow properties = %v", seededProps)
	}

	// ===== creating a flow persists it per environment behind arm flow ids; other environments keep the seeded fallback =====
	created := f.call("flows", "on_create_flow", "POST", flowPath, flowParams, nil,
		map[string]any{"properties": map[string]any{"displayName": "My New Flow"}}, pplatAuth)
	if created.Status != 201 {
		t.Fatalf("create flow -> %d: %v", created.Status, created.Body)
	}
	if created.Body["name"] != "flow-1" || created.Body["id"] != "/providers/Microsoft.Flow/flows/flow-1" || created.Body["type"] != "Microsoft.Flow/flows" {
		t.Fatalf("created flow = %v", created.Body)
	}
	createdProps := pplatProps(t, created.Body)
	if createdProps["displayName"] != "My New Flow" || createdProps["state"] != "Enabled" {
		t.Fatalf("created flow properties = %v", createdProps)
	}
	// A body without properties takes the documented "New Flow" default.
	defaulted := f.call("flows", "on_create_flow", "POST", flowPath, flowParams, nil, map[string]any{}, pplatAuth)
	if defaulted.Status != 201 || pplatProps(t, defaulted.Body)["displayName"] != "New Flow" {
		t.Fatalf("defaulted flow -> %d %v", defaulted.Status, defaulted.Body)
	}
	// The created flows replace the fallback in their own environment.
	byDisplay := pplatByField(t, f.call("flows", "on_list_flows", "GET", flowPath, flowParams, nil, nil, pplatAuth), "name")
	if len(byDisplay) != 2 || byDisplay["flow-1"] == nil || byDisplay["flow-2"] == nil {
		t.Fatalf("flows after creates = %d entries, want flow-1 and flow-2 (no fallback)", len(byDisplay))
	}
	// But another environment is untouched and keeps its fallback.
	devPath, devParams := pplatSubRoute(pplatEnvDev, "flows")
	devFlows := pplatByField(t, f.call("flows", "on_list_flows", "GET", devPath, devParams, nil, nil, pplatAuth), "name")
	if len(devFlows) != 1 || devFlows["seeded-flow-001"] == nil {
		t.Fatalf("dev environment flows = %d entries, want only the seeded fallback", len(devFlows))
	}

	// ===== connectors are static arm api resources with tiers =====
	connPath, connParams := pplatSubRoute(pplatEnvDefault, "connectors")
	conns := pplatByField(t, f.call("conns", "on_list_connectors", "GET", connPath, connParams, nil, nil, pplatAuth), "name")
	if len(conns) != 2 {
		t.Fatalf("connectors = %d entries, want the 2 seeded", len(conns))
	}
	sp := conns["shared_sharepointonline"]
	if sp == nil || sp["id"] != "/providers/Microsoft.PowerApps/apis/shared_sharepointonline" || sp["type"] != "Microsoft.PowerApps/apis" {
		t.Fatalf("sharepoint connector = %v", sp)
	}
	spProps := pplatProps(t, sp)
	if spProps["displayName"] != "SharePoint" || spProps["publisher"] != "Microsoft" || spProps["tier"] != "Standard" {
		t.Fatalf("sharepoint connector properties = %v", spProps)
	}
	if sql := conns["shared_sql"]; sql == nil || pplatProps(t, sql)["tier"] != "Premium" {
		t.Fatalf("sql connector = %v, want Premium tier", conns["shared_sql"])
	}
}
