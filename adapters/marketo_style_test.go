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

// Drives the marketo-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: the OAuth client_credentials
// mint, lead create/get/upsert-by-email, batchSize/nextPageToken paging,
// and the 403 envelope for invalid access tokens.
type marketoFixture struct {
	t      *testing.T
	vc     *clock.Clock
	vmLe   *starlark.VM
	vmAuth *starlark.VM
	host   string
}

func newMarketoFixture(t *testing.T, start time.Time) *marketoFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "marketo-style")
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
	return &marketoFixture{t: t, vc: vc, vmLe: load("leads.star"), vmAuth: load("auth.star"), host: "123-abcd.mktorest.test"}
}

func (f *marketoFixture) token(query map[string]string) starlark.Response {
	f.t.Helper()
	resp, err := f.vmAuth.Call("on_token", starlark.Request{
		Method: "POST", Path: "/identity/oauth/token", Host: f.host, Query: query,
	})
	if err != nil {
		f.t.Fatalf("on_token: %v", err)
	}
	return resp
}

func (f *marketoFixture) le(handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vmLe.Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

func TestMarketoLeadsLifecycle(t *testing.T) {
	base := time.Date(2026, 2, 3, 10, 0, 0, 0, time.UTC)
	f := newMarketoFixture(t, base)

	// Only client_credentials mints; missing params are a 400.
	if r := f.token(map[string]string{"grant_type": "authorization_code", "client_id": "x", "client_secret": "y"}); r.Status != 400 {
		t.Fatalf("authorization_code grant -> %d, want 400", r.Status)
	}
	tok := f.token(map[string]string{"grant_type": "client_credentials", "client_id": "conf", "client_secret": "secret"})
	if tok.Status != 200 || tok.Body["access_token"] == "" {
		t.Fatalf("token -> %d %v", tok.Status, tok.Body)
	}
	bearer := "Bearer " + tok.Body["access_token"].(string)

	// Invalid access token -> the Marketo 403 envelope, success=false.
	bad := f.le("on_list_leads", "GET", "/rest/v1/leads", nil, nil, nil, "Bearer nope")
	if bad.Status != 403 || bad.Body["success"] != false {
		t.Fatalf("bad token -> %d %v", bad.Status, bad.Body)
	}

	// Create via the input array; ids are assigned.
	created := f.le("on_create_lead", "POST", "/rest/v1/leads", nil, nil, map[string]any{
		"action": "createOnly",
		"input":  []any{map[string]any{"email": "vm@example.test", "firstName": "VM"}},
	}, bearer)
	if created.Status != 200 || created.Body["success"] != true {
		t.Fatalf("create -> %d %v", created.Status, created.Body)
	}
	res := created.Body["result"].([]any)
	if len(res) != 1 || res[0].(map[string]any)["status"] != "created" {
		t.Fatalf("create result = %v", res)
	}
	leadID, _ := res[0].(map[string]any)["id"].(string)

	// Get by id round-trips the fields.
	got := f.le("on_get_lead", "GET", "/rest/v1/leads/"+leadID, map[string]string{"id": leadID}, nil, nil, bearer)
	if got.Status != 200 {
		t.Fatalf("get lead -> %d %v", got.Status, got.Body)
	}
	gres := got.Body["result"].([]any)
	if len(gres) == 0 || gres[0].(map[string]any)["email"] != "vm@example.test" {
		t.Fatalf("get lead result = %v", gres)
	}

	// createOrUpdate dedupes by email: same email -> updated, no new row.
	f.le("on_create_lead", "POST", "/rest/v1/leads", nil, nil, map[string]any{
		"action": "createOrUpdate",
		"input":  []any{map[string]any{"email": "vm@example.test", "firstName": "Renamed"}},
	}, bearer)
	upd := f.le("on_get_lead", "GET", "/rest/v1/leads/"+leadID, map[string]string{"id": leadID}, nil, nil, bearer)
	if ures := upd.Body["result"].([]any); len(ures) == 0 || ures[0].(map[string]any)["firstName"] != "Renamed" {
		t.Fatalf("upsert did not update in place: %v", ures)
	}

	// A second lead, then batchSize paging walks both without overlap.
	f.le("on_create_lead", "POST", "/rest/v1/leads", nil, nil, map[string]any{
		"action": "createOnly",
		"input":  []any{map[string]any{"email": "second@example.test"}},
	}, bearer)
	page1 := f.le("on_list_leads", "GET", "/rest/v1/leads", nil, map[string]string{"batchSize": "1"}, nil, bearer)
	if p1, ok := page1.Body["result"].([]any); !ok || len(p1) != 1 {
		t.Fatalf("page1 result = %v", page1.Body)
	}
	next, _ := page1.Body["nextPageToken"].(string)
	if next == "" || page1.Body["moreResult"] != true {
		t.Fatalf("page1 nextPageToken=%q moreResult=%v", next, page1.Body["moreResult"])
	}
	page2 := f.le("on_list_leads", "GET", "/rest/v1/leads", nil, map[string]string{"batchSize": "1", "nextPageToken": next}, nil, bearer)
	id1 := page1.Body["result"].([]any)[0].(map[string]any)["id"]
	id2 := page2.Body["result"].([]any)[0].(map[string]any)["id"]
	if id1 == id2 {
		t.Fatalf("paging returned the same lead twice (%v)", id1)
	}
}
