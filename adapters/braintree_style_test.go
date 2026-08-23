package adapters

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// Drives the braintree-style adapter scripts directly (lib.star preloaded)
// over a shared store and a VIRTUAL clock: the GraphQL surface is executed
// against the engine's real GraphQL executor (schema + convention-named
// resolvers, variables/fragments/introspection, spec-shaped errors[]) while
// the REST surface is dispatched handler-by-handler with extracted path
// params — the transaction lifecycle (authorized -> submitted_for_settlement
// -> settled, derive-on-read on the compressed 1s/3s clocks), the guard
// codes, refunds and their unrefunded-balance cap, the search-criteria
// vocabulary, idempotent creates, subscription billing, and the signed
// bt_signature/bt_payload webhook scheme verified with the real HMAC-SHA1
// algorithm against the mock keypair.
const (
	btMerchantID = "merchant123"
	btTxns       = "/merchants/" + btMerchantID + "/transactions"
	btAuth       = "Bearer bt-token"
	// Mock webhook keypair from scripts/lib.star — the exact strings a
	// receiver must configure to verify signatures.
	btWebhookPublicKey  = "stunt_mock_public_key_2026"
	btWebhookPrivateKey = "stunt_mock_private_key_2026"
)

// btDelivery is one captured outbound webhook delivery.
type btDelivery struct {
	kind    string
	body    string
	headers http.Header
}

type btFixture struct {
	t       *testing.T
	vc      *clock.Clock
	vms     map[string]*starlark.VM
	schema  *ast.Schema
	mu      sync.Mutex
	sink    []btDelivery
	sinkURL string
}

func newBraintreeFixture(t *testing.T, start time.Time) *btFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "braintree-style")
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
	f := &btFixture{t: t, vc: vc, schema: schema}
	// Real (local) sink so signed deliveries can be captured and verified —
	// the same emitter the engine hands handlers.
	em := events.NewEmitter()
	t.Cleanup(em.Close)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.sink = append(f.sink, btDelivery{kind: r.Header.Get("bt-kind"), body: string(b), headers: r.Header.Clone()})
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sink.Close)
	f.sinkURL = sink.URL

	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: em,
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
	f.vms = map[string]*starlark.VM{
		"rest": load("rest.star"), "subs": load("subscriptions.star"),
		"hooks": load("webhooks.star"), "gql": load("resolvers.star"),
	}
	return f
}

// call drives one handler the way the engine dispatches it: the route's path
// params extracted into params, credentials in headers.
func (f *btFixture) call(group, handler, method, path string, params map[string]string, body map[string]any, headers map[string]string) starlark.Response {
	f.t.Helper()
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: "api.sandbox.braintreegateway.test",
		Headers: headers, Body: body, Params: params,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

func (f *btFixture) rest(handler, method, path string, params map[string]string, body map[string]any, headers map[string]string) starlark.Response {
	f.t.Helper()
	return f.call("rest", handler, method, path, params, body, headers)
}

// --- REST fixture drivers ---

func btAuthHdr() map[string]string { return map[string]string{"Authorization": btAuth} }

func (f *btFixture) createTxn(body map[string]any, headers map[string]string) map[string]any {
	f.t.Helper()
	h := btAuthHdr()
	for k, v := range headers {
		h[k] = v
	}
	r := f.rest("on_create_transaction", "POST", btTxns, nil, body, h)
	if r.Status != 200 {
		f.t.Fatalf("create transaction -> %d: %v", r.Status, r.Body)
	}
	return btBodyTxn(f.t, r)
}

func (f *btFixture) getTxn(id string) map[string]any {
	f.t.Helper()
	r := f.rest("on_get_transaction", "GET", btTxns+"/"+id,
		map[string]string{"id": id, "merchantId": btMerchantID}, nil, btAuthHdr())
	if r.Status != 200 {
		f.t.Fatalf("get transaction %s -> %d: %v", id, r.Status, r.Body)
	}
	return btBodyTxn(f.t, r)
}

// txnPath builds the id-scoped route and its params.
func btTxnPath(id string) (string, map[string]string) {
	return btTxns + "/" + id, map[string]string{"id": id, "merchantId": btMerchantID}
}

// --- assertion helpers ---

// btBodyTxn unwraps a {"transaction": {...}} response body.
func btBodyTxn(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	return btObj(t, r.Body, "transaction")
}

// btObj fetches key as a non-nil object.
func btObj(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %v (%T), want object", key, m[key], m[key])
	}
	return v
}

// btRESTErr asserts the REST error envelope: the status plus a non-empty
// {"error": {code, message}} carrying the expected Braintree code.
func btRESTErr(t *testing.T, r starlark.Response, wantStatus int, wantCode string) {
	t.Helper()
	if r.Status != wantStatus {
		t.Fatalf("%s: status -> %d, want %d; body %v", wantCode, r.Status, wantStatus, r.Body)
	}
	e, ok := r.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("%s: error = %v, want object envelope", wantCode, r.Body["error"])
	}
	if e["code"] != wantCode {
		t.Fatalf("error code = %v, want %s (envelope %v)", e["code"], wantCode, e)
	}
	if txt, _ := e["message"].(string); txt == "" {
		t.Fatalf("%s: error message empty", wantCode)
	}
}

// --- GraphQL harness ---

// btResolverSet mirrors the engine's convention-named resolver dispatch:
// root fields on_<field>, object fields resolve_<Type>_<field>, and the
// default parent[field] resolver when neither is defined.
type btResolverSet struct{ f *btFixture }

func (rs *btResolverSet) Lookup(parentType, field string) (graphqlsim.Resolver, bool) {
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
		return btResultToGo(raw)
	}, true
}

// btResultToGo unwraps a respond(...) dict like the engine does; a None body
// is a null GraphQL value.
func btResultToGo(v sk.Value) (any, error) {
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
func (f *btFixture) gql(query string, vars map[string]any) (*graphqlsim.Result, error) {
	return graphqlsim.Execute(context.Background(), f.schema, query, vars, "", &btResolverSet{f}, graphqlsim.Options{})
}

// gqlOK executes and fatals on validation or execution errors, returning data.
func (f *btFixture) gqlOK(query string, vars map[string]any) map[string]any {
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

// gqlErrs executes and fatals when the document does NOT produce errors[].
// Returns the result plus the joined messages.
func (f *btFixture) gqlErrs(query string, vars map[string]any) (*graphqlsim.Result, string) {
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

// --- webhook helpers ---

func (f *btFixture) deliveries() []btDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]btDelivery, len(f.sink))
	copy(out, f.sink)
	return out
}

// btVerifySig checks the Braintree signature scheme on one delivery:
// bt_signature is "<public_key>|<hex(HMAC-SHA1(private_key, bt_payload))>",
// the bt-hash header repeats the MAC, and bt_payload is base64 of the engine
// envelope whose payload carries {timestamp, kind, subject}. Returns the
// decoded envelope.
func btVerifySig(t *testing.T, d btDelivery) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(d.body), &env); err != nil {
		t.Fatalf("delivery body is not JSON: %v (body %s)", err, d.body)
	}
	payload := btObj(t, env, "payload")
	sig, _ := payload["bt_signature"].(string)
	b64, _ := payload["bt_payload"].(string)
	parts := strings.SplitN(sig, "|", 2)
	if len(parts) != 2 || parts[0] != btWebhookPublicKey {
		t.Fatalf("bt_signature = %q, want %s|<hex mac>", sig, btWebhookPublicKey)
	}
	mac := hmac.New(sha1.New, []byte(btWebhookPrivateKey))
	mac.Write([]byte(b64))
	want := hex.EncodeToString(mac.Sum(nil))
	if parts[1] != want {
		t.Fatalf("signature = %s, want HMAC-SHA1 hex %s over bt_payload", parts[1], want)
	}
	if got := d.headers.Get("bt-hash"); got != want {
		t.Fatalf("bt-hash header = %q, want %s", got, want)
	}
	if got := d.headers.Get("bt_signature"); got != sig {
		t.Fatalf("bt_signature header = %q, want the body value %q", got, sig)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("bt_payload is not base64: %v", err)
	}
	var note map[string]any
	if err := json.Unmarshal(raw, &note); err != nil {
		t.Fatalf("decoded bt_payload is not JSON: %v (raw %s)", err, raw)
	}
	return note
}

// TestBraintreeGraphQLSurface: the executor-backed /graphql contract —
// liveness + introspection, validation-time rejection, the customer and
// charge/authorize mutations with their uppercase enum vocabulary, the
// errors[] shape for resolver failures, searchTransactions, and the +3s
// derive-on-read settlement.
func TestBraintreeGraphQLSurface(t *testing.T) {
	f := newBraintreeFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== ping answers true and introspection names the query root =====
	data := f.gqlOK("{ ping }", nil)
	if data["ping"] != true {
		t.Fatalf("ping = %v, want true", data["ping"])
	}
	data = f.gqlOK("{ __schema { queryType { name } } }", nil)
	if got := btObj(t, btObj(t, data, "__schema"), "queryType")["name"]; got != "Query" {
		t.Fatalf("queryType = %v, want Query", got)
	}

	// ===== unknown fields and operations fail validation before execution =====
	if _, err := f.gql(`{ transaction(id: "t1") { bogusField } }`, nil); err == nil {
		t.Fatal("unknown field accepted; want a validation error naming the field")
	} else if !strings.Contains(err.Error(), "bogusField") {
		t.Fatalf("validation error = %v, want it to name bogusField", err)
	}
	if _, err := f.gql(`mutation { captureTransaction(input: {transactionId: "t1"}) { transaction { id } } }`, nil); err == nil {
		t.Fatal("unknown mutation accepted; want a validation error")
	}

	// ===== createCustomer assigns an id and the customer query reads it back =====
	data = f.gqlOK(`mutation($input: CustomerCreateInput!) {
		createCustomer(input: $input) { customer { id firstName lastName email createdAt } }
	}`, map[string]any{"input": map[string]any{
		"firstName": "John", "lastName": "Doe", "email": "john@example.test",
	}})
	customer := btObj(t, btObj(t, data, "createCustomer"), "customer")
	customerID, _ := customer["id"].(string)
	if !strings.HasPrefix(customerID, "customer_") {
		t.Fatalf("customer id = %q, want customer_ prefix", customerID)
	}
	if customer["email"] != "john@example.test" || customer["createdAt"] == "" {
		t.Fatalf("customer = %v, want the input echo + createdAt", customer)
	}
	read := f.gqlOK(`query($id: ID!) { customer(id: $id) { id firstName email } }`,
		map[string]any{"id": customerID})
	if got := btObj(t, read, "customer"); got["firstName"] != "John" || got["id"] != customerID {
		t.Fatalf("customer read-back = %v", got)
	}
	// An unknown customer resolves to null, not an error.
	missing := f.gqlOK(`query($id: ID!) { customer(id: $id) { id } }`,
		map[string]any{"id": "customer_999"})
	if v, ok := missing["customer"]; !ok || v != nil {
		t.Fatalf("unknown customer = %v, want null", missing["customer"])
	}

	// ===== a charge is born SUBMITTED_FOR_SETTLEMENT; an authorization is AUTHORIZED =====
	chargeMut := `mutation($input: ChargePaymentMethodInput!) {
		chargePaymentMethod(input: $input) { transaction { id status type amount currencyISOCode creditCard { last4 cardType expirationDate } } }
	}`
	data = f.gqlOK(chargeMut, map[string]any{"input": map[string]any{
		"paymentMethodId": "pm-1",
		"transaction":     map[string]any{"amount": "50.00", "orderId": "gql-order-1"},
	}})
	charge := btObj(t, btObj(t, data, "chargePaymentMethod"), "transaction")
	chargeID, _ := charge["id"].(string)
	if !strings.HasPrefix(chargeID, "t") {
		t.Fatalf("charge id = %q, want a t-prefixed Braintree id", chargeID)
	}
	if charge["status"] != "SUBMITTED_FOR_SETTLEMENT" || charge["type"] != "SALE" {
		t.Fatalf("charge = %v, want SUBMITTED_FOR_SETTLEMENT SALE (uppercase enums)", charge)
	}
	if charge["amount"] != "50.00" || charge["currencyISOCode"] != "USD" {
		t.Fatalf("charge amount/currency = %v/%v, want decimal-string 50.00 USD", charge["amount"], charge["currencyISOCode"])
	}
	card := btObj(t, charge, "creditCard")
	if card["last4"] != "1111" || card["cardType"] != "Visa" || card["expirationDate"] != "03/2030" {
		t.Fatalf("creditCard = %v, want the synthetic Visa", card)
	}

	data = f.gqlOK(`mutation { authorizeCreditCard(input: {
		creditCard: {number: "4111111111111111", cvv: "123", expirationDate: "03/2030"},
		transaction: {amount: "80.00"}}) { transaction { id status type } } }`, nil)
	authTxn := btObj(t, btObj(t, data, "authorizeCreditCard"), "transaction")
	authID, _ := authTxn["id"].(string)
	if authTxn["status"] != "AUTHORIZED" || authTxn["type"] != "AUTHORIZATION" {
		t.Fatalf("authorization = %v, want AUTHORIZED/AUTHORIZATION", authTxn)
	}

	// ===== resolver failures surface as errors[] with a null field, not an HTTP status =====
	res, msgs := f.gqlErrs(chargeMut, map[string]any{"input": map[string]any{
		"paymentMethodId": "pm-1", "transaction": map[string]any{"amount": "0.00"},
	}})
	if !strings.Contains(msgs, "greater than zero") {
		t.Fatalf("zero-amount charge errors = %q, want the amount guard message", msgs)
	}
	// The mutation fields are non-null (ChargePaymentMethodPayload!), so a
	// resolver failure nulls the whole data root — the {data: null,
	// errors: []} envelope the adapter documents.
	if res.Data != nil {
		t.Fatalf("failed charge data = %v, want null data with errors[]", res.Data)
	}

	voidMut := `mutation($input: VoidTransactionInput!) {
		voidTransaction(input: $input) { transaction { id status voidedAt } }
	}`
	if _, msgs = f.gqlErrs(voidMut, map[string]any{"input": map[string]any{"transactionId": "tnosuch"}}); !strings.Contains(msgs, "not found") {
		t.Fatalf("void of unknown id errors = %q, want not found", msgs)
	}
	data = f.gqlOK(voidMut, map[string]any{"input": map[string]any{"transactionId": authID}})
	voided := btObj(t, btObj(t, data, "voidTransaction"), "transaction")
	if voided["status"] != "VOIDED" || voided["voidedAt"] == "" {
		t.Fatalf("voided transaction = %v, want VOIDED with voidedAt", voided)
	}
	if _, msgs = f.gqlErrs(voidMut, map[string]any{"input": map[string]any{"transactionId": authID}}); !strings.Contains(msgs, "only be voided") {
		t.Fatalf("double-void errors = %q, want the REST void guard message", msgs)
	}

	// ===== searchTransactions speaks the criteria vocabulary with enum normalization =====
	authPmMut := `mutation($input: AuthorizePaymentMethodInput!) {
		authorizePaymentMethod(input: $input) { transaction { id status amount } }
	}`
	for _, amt := range []string{"12.00", "9.00"} {
		f.gqlOK(authPmMut, map[string]any{"input": map[string]any{
			"paymentMethodId": "pm-1", "transaction": map[string]any{"amount": amt},
		}})
	}
	searchQ := `query($s: TransactionSearchInput!) {
		found: searchTransactions(search: $s) { totalCount edges { node { id status amount } } }
	}`
	data = f.gqlOK(searchQ, map[string]any{"s": map[string]any{
		"status": map[string]any{"is": "AUTHORIZED"},
	}})
	if got := btObj(t, data, "found")["totalCount"]; got != int64(2) {
		t.Fatalf("status=AUTHORIZED count = %v, want 2 (the 12.00/9.00 auths; the 80.00 is voided)", got)
	}
	data = f.gqlOK(searchQ, map[string]any{"s": map[string]any{
		"status": map[string]any{"in": []any{"AUTHORIZED", "VOIDED"}},
	}})
	if got := btObj(t, data, "found")["totalCount"]; got != int64(3) {
		t.Fatalf("status in [AUTHORIZED, VOIDED] count = %v, want 3", got)
	}
	data = f.gqlOK(searchQ, map[string]any{"s": map[string]any{
		"amount": map[string]any{"min": "60.00"},
	}})
	big := btObj(t, data, "found")
	if big["totalCount"] != int64(1) {
		t.Fatalf("amount>=60 count = %v, want 1 (only the 80.00)", big["totalCount"])
	}
	edges, ok := big["edges"].([]any)
	if !ok || len(edges) != 1 {
		t.Fatalf("edges = %v, want one", big["edges"])
	}
	if node := btObj(t, edges[0].(map[string]any), "node"); node["id"] != authID || node["amount"] != "80.00" {
		t.Fatalf("amount>=60 node = %v, want the 80.00 voided auth", node)
	}

	// ===== the charge settles at +3s, derived on read =====
	f.vc.Advance(4 * time.Second)
	data = f.gqlOK(`query($id: ID!) { transaction(id: $id) { id status amount settledAt } }`,
		map[string]any{"id": chargeID})
	settled := btObj(t, data, "transaction")
	if settled["status"] != "SETTLED" {
		t.Fatalf("charge after 3s window = %v, want SETTLED", settled["status"])
	}
	if s, _ := settled["settledAt"].(string); s == "" {
		t.Fatalf("settled charge carries no settledAt: %v", settled)
	}
}

// TestBraintreeRestTransactions: the REST surface — the credential gate,
// client tokens and vaulted payment methods, the derive-on-read lifecycle
// (including simulated authorization expiry), the guard codes, refunds and
// their unrefunded-balance cap, idempotent creates, and advanced_search's
// criteria vocabulary.
func TestBraintreeRestTransactions(t *testing.T) {
	f := newBraintreeFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== REST handlers require credentials; Bearer or Basic both unlock =====
	btRESTErr(t, f.rest("on_get_transaction", "GET", btTxns+"/tnope",
		map[string]string{"id": "tnope", "merchantId": btMerchantID}, nil, nil), 401, "AUTHENTICATION")
	basic := f.rest("on_client_token", "POST", "/merchants/"+btMerchantID+"/client_token",
		map[string]string{"merchantId": btMerchantID}, nil,
		map[string]string{"Authorization": "Basic cHViOnByaXY="})
	if basic.Status != 200 {
		t.Fatalf("client_token with basic auth -> %d: %v (public_key:private_key must pass)", basic.Status, basic.Body)
	}

	// ===== client tokens and vaulted payment methods come back in Braintree shapes =====
	ct := f.rest("on_client_token", "POST", "/merchants/"+btMerchantID+"/client_token",
		map[string]string{"merchantId": btMerchantID}, nil, btAuthHdr())
	if ct.Status != 200 {
		t.Fatalf("client_token -> %d: %v", ct.Status, ct.Body)
	}
	if token, _ := ct.Body["client_token"].(string); !strings.HasPrefix(token, "production_cb_") {
		t.Fatalf("client_token = %q, want production_cb_ prefix", token)
	}
	pm := f.rest("on_create_payment_method", "POST", "/merchants/"+btMerchantID+"/payment_methods",
		map[string]string{"merchantId": btMerchantID}, map[string]any{"customer_id": "customer_1"}, btAuthHdr())
	if pm.Status != 200 {
		t.Fatalf("create payment method -> %d: %v", pm.Status, pm.Body)
	}
	method := btObj(t, pm.Body, "payment_method")
	if token, _ := method["token"].(string); !strings.HasPrefix(token, "pm") {
		t.Fatalf("payment method token = %q, want pm prefix", token)
	}
	if method["card_type"] != "Visa" || method["last4"] != "1111" || method["expiration_date"] != "03/2030" {
		t.Fatalf("payment method = %v, want the synthetic Visa card", method)
	}
	if method["customer_id"] != "customer_1" {
		t.Fatalf("payment method customer_id = %v, want echoed", method["customer_id"])
	}

	// ===== the transaction lifecycle derives on the clock =====
	manual := f.createTxn(map[string]any{"amount": "80.00", "type": "sale"}, nil)
	manualID, _ := manual["id"].(string)
	if manual["status"] != "authorized" || manual["amount"] != "80.00" || manual["createdAt"] == "" {
		t.Fatalf("manual sale = %v, want authorized 80.00 with createdAt", manual)
	}
	auto := f.createTxn(map[string]any{
		"amount": "100.00", "type": "sale",
		"options": map[string]any{"submit_for_settlement": true},
	}, nil)
	autoID, _ := auto["id"].(string)
	if auto["status"] != "authorized" {
		t.Fatalf("auto sale born %v, want authorized (the submit happens at +1s)", auto["status"])
	}
	f.vc.Advance(2 * time.Second)
	if got := f.getTxn(autoID)["status"]; got != "submitted_for_settlement" {
		t.Fatalf("auto sale at +2s = %v, want submitted_for_settlement", got)
	}
	if got := f.getTxn(manualID)["status"]; got != "authorized" {
		t.Fatalf("manual sale advanced to %v; without the option it must wait for capture", got)
	}
	f.vc.Advance(2 * time.Second)
	settledAuto := f.getTxn(autoID)
	if settledAuto["status"] != "settled" || settledAuto["settledAt"] == "" {
		t.Fatalf("auto sale at +4s = %v, want settled with settledAt", settledAuto)
	}

	// ===== guard failures carry the real Braintree error codes =====
	btRESTErr(t, f.rest("on_create_transaction", "POST", btTxns, nil,
		map[string]any{"amount": "-5.00", "type": "sale"}, btAuthHdr()), 422, "81501")
	mpath, mparams := btTxnPath(manualID)
	btRESTErr(t, f.rest("on_refund_transaction", "POST", mpath+"/refund", mparams,
		map[string]any{}, btAuthHdr()), 422, "91507")
	btRESTErr(t, f.rest("on_settle_transaction", "POST", mpath+"/settle", mparams,
		map[string]any{"amount": "0.00"}, btAuthHdr()), 422, "81501")
	partial := f.rest("on_settle_transaction", "POST", mpath+"/settle", mparams,
		map[string]any{"amount": "50.00"}, btAuthHdr())
	if partial.Status != 200 {
		t.Fatalf("partial capture -> %d: %v", partial.Status, partial.Body)
	}
	if pt := btBodyTxn(t, partial); pt["status"] != "submitted_for_settlement" || pt["amount"] != "50.00" {
		t.Fatalf("partial capture = %v, want submitted_for_settlement at 50.00", pt)
	}
	over := f.createTxn(map[string]any{"amount": "30.00"}, nil)
	opath, oparams := btTxnPath(over["id"].(string))
	btRESTErr(t, f.rest("on_settle_transaction", "POST", opath+"/settle", oparams,
		map[string]any{"amount": "40.00"}, btAuthHdr()), 422, "91522")
	voidTxn := f.createTxn(map[string]any{"amount": "20.00"}, nil)
	vpath, vparams := btTxnPath(voidTxn["id"].(string))
	vr := f.rest("on_void_transaction", "POST", vpath+"/void", vparams, map[string]any{}, btAuthHdr())
	if vr.Status != 200 {
		t.Fatalf("void authorized -> %d: %v", vr.Status, vr.Body)
	}
	if vt := btBodyTxn(t, vr); vt["status"] != "voided" || vt["voidedAt"] == "" {
		t.Fatalf("voided = %v, want voided with voidedAt", vt)
	}
	btRESTErr(t, f.rest("on_void_transaction", "POST", vpath+"/void", vparams,
		map[string]any{}, btAuthHdr()), 422, "91506")
	btRESTErr(t, f.rest("on_refund_transaction", "POST", btTxns+"/tnosuch/refund",
		map[string]string{"id": "tnosuch", "merchantId": btMerchantID}, map[string]any{}, btAuthHdr()), 404, "NOT_FOUND")
	f.vc.Advance(4 * time.Second)
	if got := f.getTxn(manualID)["status"]; got != "settled" {
		t.Fatalf("partial-captured sale at +3s = %v, want settled", got)
	}

	// ===== refunds default to the unrefunded balance and cap at it =====
	rpath, rparams := btTxnPath(autoID)
	r1 := f.rest("on_refund_transaction", "POST", rpath+"/refund", rparams,
		map[string]any{"amount": "10.00"}, btAuthHdr())
	if r1.Status != 200 {
		t.Fatalf("partial refund -> %d: %v", r1.Status, r1.Body)
	}
	if rt := btBodyTxn(t, r1); rt["type"] != "credit" || rt["amount"] != "10.00" || rt["refundedTransactionId"] != autoID {
		t.Fatalf("refund = %v, want a 10.00 credit pointing at the original", rt)
	}
	btRESTErr(t, f.rest("on_refund_transaction", "POST", rpath+"/refund", rparams,
		map[string]any{"amount": "95.00"}, btAuthHdr()), 422, "91521")
	r2 := f.rest("on_refund_transaction", "POST", rpath+"/refund", rparams,
		map[string]any{}, btAuthHdr())
	if r2.Status != 200 {
		t.Fatalf("default refund -> %d: %v", r2.Status, r2.Body)
	}
	if rt := btBodyTxn(t, r2); rt["amount"] != "90.00" {
		t.Fatalf("default refund = %v, want the remaining 90.00", rt["amount"])
	}
	btRESTErr(t, f.rest("on_refund_transaction", "POST", rpath+"/refund", rparams,
		map[string]any{"amount": "0.01"}, btAuthHdr()), 422, "91521")

	// ===== an uncaptured authorization expires past its window =====
	exp := f.createTxn(map[string]any{"amount": "25.00", "simulate_authorization_expiry": true}, nil)
	if exp["status"] != "authorized" {
		t.Fatalf("expiring sale born %v, want authorized", exp["status"])
	}
	f.vc.Advance(2 * time.Second)
	if got := f.getTxn(exp["id"].(string))["status"]; got != "authorization_expired" {
		t.Fatalf("expiring sale after window = %v, want authorization_expired", got)
	}
	epath, eparams := btTxnPath(exp["id"].(string))
	btRESTErr(t, f.rest("on_void_transaction", "POST", epath+"/void", eparams,
		map[string]any{}, btAuthHdr()), 422, "91506")

	// ===== an Idempotency-Key replays the original create =====
	idemHdr := map[string]string{"Idempotency-Key": "idem-1"}
	first := f.createTxn(map[string]any{"amount": "10.00"}, idemHdr)
	replay := f.createTxn(map[string]any{"amount": "10.00"}, idemHdr)
	if replay["id"] != first["id"] {
		t.Fatalf("idempotent replay = %v, want the original %v", replay["id"], first["id"])
	}
	other := f.createTxn(map[string]any{"amount": "10.00"}, map[string]string{"Idempotency-Key": "idem-2"})
	if other["id"] == first["id"] {
		t.Fatalf("different key produced %v, want a fresh transaction", other["id"])
	}

	// ===== advanced_search maps the search-criteria vocabulary onto typed filters =====
	search := func(s map[string]any) map[string]any {
		f.t.Helper()
		r := f.rest("on_advanced_search", "POST", btTxns+"/advanced_search",
			map[string]string{"merchantId": btMerchantID}, map[string]any{"search": s}, btAuthHdr())
		if r.Status != 200 {
			f.t.Fatalf("advanced_search -> %d: %v", r.Status, r.Body)
		}
		return r.Body
	}
	ids := func(body map[string]any) map[string]bool {
		f.t.Helper()
		out := map[string]bool{}
		for _, it := range body["transactions"].([]any) {
			out[it.(map[string]any)["id"].(string)] = true
		}
		return out
	}
	settledSales := ids(search(map[string]any{
		"status": map[string]any{"in": []any{"settled"}},
		"type":   map[string]any{"is": "sale"},
	}))
	if len(settledSales) != 2 || !settledSales[manualID] || !settledSales[autoID] {
		t.Fatalf("settled sales = %v, want exactly manual %s + auto %s (refunds are credits)", settledSales, manualID, autoID)
	}
	if settledSales[voidTxn["id"].(string)] || settledSales[exp["id"].(string)] {
		t.Fatal("voided/expired transactions matched a settled-only search")
	}
	ranged := search(map[string]any{"amount": map[string]any{"min": "95.00", "max": "110.00"}})
	if ranged["total_count"] != int64(1) || !ids(ranged)[autoID] {
		t.Fatalf("amount range = %v, want only the 100.00 sale", ranged)
	}
	byID := search(map[string]any{"id": autoID})
	if byID["total_count"] != int64(1) || !ids(byID)[autoID] {
		t.Fatalf("id search = %v, want only %s", byID, autoID)
	}
	last4op := search(map[string]any{"credit_card_number": map[string]any{"ends_with": "1111"}})
	last4bare := search(map[string]any{"credit_card_number": "1111"})
	if last4op["total_count"] == int64(0) || last4op["total_count"] != last4bare["total_count"] {
		t.Fatalf("last4 search operator=%v bare=%v, want the same non-empty set (numeric-looking strings compare numerically is NOT wanted here — both match the stored 1111)", last4op["total_count"], last4bare["total_count"])
	}
	if got := search(map[string]any{"credit_card_number": map[string]any{"ends_with": "9999"}})["total_count"]; got != int64(0) {
		t.Fatalf("last4=9999 matched %v transactions, want none", got)
	}
}

// TestBraintreeSubscriptionsAndWebhookSigning: webhook registration with its
// signed check notification, one signed delivery per new lifecycle state
// filtered by the hook's kinds, the subscription billing machine, and the
// inbound verification split.
func TestBraintreeSubscriptionsAndWebhookSigning(t *testing.T) {
	f := newBraintreeFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== registration stores the hook and delivers a signed check =====
	reg := f.call("hooks", "on_webhook", "POST", "/webhooks", nil, map[string]any{
		"url": f.sinkURL, "kinds": []any{"transaction_settled"},
	}, nil)
	if reg.Status != 201 {
		t.Fatalf("register webhook -> %d: %v", reg.Status, reg.Body)
	}
	hook := btObj(t, reg.Body, "webhook")
	if hook["url"] != f.sinkURL {
		t.Fatalf("hook url = %v, want the sink", hook["url"])
	}
	if kinds, _ := hook["kinds"].([]any); len(kinds) != 1 || kinds[0] != "transaction_settled" {
		t.Fatalf("hook kinds = %v, want echoed", hook["kinds"])
	}
	dv := f.deliveries()
	if len(dv) != 1 || dv[0].kind != "check" {
		t.Fatalf("deliveries after registration = %d (%v), want exactly one check", len(dv), dv)
	}
	note := btVerifySig(t, dv[0])
	if note["type"] != "check" {
		t.Fatalf("check envelope type = %v, want check", note["type"])
	}
	np := btObj(t, note, "payload")
	if np["kind"] != "check" || np["timestamp"] == "" {
		t.Fatalf("check notification = %v, want kind + timestamp", np)
	}
	if got := btObj(t, btObj(t, np, "subject"), "merchant_account")["id"]; got != "stunt_mock_merchant" {
		t.Fatalf("check subject merchant_account = %v", got)
	}

	// ===== settled transactions fire one signed notification per new state, filtered by kind =====
	txn := f.createTxn(map[string]any{
		"amount": "60.00", "options": map[string]any{"submit_for_settlement": true},
	}, nil)
	txnID, _ := txn["id"].(string)
	f.vc.Advance(4 * time.Second)
	if got := f.getTxn(txnID)["status"]; got != "settled" {
		t.Fatalf("auto sale at +4s = %v, want settled", got)
	}
	dv = f.deliveries()
	if len(dv) != 2 || dv[1].kind != "transaction_settled" {
		t.Fatalf("deliveries after settlement = %d (%v), want exactly one transaction_settled", len(dv), dv)
	}
	note = btVerifySig(t, dv[1])
	np = btObj(t, note, "payload")
	if np["kind"] != "transaction_settled" {
		t.Fatalf("settlement notification kind = %v", np["kind"])
	}
	if got := btObj(t, btObj(t, np, "subject"), "transaction")["id"]; got != txnID {
		t.Fatalf("settlement subject transaction = %v, want %s", got, txnID)
	}
	// A repeated read does not re-emit.
	f.getTxn(txnID)
	if got := len(f.deliveries()); got != 2 {
		t.Fatalf("re-read emitted again: %d deliveries", got)
	}
	// refund_opened is not in the hook's kinds, so it is not delivered.
	rpath, rparams := btTxnPath(txnID)
	refund := f.rest("on_refund_transaction", "POST", rpath+"/refund", rparams, map[string]any{}, btAuthHdr())
	if refund.Status != 200 {
		t.Fatalf("refund settled sale -> %d: %v", refund.Status, refund.Body)
	}
	if got := len(f.deliveries()); got != 2 {
		t.Fatalf("refund_opened delivered %d times; the hook subscribes only to transaction_settled", got-2)
	}

	// ===== subscriptions bill per cycle, expire at the last, and cancel only while Active =====
	plans := "/merchants/" + btMerchantID + "/plans"
	pr := f.call("subs", "on_create_plan", "POST", plans, map[string]string{"merchantId": btMerchantID},
		map[string]any{"id": "starter-plan", "name": "Starter", "price": "15.00", "number_of_billing_cycles": 3},
		btAuthHdr())
	if pr.Status != 200 {
		t.Fatalf("create plan -> %d: %v", pr.Status, pr.Body)
	}
	plan := btObj(t, pr.Body, "plan")
	if plan["id"] != "starter-plan" || plan["price"] != "15.00" || plan["number_of_billing_cycles"] != int64(3) {
		t.Fatalf("plan = %v, want starter-plan at 15.00/3 cycles", plan)
	}
	listed := f.call("subs", "on_list_plans", "GET", plans, map[string]string{"merchantId": btMerchantID}, nil, btAuthHdr())
	if got, _ := listed.Body["total_count"].(int64); got != 1 {
		t.Fatalf("plan list count = %v, want 1", listed.Body["total_count"])
	}
	if r := f.call("subs", "on_get_plan", "GET", plans+"/starter-plan",
		map[string]string{"id": "starter-plan", "merchantId": btMerchantID}, nil, btAuthHdr()); r.Status != 200 {
		t.Fatalf("get plan -> %d: %v", r.Status, r.Body)
	}
	btRESTErr(t, f.call("subs", "on_get_plan", "GET", plans+"/nope",
		map[string]string{"id": "nope", "merchantId": btMerchantID}, nil, btAuthHdr()), 404, "NOT_FOUND")

	subs := "/merchants/" + btMerchantID + "/subscriptions"
	btRESTErr(t, f.call("subs", "on_create_subscription", "POST", subs,
		map[string]string{"merchantId": btMerchantID}, map[string]any{"plan_id": "no-such-plan"}, btAuthHdr()), 404, "NOT_FOUND")
	s1 := f.call("subs", "on_create_subscription", "POST", subs, map[string]string{"merchantId": btMerchantID}, map[string]any{
		"subscription": map[string]any{
			"plan_id": "starter-plan", "payment_method_token": "pm-token", "number_of_billing_cycles": 1,
		},
	}, btAuthHdr())
	if s1.Status != 200 {
		t.Fatalf("create subscription -> %d: %v", s1.Status, s1.Body)
	}
	sub1 := btObj(t, s1.Body, "subscription")
	sub1ID, _ := sub1["id"].(string)
	if sub1["status"] != "Active" || sub1["plan_id"] != "starter-plan" || sub1["price"] != "15.00" {
		t.Fatalf("subscription = %v, want Active on the plan at its price", sub1)
	}
	if sub1["billing_cycles_completed"] != int64(0) || sub1["billing_cycles_remaining"] != int64(1) {
		t.Fatalf("subscription cycles = %v/%v, want 0 done / 1 remaining", sub1["billing_cycles_completed"], sub1["billing_cycles_remaining"])
	}
	s2 := f.call("subs", "on_create_subscription", "POST", subs, map[string]string{"merchantId": btMerchantID},
		map[string]any{"plan_id": "starter-plan"}, btAuthHdr())
	sub2 := btObj(t, s2.Body, "subscription")
	sub2ID, _ := sub2["id"].(string)
	if tok, _ := sub2["payment_method_token"].(string); !strings.HasPrefix(tok, "pm") {
		t.Fatalf("default payment method token = %q, want a generated pm token", tok)
	}

	f.vc.Advance(2 * time.Second)
	g1 := f.call("subs", "on_get_subscription", "GET", subs+"/"+sub1ID,
		map[string]string{"id": sub1ID, "merchantId": btMerchantID}, nil, btAuthHdr())
	if got := btObj(t, g1.Body, "subscription"); got["status"] != "Expired" ||
		got["billing_cycles_completed"] != int64(1) || got["billing_cycles_remaining"] != int64(0) {
		t.Fatalf("subscription after its only cycle = %v, want Expired 1/0", got)
	}
	btRESTErr(t, f.call("subs", "on_cancel_subscription", "POST", subs+"/"+sub1ID+"/cancel",
		map[string]string{"id": sub1ID, "merchantId": btMerchantID}, map[string]any{}, btAuthHdr()), 422, "81902")
	g2 := f.call("subs", "on_get_subscription", "GET", subs+"/"+sub2ID,
		map[string]string{"id": sub2ID, "merchantId": btMerchantID}, nil, btAuthHdr())
	if got := btObj(t, g2.Body, "subscription"); got["status"] != "Active" ||
		got["billing_cycles_completed"] != int64(1) || got["billing_cycles_remaining"] != int64(2) {
		t.Fatalf("subscription after one of three cycles = %v, want Active 1/2", got)
	}
	cancel := f.call("subs", "on_cancel_subscription", "POST", subs+"/"+sub2ID+"/cancel",
		map[string]string{"id": sub2ID, "merchantId": btMerchantID}, map[string]any{}, btAuthHdr())
	if cancel.Status != 200 {
		t.Fatalf("cancel active subscription -> %d: %v", cancel.Status, cancel.Body)
	}
	if got := btObj(t, cancel.Body, "subscription"); got["status"] != "Canceled" {
		t.Fatalf("canceled subscription = %v, want Canceled", got)
	}
	// Subscription notifications stay filtered: the hook subscribes only to
	// transaction_settled, so no billing deliveries landed on the sink.
	if got := len(f.deliveries()); got != 2 {
		t.Fatalf("subscription lifecycle delivered %d extra notifications", got-2)
	}

	// ===== inbound verification splits 200/400 =====
	okIn := f.call("hooks", "on_webhook", "POST", "/webhooks", nil, map[string]any{
		"bt_signature": "key|abc123sig", "bt_payload": "YmFzZTY0cGF5bG9hZA==",
	}, nil)
	if okIn.Status != 200 || okIn.Body["status"] != "OK" {
		t.Fatalf("inbound verification -> %d %v, want 200 OK", okIn.Status, okIn.Body)
	}
	badIn := f.call("hooks", "on_webhook", "POST", "/webhooks", nil, map[string]any{
		"bt_signature": "key|abc123sig",
	}, nil)
	if badIn.Status != 400 {
		t.Fatalf("inbound verification without payload -> %d, want 400", badIn.Status)
	}
	if r := f.call("hooks", "on_webhook", "POST", "/webhooks", nil, nil, nil); r.Status != 400 {
		t.Fatalf("webhook without body -> %d, want 400", r.Status)
	}
}
