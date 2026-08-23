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

// Drives the workday-style adapter scripts directly (lib.star preloaded)
// over a shared store: the Bearer/Basic presence gate, the staffing worker
// read surfaces, RaaS Create_Worker/Custom_Report, search-before-paging,
// limit/offset paging, the compensation shelf, and the Workday error
// envelope. Collections start empty in this harness (fixture seeds load
// only under the engine — see internal/engine/workday_style_test.go), so
// worker state is built through Create_Worker like any client would.
const wdBearer = "Bearer mock-workday-token"

type workdayFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newWorkdayFixture(t *testing.T, start time.Time) *workdayFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "workday-style")
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
	return &workdayFixture{t: t, vc: vc, host: "services.myworkday.test", vms: map[string]*starlark.VM{
		"workers": load("workers.star"), "comp": load("compensation.star"),
		"payroll": load("payroll.star"), "hr": load("hr.star"),
		"fin": load("financials.star"), "raas": load("raas.star"),
	}}
}

// call invokes a workday handler for one of the manifest's routes; params
// mirrors the route's path parameters ({id}, {tenant}).
func (f *workdayFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
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

// wdCreateWorker POSTs the RaaS Create_Worker custom endpoint and returns
// the created worker body (the only write surface in this adapter).
func (f *workdayFixture) wdCreateWorker(name, email string) map[string]any {
	f.t.Helper()
	r := f.call("raas", "on_create_worker", "POST", "/ccx/v1/acme/staffing/Create_Worker",
		map[string]string{"tenant": "acme"}, nil, map[string]any{
			"Worker_Name": name, "Primary_Work_Email": email,
		}, wdBearer)
	if r.Status != 200 {
		f.t.Fatalf("Create_Worker %q -> %d: %v", name, r.Status, r.Body)
	}
	return r.Body
}

// wdData unwraps the {data:[...]} list envelope.
func wdData(t *testing.T, r starlark.Response) []any {
	t.Helper()
	d, ok := r.Body["data"].([]any)
	if !ok {
		t.Fatalf("no data envelope: %v", r.Body)
	}
	return d
}

// wdTotal reads the list envelope's total. Starlark ints arrive as int64;
// numbers that round-tripped through JSON arrive as float64.
func wdTotal(t *testing.T, r starlark.Response) int {
	t.Helper()
	switch v := r.Body["total"].(type) {
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		t.Fatalf("total is %T: %v", r.Body["total"], r.Body["total"])
		return 0
	}
}

// wdErrCode reads errors[0].errorCode from a Workday error envelope.
func wdErrCode(t *testing.T, r starlark.Response) string {
	t.Helper()
	errs, ok := r.Body["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("no errors envelope: %v", r.Body)
	}
	e, _ := errs[0].(map[string]any)
	code, _ := e["errorCode"].(string)
	return code
}

func wdField(item map[string]any, key string) string {
	if v, ok := item[key]; ok {
		s, _ := v.(string)
		return s
	}
	return ""
}

func TestWorkdayWorkersAndReports(t *testing.T) {
	base := time.Date(2026, 2, 3, 8, 0, 0, 0, time.UTC)
	f := newWorkdayFixture(t, base)

	// ===== requests without a Bearer or Basic scheme are 401 at the gate =====
	// The gate checks scheme presence only, so a missing header or an
	// unknown scheme is a 401 in the shared errors[] envelope.
	if r := f.call("workers", "on_list_workers", "GET", "/wbs/v40.0/staffing/workers", nil, nil, nil, ""); r.Status != 401 || wdErrCode(t, r) != "AUTHENTICATION_FAILED" {
		t.Fatalf("no auth -> %d %v, want 401 AUTHENTICATION_FAILED", r.Status, r.Body)
	}
	if r := f.call("workers", "on_list_workers", "GET", "/wbs/v40.0/staffing/workers", nil, nil, nil, "Token abc"); r.Status != 401 || wdErrCode(t, r) != "AUTHENTICATION_FAILED" {
		t.Fatalf("unknown scheme -> %d %v, want 401 AUTHENTICATION_FAILED", r.Status, r.Body)
	}

	// ===== both Bearer and Basic credentials clear the gate =====
	// Either documented scheme is accepted; the credential value itself is
	// never validated (documented mock behavior).
	if r := f.call("workers", "on_list_workers", "GET", "/wbs/v40.0/staffing/workers", nil, nil, nil, "Basic dXNlcjpwYXNz"); r.Status != 200 {
		t.Fatalf("basic auth list -> %d: %v", r.Status, r.Body)
	}

	// ===== Create_Worker mints an id that reads back through staffing =====
	// The RaaS create is the only write surface; the minted worker must be
	// readable from the REST staffing get with its fields round-tripped.
	ada := f.wdCreateWorker("Ada Lovelace (alovelace)", "ada@corp.test")
	adaID := wdField(ada, "id")
	if adaID == "" {
		t.Fatalf("Create_Worker returned no id: %v", ada)
	}
	if ref, _ := ada["workerID"].(map[string]any); wdField(ref, "id") != adaID {
		t.Fatalf("workerID.id = %v, want %q", ada["workerID"], adaID)
	}
	got := f.call("workers", "on_get_worker", "GET", "/wbs/v40.0/staffing/workers/"+adaID,
		map[string]string{"id": adaID}, nil, nil, wdBearer)
	if got.Status != 200 || wdField(got.Body, "descriptor") != "Ada Lovelace (alovelace)" || wdField(got.Body, "primaryWorkEmail") != "ada@corp.test" {
		t.Fatalf("get worker %s -> %d %v", adaID, got.Status, got.Body)
	}

	// ===== the RaaS custom report wraps rows in Report_Entry =====
	// RaaS wraps rows in "Report_Entry"; the rows are static and do not
	// reflect workers created at runtime (asserted as-is).
	rep := f.call("raas", "on_custom_report", "GET", "/ccx/v1/acme/RaaS/Custom_Report",
		map[string]string{"tenant": "acme"}, nil, nil, wdBearer)
	entries, ok := rep.Body["Report_Entry"].([]any)
	if rep.Status != 200 || !ok || len(entries) != 3 {
		t.Fatalf("custom report -> %d %v, want 200 with 3 rows", rep.Status, rep.Body)
	}
	row0, _ := entries[0].(map[string]any)
	if wdField(row0, "WorkerName") != "John Smith" || wdField(row0, "Status") != "Active" {
		t.Fatalf("report row0 = %v, want John Smith/Active", row0)
	}

	// ===== search narrows the worker list before paging =====
	// search is a case-insensitive partial match over descriptor, email and
	// worker id, applied before limit/offset; total reflects the filter.
	f.wdCreateWorker("Grace Hopper (ghopper)", "grace@corp.test")
	f.wdCreateWorker("Alan Turing (aturing)", "alan@corp.test")
	byName := f.call("workers", "on_list_workers", "GET", "/wbs/v40.0/staffing/workers",
		nil, map[string]string{"search": "LOVELACE"}, nil, wdBearer)
	if byName.Status != 200 || wdTotal(t, byName) != 1 || len(wdData(t, byName)) != 1 ||
		wdField(wdData(t, byName)[0].(map[string]any), "id") != adaID {
		t.Fatalf("search LOVELACE -> %d %v, want only %s", byName.Status, byName.Body, adaID)
	}
	byEmailPage := f.call("workers", "on_list_workers", "GET", "/wbs/v40.0/staffing/workers",
		nil, map[string]string{"search": "corp.test", "limit": "2"}, nil, wdBearer)
	if wdTotal(t, byEmailPage) != 3 || len(wdData(t, byEmailPage)) != 2 || byEmailPage.Body["more"] != true {
		t.Fatalf("search+limit -> total=%d len=%d more=%v, want 3/2/true",
			wdTotal(t, byEmailPage), len(wdData(t, byEmailPage)), byEmailPage.Body["more"])
	}
	if r := f.call("workers", "on_list_workers", "GET", "/wbs/v40.0/staffing/workers",
		nil, map[string]string{"search": "no-such-person"}, nil, wdBearer); wdTotal(t, r) != 0 || len(wdData(t, r)) != 0 {
		t.Fatalf("search no match -> %v, want empty page", r.Body)
	}

	// ===== limit/offset pages tile the list without repeats =====
	// Pages must tile the list exactly, total stays the full count, and a
	// page starting past the end is empty with more=false.
	page1 := f.call("workers", "on_list_workers", "GET", "/wbs/v40.0/staffing/workers",
		nil, map[string]string{"limit": "2", "offset": "0"}, nil, wdBearer)
	page2 := f.call("workers", "on_list_workers", "GET", "/wbs/v40.0/staffing/workers",
		nil, map[string]string{"limit": "2", "offset": "2"}, nil, wdBearer)
	if len(wdData(t, page1)) != 2 || page1.Body["more"] != true {
		t.Fatalf("page1 -> len=%d more=%v, want 2/true", len(wdData(t, page1)), page1.Body["more"])
	}
	if len(wdData(t, page2)) != 1 || page2.Body["more"] != false {
		t.Fatalf("page2 -> len=%d more=%v, want 1/false", len(wdData(t, page2)), page2.Body["more"])
	}
	seen := map[string]bool{}
	for _, w := range append(wdData(t, page1), wdData(t, page2)...) {
		seen[wdField(w.(map[string]any), "id")] = true
	}
	if len(seen) != 3 {
		t.Fatalf("pages covered %d distinct workers, want 3", len(seen))
	}
	past := f.call("workers", "on_list_workers", "GET", "/wbs/v40.0/staffing/workers",
		nil, map[string]string{"limit": "2", "offset": "99"}, nil, wdBearer)
	if len(wdData(t, past)) != 0 || wdTotal(t, past) != 3 || past.Body["more"] != false {
		t.Fatalf("past-the-end page -> %v, want empty/total 3/more false", past.Body)
	}

	// ===== compensation serves the seed shelf and empty for created workers =====
	// Compensation is a static shelf keyed to seed worker ids 1-3; created
	// workers read back empty, and unknown ids do NOT 404 (asserted as-is;
	// real Workday rejects unknown workers on subresources).
	shelf := f.call("comp", "on_worker_compensation", "GET", "/wbs/v40.0/compensation/workers/1/compensation",
		map[string]string{"id": "1"}, nil, nil, wdBearer)
	if shelf.Status != 200 || wdTotal(t, shelf) != 2 {
		t.Fatalf("compensation shelf -> %d %v, want 200 with 2 components", shelf.Status, shelf.Body)
	}
	comp0, _ := wdData(t, shelf)[0].(map[string]any)
	component, _ := comp0["compensationComponent"].(map[string]any)
	if wdField(component, "descriptor") != "Base Salary" || wdField(comp0, "amount") != "145000.00" {
		t.Fatalf("compensation row0 = %v, want Base Salary 145000.00", comp0)
	}
	mine := f.call("comp", "on_worker_compensation", "GET", "/wbs/v40.0/compensation/workers/"+adaID+"/compensation",
		map[string]string{"id": adaID}, nil, nil, wdBearer)
	if mine.Status != 200 || wdTotal(t, mine) != 0 || len(wdData(t, mine)) != 0 {
		t.Fatalf("compensation for created worker -> %d %v, want 200 empty", mine.Status, mine.Body)
	}
	if r := f.call("comp", "on_worker_compensation", "GET", "/wbs/v40.0/compensation/workers/999/compensation",
		map[string]string{"id": "999"}, nil, nil, wdBearer); r.Status != 200 || wdTotal(t, r) != 0 {
		t.Fatalf("compensation for unknown worker -> %d %v, want 200 empty (no 404)", r.Status, r.Body)
	}

	// ===== payroll, positions, and financials share the list envelope =====
	// The remaining collection-backed routes expose the same {data,total,
	// more} envelope; they start empty in this harness (seeds load under
	// the engine, exercised by internal/engine/workday_style_test.go).
	for _, tc := range []struct{ group, handler, path string }{
		{"payroll", "on_pay_components", "/wbs/v40.0/payroll/pay_components"},
		{"hr", "on_positions", "/wbs/v40.0/human_resources/positions"},
		{"fin", "on_accounts", "/wbs/v40.0/financials/accounts"},
	} {
		r := f.call(tc.group, tc.handler, "GET", tc.path, nil, nil, nil, wdBearer)
		if r.Status != 200 || wdTotal(t, r) != 0 {
			t.Fatalf("%s -> %d %v, want 200 with empty envelope", tc.path, r.Status, r.Body)
		}
		if _, ok := r.Body["more"]; !ok {
			t.Fatalf("%s: no more flag: %v", tc.path, r.Body)
		}
	}

	// ===== an unknown worker is a 404 with the Workday error envelope =====
	// Reads of unknown ids 404 through the shared errors[] envelope.
	if r := f.call("workers", "on_get_worker", "GET", "/wbs/v40.0/staffing/workers/9999",
		map[string]string{"id": "9999"}, nil, nil, wdBearer); r.Status != 404 || wdErrCode(t, r) != "RESOURCE_NOT_FOUND" {
		t.Fatalf("unknown worker -> %d %v, want 404 RESOURCE_NOT_FOUND", r.Status, r.Body)
	}
}
