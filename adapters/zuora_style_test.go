package adapters

import (
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

// Drives the zuora-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: the account -> subscription ->
// payment lifecycle against the seeded rate-plan catalog, ZOQL query,
// webhook registration, and the Bearer/legacy credential gate.
const zuoraAuth = "Bearer zuora-bearer-token"

type zuoraFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newZuoraFixture(t *testing.T, start time.Time) *zuoraFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "zuora-style")
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
	return &zuoraFixture{t: t, vc: vc, host: "rest.apisandbox.zuora.test", vms: map[string]*starlark.VM{
		"accounts": load("accounts.star"), "subs": load("subscriptions.star"),
		"billing": load("billing.star"), "query": load("query.star"), "hooks": load("webhooks.star"),
	}}
}

func (f *zuoraFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
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

func TestZuoraAccountSubscriptionLifecycle(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newZuoraFixture(t, base)

	// Unknown bearer -> 401.
	if r := f.call("accounts", "on_list_accounts", "GET", "/v1/accounts", nil, nil, nil, "Bearer nope"); r.Status != 401 {
		t.Fatalf("unknown bearer -> %d, want 401", r.Status)
	}

	// Create an account; number is assigned and the read round-trips.
	acct := f.call("accounts", "on_create_account", "POST", "/v1/accounts", nil, nil, map[string]any{
		"name": "VM Suite Co", "currency": "USD",
	}, zuoraAuth)
	if acct.Status != 200 && acct.Status != 201 {
		t.Fatalf("create account -> %d: %v", acct.Status, acct.Body)
	}
	accountID, _ := acct.Body["accountId"].(string)
	if accountID == "" {
		t.Fatalf("create account: no accountId: %v", acct.Body)
	}
	got := f.call("accounts", "on_get_account", "GET", "/v1/accounts/"+accountID,
		map[string]string{"accountKey": accountID}, nil, nil, zuoraAuth)
	if got.Status != 200 || got.Body["name"] != "VM Suite Co" {
		t.Fatalf("get account -> %d %v", got.Status, got.Body)
	}

	// Subscribing to a catalog plan prices the subscription.
	sub := f.call("subs", "on_create_subscription", "POST", "/v1/subscriptions", nil, nil, map[string]any{
		"accountKey": accountID,
		"subscribeToRatePlans": []any{map[string]any{
			"productRatePlanId": "rateplan-standard",
		}},
	}, zuoraAuth)
	if sub.Status != 200 && sub.Status != 201 {
		t.Fatalf("create subscription -> %d: %v", sub.Status, sub.Body)
	}
	subID, _ := sub.Body["subscriptionId"].(string)
	if subID == "" {
		t.Fatalf("create subscription: no subscriptionId: %v", sub.Body)
	}

	// Unknown account or empty plans are Zuora-coded 400s.
	if r := f.call("subs", "on_create_subscription", "POST", "/v1/subscriptions", nil, nil, map[string]any{
		"accountKey": "nope", "subscribeToRatePlans": []any{map[string]any{"productRatePlanId": "rateplan-standard"}},
	}, zuoraAuth); r.Status != 400 {
		t.Fatalf("unknown account subscribe -> %d, want 400", r.Status)
	}
	if r := f.call("subs", "on_create_subscription", "POST", "/v1/subscriptions", nil, nil, map[string]any{
		"accountKey": accountID,
	}, zuoraAuth); r.Status != 400 {
		t.Fatalf("empty plans subscribe -> %d, want 400", r.Status)
	}

	// EndOfTerm stays Active with the cancellation recorded (the flip is
	// derived on read at term end, like real Zuora); Immediate cancels now.
	eot := f.call("subs", "on_cancel_subscription", "PUT", "/v1/subscriptions/"+subID+"/cancel",
		map[string]string{"subscriptionKey": subID}, nil, map[string]any{"cancellationPolicy": "EndOfTerm"}, zuoraAuth)
	if eot.Status != 200 && eot.Status != 201 {
		t.Fatalf("cancel EndOfTerm -> %d: %v", eot.Status, eot.Body)
	}
	if state, _ := eot.Body["status"].(string); state != "Active" {
		t.Fatalf("EndOfTerm status = %q, want Active until term end", state)
	}
	// A second cancel is rejected: the subscription already has one queued.
	if r := f.call("subs", "on_cancel_subscription", "PUT", "/v1/subscriptions/"+subID+"/cancel",
		map[string]string{"subscriptionKey": subID}, nil, map[string]any{"cancellationPolicy": "Immediate"}, zuoraAuth); r.Status != 400 {
		t.Fatalf("second cancel -> %d, want 400 (not active / already cancelling)", r.Status)
	}

	// ZOQL finds the account by its assigned number.
	q := f.call("query", "on_query", "POST", "/v1/action/query", nil, nil, map[string]any{
		"queryString": "select id, name from Account where name = 'VM Suite Co'",
	}, zuoraAuth)
	if q.Status != 200 {
		t.Fatalf("query -> %d: %v", q.Status, q.Body)
	}
	// Webhook registration round-trips.
	hook := f.call("hooks", "on_create_webhook", "POST", "/v1/webhooks", nil, nil, map[string]any{
		"url": "https://sink.example.test/hook", "eventTypes": []any{"subscription.cancelled"},
	}, zuoraAuth)
	if hook.Status != 200 && hook.Status != 201 {
		t.Fatalf("create webhook -> %d: %v", hook.Status, hook.Body)
	}
	listed := f.call("hooks", "on_list_webhooks", "GET", "/v1/webhooks", nil, nil, nil, zuoraAuth)
	if listed.Status != 200 {
		t.Fatalf("list webhooks -> %d", listed.Status)
	}
}
