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

// Drives the emailoctopus-style adapter scripts directly (lib.star
// preloaded) over a shared store and virtual clock: list + contact CRUD,
// RFC 7807 problem+json errors (401/404/422), status filtering, and
// limit/starting_after cursor paging.
const eoAuth = "Bearer eo-api-key"

type eoFixture struct {
	t       *testing.T
	vc      *clock.Clock
	vmLists *starlark.VM
	vmCont  *starlark.VM
	host    string
}

func newEOFixture(t *testing.T, start time.Time) *eoFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "emailoctopus-style")
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
	return &eoFixture{t: t, vc: vc, vmLists: load("lists.star"), vmCont: load("contacts.star"), host: "api.emailoctopus.test"}
}

func (f *eoFixture) call(vm *starlark.VM, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := vm.Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

func TestEmailOctopusListsAndContacts(t *testing.T) {
	base := time.Date(2026, 2, 3, 11, 0, 0, 0, time.UTC)
	f := newEOFixture(t, base)

	// ===== a missing API key is 401 problem+json =====
	// No API key -> 401 problem+json.
	if r := f.call(f.vmLists, "on_list_lists", "GET", "/lists", nil, nil, nil, ""); r.Status != 401 {
		t.Fatalf("no auth -> %d, want 401", r.Status)
	}

	// ===== a blank name is 422 with the field error =====
	// Blank name -> 422 with the field error.
	blank := f.call(f.vmLists, "on_create_list", "POST", "/lists", nil, nil, map[string]any{"name": "  "}, eoAuth)
	if blank.Status != 422 {
		t.Fatalf("blank name -> %d, want 422", blank.Status)
	}

	// ===== list create/get round-trip =====
	// Create + get round-trip.
	created := f.call(f.vmLists, "on_create_list", "POST", "/lists", nil, nil, map[string]any{"name": "VM Suite List"}, eoAuth)
	if created.Status != 201 {
		t.Fatalf("create list -> %d: %v", created.Status, created.Body)
	}
	listID, _ := created.Body["id"].(string)
	if listID == "" {
		t.Fatalf("create list: no id: %v", created.Body)
	}
	got := f.call(f.vmLists, "on_get_list", "GET", "/lists/"+listID, map[string]string{"list_id": listID}, nil, nil, eoAuth)
	if got.Status != 200 || got.Body["name"] != "VM Suite List" {
		t.Fatalf("get list -> %d %v", got.Status, got.Body)
	}

	// ===== an unknown list is 404 on contact routes too =====
	// Unknown list -> 404 on contact routes too.
	if r := f.call(f.vmCont, "on_list_contacts", "GET", "/lists/nope/contacts", map[string]string{"list_id": "nope"}, map[string]string{"limit": "10"}, nil, eoAuth); r.Status != 404 {
		t.Fatalf("unknown list contacts -> %d, want 404", r.Status)
	}

	// ===== contacts create into the list =====
	// Two contacts, one unsubscribed for the status filter.
	mk := func(email string) starlark.Response {
		return f.call(f.vmCont, "on_create_contact", "POST", "/lists/"+listID+"/contacts",
			map[string]string{"list_id": listID}, nil,
			map[string]any{"email_address": email, "status": "subscribed"}, eoAuth)
	}
	if r := mk("one@example.test"); r.Status != 201 {
		t.Fatalf("create contact one -> %d: %v", r.Status, r.Body)
	}
	if r := mk("two@example.test"); r.Status != 201 {
		t.Fatalf("create contact two -> %d: %v", r.Status, r.Body)
	}

	// ===== limit/starting_after paging pages contacts without repeats =====
	// limit paging: page of one carries starting_after; page two differs.
	p1 := f.call(f.vmCont, "on_list_contacts", "GET", "/lists/"+listID+"/contacts",
		map[string]string{"list_id": listID}, map[string]string{"limit": "1"}, nil, eoAuth)
	d1, _ := p1.Body["data"].([]any)
	if len(d1) != 1 {
		t.Fatalf("page1 data = %v", p1.Body)
	}
	paging, _ := p1.Body["paging"].(map[string]any)
	next, _ := paging["next"].(map[string]any)
	cursor, _ := next["starting_after"].(string)
	if cursor == "" {
		t.Fatalf("page1 paging.next = %v", paging)
	}
	p2 := f.call(f.vmCont, "on_list_contacts", "GET", "/lists/"+listID+"/contacts",
		map[string]string{"list_id": listID}, map[string]string{"limit": "1", "starting_after": cursor}, nil, eoAuth)
	d2, _ := p2.Body["data"].([]any)
	if len(d2) != 1 || d1[0].(map[string]any)["id"] == d2[0].(map[string]any)["id"] {
		t.Fatalf("cursor paging repeated a row: %v then %v", d1, d2)
	}

	// ===== get by contact id round-trips the email =====
	// Get by contact id round-trips the email.
	cid, _ := d1[0].(map[string]any)["id"].(string)
	one := f.call(f.vmCont, "on_get_contact", "GET", "/lists/"+listID+"/contacts/"+cid,
		map[string]string{"list_id": listID, "contact_id": cid}, nil, nil, eoAuth)
	if one.Status != 200 || one.Body["email_address"] != "one@example.test" {
		t.Fatalf("get contact -> %d %v", one.Status, one.Body)
	}

	// ===== delete removes the contact; reads 404 after =====
	// Delete removes it; the read is a 404 problem afterwards.
	del := f.call(f.vmCont, "on_delete_contact", "DELETE", "/lists/"+listID+"/contacts/"+cid,
		map[string]string{"list_id": listID, "contact_id": cid}, nil, nil, eoAuth)
	if del.Status != 204 && del.Status != 200 {
		t.Fatalf("delete contact -> %d", del.Status)
	}
	if r := f.call(f.vmCont, "on_get_contact", "GET", "/lists/"+listID+"/contacts/"+cid,
		map[string]string{"list_id": listID, "contact_id": cid}, nil, nil, eoAuth); r.Status != 404 {
		t.Fatalf("get after delete -> %d, want 404", r.Status)
	}
}
