package adapters

import (
	"encoding/base64"
	"os"
	"path/filepath"
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

// These tests drive the escrow-style adapter scripts directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock. They pin the contract
// points of the Escrow.com 2017-09-01 surface: the HTTP Basic gate against
// the documented synthetic credentials, transaction creation with the
// parties/items/schedule/fees shapes (amounts as decimal strings, the
// initiating party auto-agreed), the numeric-id and reference reads, page/
// per_page listing, and the PATCH action lifecycle — agree -> fund (the
// /sim affordance standing in for the hosted payment page) -> secured ->
// accept closing the transaction, plus ship/receive/cancel and the nested
// errors field envelopes.
//
// The lifecycle actually modelled has NO reject or return-item actions (the
// item status flags exist, but no PATCH action drives them) — nothing here
// asserts them.

const (
	escrowHost    = "api.escrow.test"
	escrowUser    = "escrow-test"
	escrowPass    = "escrow-test-api-key"
	escrowBuyer   = "buyer@sim.invalid"
	escrowSeller  = "seller@sim.invalid"
	escrowTxPath  = "/2017-09-01/transaction"
	escrowFeeRate = 3.25 // the general-merchandise escrow fee, percent
)

// escrowFixture is one shared store + virtual clock with a loaded VM per
// handler script (each script needs its own VM; they observe the same
// collections/kv state, exactly like the engine).
type escrowFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
	auth string
}

func newEscrowFixture(t *testing.T, start time.Time) *escrowFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "escrow-style")
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
	return &escrowFixture{t: t, vc: vc, host: escrowHost,
		auth: "Basic " + base64.StdEncoding.EncodeToString([]byte(escrowUser+":"+escrowPass)),
		vms: map[string]*starlark.VM{
			"tx": load("transactions.star"), "hooks": load("webhooks.star"),
		}}
}

// call invokes a handler on the named script VM. params carries the route
// captures ({id}, {reference}) the engine extracts from the path; a nil
// headers map gets the fixture's default Basic credentials, an explicit (even
// empty) map stands in for what the client sent.
func (f *escrowFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, headers map[string]string) starlark.Response {
	f.t.Helper()
	if headers == nil {
		headers = map[string]string{"Authorization": f.auth}
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s (%s): %v", method, path, handler, err)
	}
	return resp
}

// callRaw sends an undecodable raw body down the raw_body path body_of falls
// back to (what the engine does when the request is not JSON).
func (f *escrowFixture) callRaw(handler, method, path, raw string) starlark.Response {
	f.t.Helper()
	resp, err := f.vms["tx"].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host,
		Headers: map[string]string{"Authorization": f.auth}, RawBody: raw,
	})
	if err != nil {
		f.t.Fatalf("%s %s (%s): %v", method, path, handler, err)
	}
	return resp
}

// createTx posts the README's create shape: buyer + seller, one item with a
// single-entry payment schedule. Amounts are float64 because JSON numbers
// arrive as floats once the engine parses the body.
func (f *escrowFixture) createTx(desc string, amount float64, reference string) map[string]any {
	f.t.Helper()
	body := map[string]any{
		"description": desc,
		"parties": []any{
			map[string]any{"role": "buyer", "customer": escrowBuyer},
			map[string]any{"role": "seller", "customer": escrowSeller},
		},
		"items": []any{map[string]any{
			"title":       "Website",
			"description": "handover",
			"schedule": []any{map[string]any{
				"amount":               amount,
				"payer_customer":       escrowBuyer,
				"beneficiary_customer": escrowSeller,
			}},
		}},
	}
	if reference != "" {
		body["reference"] = reference
	}
	resp := f.call("tx", "on_transaction_create", "POST", escrowTxPath, nil, nil, body, nil)
	if resp.Status != 201 {
		f.t.Fatalf("create transaction -> %d: %v", resp.Status, resp.Body)
	}
	return resp.Body
}

// act PATCHes an action onto the transaction (the customer names the acting
// party; the real API infers it from the authenticated user).
func (f *escrowFixture) act(id, action, customer string) starlark.Response {
	f.t.Helper()
	body := map[string]any{"action": action}
	if customer != "" {
		body["customer"] = customer
	}
	return f.call("tx", "on_transaction_action", "PATCH", escrowTxPath+"/"+id, map[string]string{"id": id}, nil, body, nil)
}

// fund drives the /sim affordance that stands in for the buyer paying on the
// hosted page.
func (f *escrowFixture) fund(id string) starlark.Response {
	f.t.Helper()
	return f.call("tx", "on_sim_fund", "POST", "/sim/transaction/"+id+"/fund", map[string]string{"id": id}, nil, nil, nil)
}

// get reads the transaction back by its numeric id.
func (f *escrowFixture) get(id string) map[string]any {
	f.t.Helper()
	resp := f.call("tx", "on_transaction_get", "GET", escrowTxPath+"/"+id, map[string]string{"id": id}, nil, nil, nil)
	if resp.Status != 200 {
		f.t.Fatalf("get transaction %s -> %d: %v", id, resp.Status, resp.Body)
	}
	return resp.Body
}

// --- nested-body accessors (responses are map[string]any / []any trees) ---

func escMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s = %v (%T), want object", what, v, v)
	}
	return m
}

func escList(t *testing.T, v any, what string) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("%s = %v (%T), want array", what, v, v)
	}
	return l
}

// escInt reads a number that renders as int at create time but float after a
// store round trip (the store persists JSON); the wire output is identical.
func escInt(t *testing.T, v any, what string) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	t.Fatalf("%s = %v (%T), want number", what, v, v)
	return 0
}

// escError digs errors.<field>[0] out of an error envelope.
func escError(t *testing.T, r starlark.Response, field string) string {
	t.Helper()
	fields := escMap(t, r.Body["errors"], "errors")
	return escList(t, fields[field], "errors."+field)[0].(string)
}

// escItem digs items[i] out of a presented transaction.
func escItem(t *testing.T, tx map[string]any, i int) map[string]any {
	t.Helper()
	return escMap(t, escList(t, tx["items"], "items")[i], "items[i]")
}

// escSchedule digs items[i].schedule[j] out of a presented transaction.
func escSchedule(t *testing.T, tx map[string]any, i, j int) map[string]any {
	t.Helper()
	item := escItem(t, tx, i)
	return escMap(t, escList(t, item["schedule"], "schedule")[j], "schedule[j]")
}

// escStatus digs items[i].status — where the item lifecycle flags live (the
// transaction itself carries no top-level status).
func escStatus(t *testing.T, tx map[string]any, i int) map[string]any {
	t.Helper()
	return escMap(t, escItem(t, tx, i)["status"], "items[i].status")
}

// escFee digs items[i].fees[j].
func escFee(t *testing.T, tx map[string]any, i, j int) map[string]any {
	t.Helper()
	item := escItem(t, tx, i)
	return escMap(t, escList(t, item["fees"], "fees")[j], "fees[j]")
}

// TestEscrowBasicAuthGate: every route sits behind HTTP Basic auth against
// the documented synthetic credentials; failures answer the nested errors
// envelope plus the WWW-Authenticate challenge.
func TestEscrowBasicAuthGate(t *testing.T) {
	base := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	f := newEscrowFixture(t, base)

	// ===== missing, Bearer, and malformed Authorization are 401s with the challenge header =====
	for name, hdr := range map[string]string{
		"no credentials":    "",
		"bearer scheme":     "Bearer some-token",
		"malformed base64":  "Basic !!!not-base64!!!",
		"colonless payload": "Basic " + base64.StdEncoding.EncodeToString([]byte("secretagent")),
		"wrong password":    "Basic " + base64.StdEncoding.EncodeToString([]byte(escrowUser+":wrong")),
		"unknown user":      "Basic " + base64.StdEncoding.EncodeToString([]byte("nobody:"+escrowPass)),
	} {
		r := f.call("tx", "on_customer_me", "GET", "/2017-09-01/customer/me", nil, nil, nil, map[string]string{"Authorization": hdr})
		if r.Status != 401 {
			t.Fatalf("%s -> %d, want 401", name, r.Status)
		}
		if escError(t, r, "auth") != "Unauthorized" {
			t.Fatalf("%s errors.auth = %v", name, r.Body)
		}
		if r.Headers["WWW-Authenticate"] != `Basic realm="escrow"` {
			t.Fatalf("%s WWW-Authenticate = %q, want the Basic challenge", name, r.Headers["WWW-Authenticate"])
		}
	}

	// ===== the documented credentials unlock the customer record =====
	me := f.call("tx", "on_customer_me", "GET", "/2017-09-01/customer/me", nil, nil, nil, nil)
	if me.Status != 200 {
		t.Fatalf("customer/me -> %d: %v", me.Status, me.Body)
	}
	if me.Body["email"] != "me@sim.invalid" || me.Body["first_name"] != "Sandbox" || me.Body["last_name"] != "Customer" {
		t.Fatalf("customer/me = %v", me.Body)
	}
	if escInt(t, me.Body["id"], "customer id") != 1 {
		t.Fatalf("customer id = %v, want 1", me.Body["id"])
	}

	// ===== the gate covers the transaction and webhook surfaces alike =====
	for name, tc := range map[string]struct {
		group, handler, method, path string
	}{
		"list transactions":  {"tx", "on_transaction_list", "GET", escrowTxPath},
		"create transaction": {"tx", "on_transaction_create", "POST", escrowTxPath},
		"list webhooks":      {"hooks", "on_webhook_list", "GET", "/2017-09-01/customer/me/webhook"},
	} {
		r := f.call(tc.group, tc.handler, tc.method, tc.path, nil, nil, nil, map[string]string{})
		if r.Status != 401 || escError(t, r, "auth") != "Unauthorized" {
			t.Fatalf("%s without credentials -> %d %v, want 401", name, r.Status, r.Body)
		}
	}
}

// TestEscrowTransactionCreateGet: creation answers 201 with the 2017-09-01
// transaction shape — numeric id, parties with the initiator auto-agreed,
// items with schedule amounts as decimal strings and the default 3.25% escrow
// fee on the buyer — and the read by id returns the same shape (amounts must
// survive the store round trip). Unknown ids answer the id-keyed 404.
func TestEscrowTransactionCreateGet(t *testing.T) {
	base := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	f := newEscrowFixture(t, base)

	// ===== create assigns the first numeric id, stamps creation_date, and defaults currency =====
	tx := f.createTx("Domain handover", 1000.0, "VM-REF-1")
	if id, ok := tx["id"].(int64); !ok || id != 1 {
		t.Fatalf("transaction id = %v (%T), want int64 1", tx["id"], tx["id"])
	}
	if tx["currency"] != "usd" {
		t.Fatalf("currency = %v, want the usd default", tx["currency"])
	}
	if tx["description"] != "Domain handover" || tx["reference"] != "VM-REF-1" {
		t.Fatalf("description/reference = %v / %v, want round-tripped", tx["description"], tx["reference"])
	}
	if tx["creation_date"] != base.Format(time.RFC3339) {
		t.Fatalf("creation_date = %v, want the virtual clock's %s", tx["creation_date"], base.Format(time.RFC3339))
	}
	if tx["close_date"] != nil || tx["is_cancelled"] != false {
		t.Fatalf("fresh transaction close_date/is_cancelled = %v / %v, want nil / false", tx["close_date"], tx["is_cancelled"])
	}
	// No top-level status and no internal keys leak: funding state lives only
	// on items[].schedule[].status.secured.
	if _, has := tx["status"]; has {
		t.Fatalf("transaction carries a top-level status: %v", tx["status"])
	}
	for _, leak := range []string{"ref_id", "_id"} {
		if _, has := tx[leak]; has {
			t.Fatalf("internal key %q leaked into the response", leak)
		}
	}

	// ===== the initiating party is auto-agreed; the counterparty is not =====
	parties := escList(t, tx["parties"], "parties")
	if len(parties) != 2 {
		t.Fatalf("parties = %d entries, want 2", len(parties))
	}
	buyer := escMap(t, parties[0], "parties[0]")
	if buyer["role"] != "buyer" || buyer["customer"] != escrowBuyer || buyer["agreed"] != true {
		t.Fatalf("initiating buyer = %v, want auto-agreed (the creator-agrees rule)", buyer)
	}
	seller := escMap(t, parties[1], "parties[1]")
	if seller["role"] != "seller" || seller["customer"] != escrowSeller || seller["agreed"] != false {
		t.Fatalf("counterparty seller = %v, want not yet agreed", seller)
	}

	// ===== items carry the 2017-09-01 defaults and schedule amounts as decimal strings =====
	item := escItem(t, tx, 0)
	if item["title"] != "Website" || item["type"] != "general_merchandise" {
		t.Fatalf("item = %v, want title round-tripped + the general_merchandise default", item)
	}
	if escInt(t, item["inspection_period"], "inspection_period") != 259200 || escInt(t, item["quantity"], "quantity") != 1 {
		t.Fatalf("inspection_period/quantity = %v / %v, want 259200 (3 days) / 1", item["inspection_period"], item["quantity"])
	}
	sched := escSchedule(t, tx, 0, 0)
	if sched["amount"] != "1000.00" {
		t.Fatalf("schedule amount = %v (%T), want the decimal string \"1000.00\"", sched["amount"], sched["amount"])
	}
	if sched["type"] != "deposit" || sched["payer_customer"] != escrowBuyer || sched["beneficiary_customer"] != escrowSeller {
		t.Fatalf("schedule entry = %v, want deposit type + payer/beneficiary echoed", sched)
	}
	if escMap(t, sched["status"], "schedule.status")["secured"] != false {
		t.Fatalf("fresh schedule secured = %v, want false until funded", sched["status"])
	}
	st := escStatus(t, tx, 0)
	for _, flag := range []string{"accepted", "received", "shipped", "rejected", "canceled", "in_dispute"} {
		if st[flag] != false {
			t.Fatalf("fresh item status.%s = %v, want false", flag, st[flag])
		}
	}

	// ===== the default escrow fee is 3.25% charged to the buyer =====
	fee := escFee(t, tx, 0, 0)
	if fee["type"] != "escrow" || fee["amount"] != "32.50" || fee["payer_customer"] != escrowBuyer {
		t.Fatalf("default fee = %v, want escrow 32.50 (3.25%% of 1000.00) on the buyer", fee)
	}

	// ===== get by id returns the same shape — amounts survive the store round trip =====
	got := f.get("1")
	if id, ok := got["id"].(int64); !ok || id != 1 {
		t.Fatalf("read-back id = %v (%T), want int64 1", got["id"], got["id"])
	}
	if s := escSchedule(t, got, 0, 0); s["amount"] != "1000.00" {
		t.Fatalf("read-back schedule amount = %v (%T), want \"1000.00\"", s["amount"], s["amount"])
	}
	if fee := escFee(t, got, 0, 0); fee["amount"] != "32.50" {
		t.Fatalf("read-back fee amount = %v, want \"32.50\"", fee["amount"])
	}
	if escMap(t, escSchedule(t, got, 0, 0)["status"], "schedule.status")["secured"] != false {
		t.Fatalf("read-back secured = %v, want still false", escSchedule(t, got, 0, 0)["status"])
	}

	// ===== unknown ids are 404s keyed on the id field =====
	missing := f.call("tx", "on_transaction_get", "GET", escrowTxPath+"/999", map[string]string{"id": "999"}, nil, nil, nil)
	if missing.Status != 404 || escError(t, missing, "id") != "Transaction not found" {
		t.Fatalf("get unknown transaction -> %d %v, want 404 errors.id", missing.Status, missing.Body)
	}
}

// TestEscrowCreateValidation: party validation uses Escrow's nested field
// errors; amounts parse from floats and decimal strings alike; a caller fee
// split is honored in the schedule's units; junk bodies are 400s.
func TestEscrowCreateValidation(t *testing.T) {
	base := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	f := newEscrowFixture(t, base)
	buyer := map[string]any{"role": "buyer", "customer": escrowBuyer}
	seller := map[string]any{"role": "seller", "customer": escrowSeller}

	post := func(parties []any, items []any) starlark.Response {
		return f.call("tx", "on_transaction_create", "POST", escrowTxPath, nil, nil,
			map[string]any{"parties": parties, "items": items}, nil)
	}

	// ===== party validation answers the nested Escrow field errors =====
	one := post([]any{buyer}, nil)
	if one.Status != 400 || escError(t, one, "parties") != "Transaction must have a buyer and a seller" {
		t.Fatalf("single party -> %d %v, want 400 errors.parties list form", one.Status, one.Body)
	}
	twoBuyers := post([]any{buyer, map[string]any{"role": "buyer", "customer": "b2@sim.invalid"}}, nil)
	if twoBuyers.Status != 400 {
		t.Fatalf("two buyers -> %d, want 400", twoBuyers.Status)
	}
	fields := escMap(t, escMap(t, twoBuyers.Body["errors"], "errors")["parties"], "errors.parties")
	if got := escList(t, fields["0"], "errors.parties.0")[0].(string); got != "Transaction must have 1 seller" {
		t.Fatalf("two buyers errors.parties.0 = %v, want the indexed form", got)
	}
	twoSellers := post([]any{seller, map[string]any{"role": "seller", "customer": "s2@sim.invalid"}}, nil)
	if twoSellers.Status != 400 {
		t.Fatalf("two sellers -> %d, want 400", twoSellers.Status)
	}
	fields = escMap(t, escMap(t, twoSellers.Body["errors"], "errors")["parties"], "errors.parties")
	if got := escList(t, fields["0"], "errors.parties.0")[0].(string); got != "Transaction must have 1 buyer" {
		t.Fatalf("two sellers errors.parties.0 = %v, want the indexed form", got)
	}

	item := func(amount any) []any {
		return []any{map[string]any{"schedule": []any{map[string]any{
			"amount": amount, "payer_customer": escrowBuyer, "beneficiary_customer": escrowSeller,
		}}}}
	}

	// ===== amounts parse from decimal strings identically to numbers =====
	strTx := post([]any{buyer, seller}, item("1000.50"))
	if strTx.Status != 201 {
		t.Fatalf("create with string amount -> %d: %v", strTx.Status, strTx.Body)
	}
	if s := escSchedule(t, strTx.Body, 0, 0); s["amount"] != "1000.50" {
		t.Fatalf("string amount schedule = %v, want \"1000.50\"", s["amount"])
	}

	// ===== multi-entry schedules sum into the default fee =====
	multi := f.call("tx", "on_transaction_create", "POST", escrowTxPath, nil, nil, map[string]any{
		"parties": []any{buyer, seller},
		"items": []any{map[string]any{"schedule": []any{
			map[string]any{"amount": 500.0, "payer_customer": escrowBuyer, "beneficiary_customer": escrowSeller},
			map[string]any{"amount": "500.00", "payer_customer": escrowBuyer, "beneficiary_customer": escrowSeller},
		}}},
	}, nil)
	if multi.Status != 201 {
		t.Fatalf("create with two schedule entries -> %d: %v", multi.Status, multi.Body)
	}
	if s := escSchedule(t, multi.Body, 0, 1); s["amount"] != "500.00" {
		t.Fatalf("second schedule entry = %v, want \"500.00\"", s["amount"])
	}
	if fee := escFee(t, multi.Body, 0, 0); fee["amount"] != "32.50" {
		t.Fatalf("fee on two 500.00 entries = %v, want 32.50 (3.25%% of the 1000.00 total)", fee["amount"])
	}

	// ===== a caller-supplied fee split is honored, in the schedule's dollar units =====
	split := f.call("tx", "on_transaction_create", "POST", escrowTxPath, nil, nil, map[string]any{
		"parties": []any{buyer, seller},
		"items": []any{map[string]any{
			"schedule": item(1000.0)[0].(map[string]any)["schedule"],
			"fees":     []any{map[string]any{"type": "escrow", "amount": 10.0, "payer_customer": escrowSeller}},
		}},
	}, nil)
	if split.Status != 201 {
		t.Fatalf("create with fee split -> %d: %v", split.Status, split.Body)
	}
	if fee := escFee(t, split.Body, 0, 0); fee["amount"] != "10.00" || fee["payer_customer"] != escrowSeller {
		t.Fatalf("caller fee = %v, want 10.00 charged to the seller", fee)
	}

	// ===== an unparseable amount stores 0.00 rather than 400ing (deviation, asserted as-is) =====
	// Real Escrow rejects a non-numeric amount with a field error; the sim
	// silently floors it to zero.
	junk := post([]any{buyer, seller}, item("not-a-number"))
	if junk.Status != 201 || escSchedule(t, junk.Body, 0, 0)["amount"] != "0.00" {
		t.Fatalf("unparseable amount -> %d %v, want 201 with \"0.00\" (documented gap)", junk.Status, junk.Body)
	}

	// ===== a non-JSON body is a 400 body error, not a 500 =====
	bad := f.callRaw("on_transaction_create", "POST", escrowTxPath, "{not json")
	if bad.Status != 400 || escError(t, bad, "body") != "Request body is not valid JSON" {
		t.Fatalf("non-JSON body -> %d %v, want 400 errors.body", bad.Status, bad.Body)
	}
}

// TestEscrowTransactionListPagination: the list endpoint walks page/per_page
// over every transaction in insertion order; junk params fall back to the
// defaults.
func TestEscrowTransactionListPagination(t *testing.T) {
	base := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	f := newEscrowFixture(t, base)
	for i, desc := range []string{"first", "second", "third"} {
		if id := escInt(t, f.createTx(desc, 100.0*float64(i+1), "")["id"], "id"); id != int64(i+1) {
			t.Fatalf("create %d assigned id %d", i+1, id)
		}
	}
	list := func(query map[string]string) []int64 {
		t.Helper()
		r := f.call("tx", "on_transaction_list", "GET", escrowTxPath, nil, query, nil, nil)
		if r.Status != 200 {
			t.Fatalf("list %v -> %d: %v", query, r.Status, r.Body)
		}
		out := []int64{}
		for _, raw := range escList(t, r.Body["transactions"], "transactions") {
			out = append(out, escInt(t, escMap(t, raw, "transaction")["id"], "id"))
		}
		return out
	}

	// ===== the default page returns every transaction in creation order =====
	if got := list(nil); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("default list = %v, want [1 2 3]", got)
	}

	// ===== per_page slices; page walks the remainder; past the end is empty =====
	if got := list(map[string]string{"per_page": "2"}); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("per_page=2 -> %v, want [1 2]", got)
	}
	if got := list(map[string]string{"page": "2", "per_page": "2"}); len(got) != 1 || got[0] != 3 {
		t.Fatalf("page=2 per_page=2 -> %v, want [3]", got)
	}
	if got := list(map[string]string{"page": "3", "per_page": "2"}); len(got) != 0 {
		t.Fatalf("page past the end -> %v, want empty", got)
	}

	// ===== junk pagination params fall back to the defaults (page 1, per_page 10) =====
	if got := list(map[string]string{"page": "abc", "per_page": "abc"}); len(got) != 3 {
		t.Fatalf("junk params -> %v, want the full default page", got)
	}
}

// TestEscrowReferenceLookup: the literal reference route resolves a
// transaction by your own reference; unknown references 404 on the field.
func TestEscrowReferenceLookup(t *testing.T) {
	base := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	f := newEscrowFixture(t, base)
	byRef := func(ref string) starlark.Response {
		return f.call("tx", "on_transaction_by_reference", "GET", escrowTxPath+"/reference/"+ref,
			map[string]string{"reference": ref}, nil, nil, nil)
	}

	// ===== a reference resolves to its transaction =====
	f.createTx("first deal", 100.0, "MY-DEAL-1")
	f.createTx("unreferenced", 200.0, "")
	f.createTx("second deal", 300.0, "MY-DEAL-2")

	found := byRef("MY-DEAL-1")
	if found.Status != 200 || escInt(t, found.Body["id"], "id") != 1 || found.Body["description"] != "first deal" {
		t.Fatalf("reference lookup -> %d %v, want transaction 1", found.Status, found.Body)
	}
	other := byRef("MY-DEAL-2")
	if other.Status != 200 || escInt(t, other.Body["id"], "id") != 3 {
		t.Fatalf("second reference lookup -> %d %v, want transaction 3", other.Status, other.Body)
	}

	// ===== an unknown reference is a 404 keyed on the reference field =====
	missing := byRef("NO-SUCH-REFERENCE")
	if missing.Status != 404 || escError(t, missing, "reference") != "Transaction not found" {
		t.Fatalf("unknown reference -> %d %v, want 404 errors.reference", missing.Status, missing.Body)
	}
}

// TestEscrowAgreeFundAcceptLifecycle: agree records each party's agreement
// ( strangers and blanks are 400s ), the /sim fund affordance waits for full
// agreement before securing every schedule entry, and accept is gated on
// secured funds — closing the transaction with close_date off the clock.
func TestEscrowAgreeFundAcceptLifecycle(t *testing.T) {
	base := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	f := newEscrowFixture(t, base)
	f.createTx("Race bike", 2500.0, "")

	// ===== agree needs a party's email and rejects strangers =====
	blank := f.act("1", "agree", "")
	if blank.Status != 400 || escError(t, blank, "customer") != "Customer can't be blank" {
		t.Fatalf("agree without customer -> %d %v, want 400 errors.customer blank", blank.Status, blank.Body)
	}
	stranger := f.act("1", "agree", "stranger@sim.invalid")
	if stranger.Status != 400 || escError(t, stranger, "customer") != "Customer is not a party on this transaction" {
		t.Fatalf("agree by a stranger -> %d %v, want 400 errors.customer not-a-party", stranger.Status, stranger.Body)
	}

	// ===== funding waits for every party's agreement =====
	early := f.fund("1")
	if early.Status != 400 || escError(t, early, "parties") != "All parties must agree before funding" {
		t.Fatalf("fund before full agreement -> %d %v, want 400 errors.parties", early.Status, early.Body)
	}
	acceptEarly := f.act("1", "accept", escrowBuyer)
	if acceptEarly.Status != 400 || escError(t, acceptEarly, "transaction") != "Transaction is not secured" {
		t.Fatalf("accept before funding -> %d %v, want 400 errors.transaction not secured", acceptEarly.Status, acceptEarly.Body)
	}

	// ===== the counterparty's agreement unlocks funding; every schedule entry secures =====
	agreed := f.act("1", "agree", escrowSeller)
	if agreed.Status != 200 {
		t.Fatalf("seller agree -> %d: %v", agreed.Status, agreed.Body)
	}
	for _, p := range escList(t, agreed.Body["parties"], "parties") {
		if escMap(t, p, "party")["agreed"] != true {
			t.Fatalf("after seller agree, parties = %v, want all agreed", agreed.Body["parties"])
		}
	}
	funded := f.fund("1")
	if funded.Status != 200 {
		t.Fatalf("fund -> %d: %v", funded.Status, funded.Body)
	}
	after := f.get("1")
	if escMap(t, escSchedule(t, after, 0, 0)["status"], "schedule.status")["secured"] != true {
		t.Fatalf("funded schedule = %v, want secured", escSchedule(t, after, 0, 0)["status"])
	}

	// ===== accept flips every item and stamps close_date off the virtual clock =====
	f.vc.Advance(time.Hour)
	accepted := f.act("1", "accept", escrowBuyer)
	if accepted.Status != 200 {
		t.Fatalf("accept -> %d: %v", accepted.Status, accepted.Body)
	}
	if escStatus(t, accepted.Body, 0)["accepted"] != true {
		t.Fatalf("accepted item status = %v", escStatus(t, accepted.Body, 0))
	}
	if accepted.Body["close_date"] != base.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("close_date = %v, want creation + 1h off the virtual clock", accepted.Body["close_date"])
	}
	if accepted.Body["is_cancelled"] != false {
		t.Fatalf("accepted transaction is_cancelled = %v, want false", accepted.Body["is_cancelled"])
	}

	// ===== PATCH actions and funding on unknown ids are 404s =====
	ghostAct := f.act("999", "agree", escrowSeller)
	if ghostAct.Status != 404 || escError(t, ghostAct, "id") != "Transaction not found" {
		t.Fatalf("PATCH unknown transaction -> %d %v, want 404 errors.id", ghostAct.Status, ghostAct.Body)
	}
	ghostFund := f.fund("999")
	if ghostFund.Status != 404 || escError(t, ghostFund, "id") != "Transaction not found" {
		t.Fatalf("fund unknown transaction -> %d %v, want 404 errors.id", ghostFund.Status, ghostFund.Body)
	}
}

// TestEscrowShipReceiveCancel: ship and receive flip the per-item lifecycle
// flags, cancel flags every item and the transaction itself, and unknown
// actions (or none at all) are 400s on the action field. None of these are
// gated on agreement or funding (deviation, asserted as-is).
func TestEscrowShipReceiveCancel(t *testing.T) {
	base := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	f := newEscrowFixture(t, base)
	f.createTx("Vintage watch", 750.0, "")

	// ===== ship marks the item shipped, receive marks it received =====
	shipped := f.act("1", "ship", escrowSeller)
	if shipped.Status != 200 {
		t.Fatalf("ship -> %d: %v", shipped.Status, shipped.Body)
	}
	st := escStatus(t, shipped.Body, 0)
	if st["shipped"] != true || st["received"] != false {
		t.Fatalf("after ship, item status = %v, want shipped only", st)
	}
	received := f.act("1", "receive", escrowBuyer)
	if received.Status != 200 {
		t.Fatalf("receive -> %d: %v", received.Status, received.Body)
	}
	st = escStatus(t, f.get("1"), 0)
	if st["received"] != true || st["shipped"] != true {
		t.Fatalf("after receive, item status = %v, want shipped + received", st)
	}

	// ===== cancel flags every item and the transaction itself, without closing it =====
	cancelled := f.act("1", "cancel", escrowBuyer)
	if cancelled.Status != 200 {
		t.Fatalf("cancel -> %d: %v", cancelled.Status, cancelled.Body)
	}
	if escStatus(t, cancelled.Body, 0)["canceled"] != true {
		t.Fatalf("after cancel, item status = %v, want canceled", escStatus(t, cancelled.Body, 0))
	}
	if cancelled.Body["is_cancelled"] != true || cancelled.Body["close_date"] != nil {
		t.Fatalf("cancelled transaction = %v / %v, want is_cancelled true and no close_date",
			cancelled.Body["is_cancelled"], cancelled.Body["close_date"])
	}

	// ===== unknown or missing actions are 400s on the action field =====
	unknown := f.act("1", "frobnicate", escrowBuyer)
	if unknown.Status != 400 || escError(t, unknown, "action") != "Unknown action: frobnicate" {
		t.Fatalf("unknown action -> %d %v, want 400 errors.action", unknown.Status, unknown.Body)
	}
	none := f.call("tx", "on_transaction_action", "PATCH", escrowTxPath+"/1", map[string]string{"id": "1"}, nil, nil, nil)
	if none.Status != 400 || escError(t, none, "action") != "Unknown action: " {
		t.Fatalf("actionless PATCH -> %d %v, want 400 errors.action", none.Status, none.Body)
	}
}

// TestEscrowWebhookRegistration: webhook URLs register and round-trip through
// the list; a blank URL is Escrow's can't-be-blank field error.
func TestEscrowWebhookRegistration(t *testing.T) {
	base := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	f := newEscrowFixture(t, base)
	create := func(body map[string]any) starlark.Response {
		return f.call("hooks", "on_webhook_create", "POST", "/2017-09-01/customer/me/webhook", nil, nil, body, nil)
	}

	// ===== registration assigns integer ids and echoes the URL =====
	first := create(map[string]any{"url": "https://sink.example.test/escrow"})
	if first.Status != 201 {
		t.Fatalf("create webhook -> %d: %v", first.Status, first.Body)
	}
	if id, ok := first.Body["id"].(int64); !ok || id != 1 {
		t.Fatalf("webhook id = %v (%T), want int64 1", first.Body["id"], first.Body["id"])
	}
	if first.Body["url"] != "https://sink.example.test/escrow" {
		t.Fatalf("webhook url = %v, want round-tripped", first.Body["url"])
	}
	if second := create(map[string]any{"url": "https://other.example.test/hook"}); second.Status != 201 {
		t.Fatalf("second webhook -> %d: %v", second.Status, second.Body)
	}

	// ===== the list carries every registered webhook =====
	listed := f.call("hooks", "on_webhook_list", "GET", "/2017-09-01/customer/me/webhook", nil, nil, nil, nil)
	if listed.Status != 200 {
		t.Fatalf("list webhooks -> %d: %v", listed.Status, listed.Body)
	}
	hooks := escList(t, listed.Body["webhooks"], "webhooks")
	if len(hooks) != 2 {
		t.Fatalf("webhooks = %d entries, want 2", len(hooks))
	}
	if escInt(t, escMap(t, hooks[0], "webhook")["id"], "webhook id") != 1 {
		t.Fatalf("first webhook = %v, want id 1 in registration order", hooks[0])
	}

	// ===== a missing or blank url is the can't-be-blank field error =====
	for name, body := range map[string]map[string]any{
		"no url":    {},
		"blank url": {"url": ""},
	} {
		r := create(body)
		if r.Status != 400 || escError(t, r, "url") != "Url can't be blank" {
			t.Fatalf("create webhook %s -> %d %v, want 400 errors.url", name, r.Status, r.Body)
		}
	}
}
