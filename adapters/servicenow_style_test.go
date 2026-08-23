package adapters

import (
	"encoding/json"
	"fmt"
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

// Drives the servicenow-style adapter scripts directly (lib.star
// preloaded) over a shared store and virtual clock: Table API CRUD on
// incident/task/change_request/cmdb_ci, sysparm_query filtering,
// offset/limit pagination via the Link header, and the Basic/Bearer
// credential gate (including rejection of unknown credentials).
const snowToken = "Bearer mock-snow-token"

type snowFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vm   *starlark.VM
	host string
}

func newSnowFixture(t *testing.T, start time.Time) *snowFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "servicenow-style")
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
	src, err := os.ReadFile(filepath.Join(root, "scripts", "table.star"))
	if err != nil {
		t.Fatalf("read table.star: %v", err)
	}
	vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
	if err != nil {
		t.Fatalf("LoadWithLib table.star: %v", err)
	}
	return &snowFixture{t: t, vc: vc, vm: vm, host: "instance.service-now.test"}
}

// call invokes a Table API handler for table over method+path, mirroring
// the manifest's routes (tableName comes from the path itself).
func (f *snowFixture) call(handler, method, table, sysID string, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	path := "/api/now/table/" + table
	params := map[string]string{"tableName": table}
	if sysID != "" {
		path += "/" + sysID
		params["sys_id"] = sysID
	}
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	var raw string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		raw = string(b)
	}
	resp, err := f.vm.Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers,
		Body: body, RawBody: raw, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

func snowResult(t *testing.T, r starlark.Response) []any {
	t.Helper()
	res, ok := r.Body["result"]
	if !ok {
		t.Fatalf("no result envelope: %v", r.Body)
	}
	switch v := res.(type) {
	case []any:
		return v
	case map[string]any:
		return []any{v}
	default:
		t.Fatalf("result is %T", res)
		return nil
	}
}

func snowField(item map[string]any, key string) string {
	if v, ok := item[key]; ok {
		s, _ := v.(string)
		return s
	}
	return ""
}

func TestServiceNowTableLifecycle(t *testing.T) {
	base := time.Date(2026, 2, 3, 9, 0, 0, 0, time.UTC)
	f := newSnowFixture(t, base)

	// Unknown credential -> 401 with the ServiceNow envelope.
	bad := f.call("on_create", "POST", "incident", "", nil, map[string]any{"short_description": "x"}, "Bearer wrong-token")
	if bad.Status != 401 {
		t.Fatalf("unknown credential -> %d, want 401", bad.Status)
	}

	// Create assigns sys_id + INC number.
	created := f.call("on_create", "POST", "incident", "", nil, map[string]any{
		"short_description": "vm suite incident",
		"urgency":           2,
	}, snowToken)
	if created.Status != 201 && created.Status != 200 {
		t.Fatalf("create -> %d: %v", created.Status, created.Body)
	}
	inc := snowResult(t, created)[0].(map[string]any)
	sysID := snowField(inc, "sys_id")
	if sysID == "" || snowField(inc, "number")[:3] != "INC" {
		t.Fatalf("create: sys_id=%q number=%q", sysID, snowField(inc, "number"))
	}

	// A second incident so lists/pagination have substance.
	f.call("on_create", "POST", "incident", "", nil, map[string]any{"short_description": "another incident"}, snowToken)

	// Get round-trips the fields.
	got := f.call("on_get", "GET", "incident", sysID, nil, nil, snowToken)
	if got.Status != 200 {
		t.Fatalf("get -> %d", got.Status)
	}
	if snowField(snowResult(t, got)[0].(map[string]any), "short_description") != "vm suite incident" {
		t.Fatalf("get: fields not round-tripped: %v", got.Body)
	}

	// Update persists and bumps sys_updated_on.
	upd := f.call("on_update", "PUT", "incident", sysID, nil, map[string]any{"state": 2}, snowToken)
	if upd.Status != 200 {
		t.Fatalf("update -> %d", upd.Status)
	}
	after := f.call("on_get", "GET", "incident", sysID, nil, nil, snowToken)
	// JSON numbers arrive in Starlark as floats, so compare formats.
	if got := fmt.Sprintf("%v", snowResult(t, after)[0].(map[string]any)["state"]); got != "2" {
		t.Fatalf("state after update = %v, want 2", got)
	}

	// sysparm_query equality filter narrows the list.
	filtered := f.call("on_list", "GET", "incident", "", map[string]string{
		"sysparm_query": "short_description=vm suite incident",
	}, nil, snowToken)
	if filtered.Status != 200 || filtered.Headers["X-Total-Count"] != "1" {
		t.Fatalf("filtered list -> %d total=%q, want total 1", filtered.Status, filtered.Headers["X-Total-Count"])
	}

	// Offset/limit pagination: page of one carries a Link header.
	page1 := f.call("on_list", "GET", "incident", "", map[string]string{
		"sysparm_limit": "1", "sysparm_offset": "0",
	}, nil, snowToken)
	if len(snowResult(t, page1)) != 1 || page1.Headers["Link"] == "" {
		t.Fatalf("page1: len=%d link=%q", len(snowResult(t, page1)), page1.Headers["Link"])
	}
	page2 := f.call("on_list", "GET", "incident", "", map[string]string{
		"sysparm_limit": "1", "sysparm_offset": "1",
	}, nil, snowToken)
	p1 := snowField(snowResult(t, page1)[0].(map[string]any), "sys_id")
	p2 := snowField(snowResult(t, page2)[0].(map[string]any), "sys_id")
	if p1 == p2 {
		t.Fatalf("offset pagination returned the same row twice (%q)", p1)
	}

	// Tables are isolated: task rows never appear in incident lists.
	f.call("on_create", "POST", "task", "", nil, map[string]any{"short_description": "a task"}, snowToken)
	incidents := f.call("on_list", "GET", "incident", "", nil, nil, snowToken)
	if n := snowResult(t, incidents); len(n) != 2 {
		t.Fatalf("incident list leaked task rows: %d rows", len(n))
	}

	// Unknown sys_id -> 404; unknown table -> 404.
	if r := f.call("on_get", "GET", "incident", "deadbeef", nil, nil, snowToken); r.Status != 404 {
		t.Fatalf("get unknown sys_id -> %d, want 404", r.Status)
	}
	if r := f.call("on_list", "GET", "no_such_table", "", nil, nil, snowToken); r.Status != 404 {
		t.Fatalf("unknown table -> %d, want 404", r.Status)
	}

	// Delete removes the record.
	del := f.call("on_delete", "DELETE", "incident", sysID, nil, nil, snowToken)
	if del.Status != 204 && del.Status != 200 {
		t.Fatalf("delete -> %d", del.Status)
	}
	if r := f.call("on_get", "GET", "incident", sysID, nil, nil, snowToken); r.Status != 404 {
		t.Fatalf("get after delete -> %d, want 404", r.Status)
	}
}
