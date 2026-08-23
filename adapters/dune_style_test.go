package adapters

import (
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

// Drives the dune-style adapter scripts directly (lib.star preloaded) over a
// shared store and virtual clock: the API-key gate, the derive-on-read
// execution lifecycle (PENDING -> EXECUTING -> COMPLETED / FAILED), the
// query-parameter model behind the 400 envelope, results paging with the
// followable next_uri, the CSV variant, and the inline-result route.
const duneAuth = "Bearer dune-api-key"

type duneFixture struct {
	t   *testing.T
	vc  *clock.Clock
	vms map[string]*starlark.VM
}

func newDuneFixture(t *testing.T, start time.Time) *duneFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "dune-style")
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
	return &duneFixture{t: t, vc: vc, vms: map[string]*starlark.VM{
		"exec": load("execute.star"), "auth": load("auth.star"),
	}}
}

func (f *duneFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	return f.callHdrs(group, handler, method, path, params, query, body, auth, nil)
}

// callHdrs is call plus extra request headers (e.g. X-Forwarded-Proto for a
// request that arrived through a TLS proxy).
func (f *duneFixture) callHdrs(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string, extra map[string]string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	for k, v := range extra {
		headers[k] = v
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: "api.dune.test", Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// execute POSTs an execution for queryID and returns the landed execution_id.
func (f *duneFixture) execute(queryID string, body map[string]any) string {
	f.t.Helper()
	r := f.call("exec", "on_execute", "POST", "/api/v1/query/"+queryID+"/execute",
		map[string]string{"query_id": queryID}, nil, body, duneAuth)
	if r.Status != 200 {
		f.t.Fatalf("execute %s -> %d: %v", queryID, r.Status, r.Body)
	}
	id, _ := r.Body["execution_id"].(string)
	if id == "" {
		f.t.Fatalf("execute %s: no execution_id: %v", queryID, r.Body)
	}
	return id
}

func (f *duneFixture) status(execID string) map[string]any {
	f.t.Helper()
	r := f.call("exec", "on_get_status", "GET", "/api/v1/execution/"+execID+"/status",
		map[string]string{"execution_id": execID}, nil, nil, duneAuth)
	if r.Status != 200 {
		f.t.Fatalf("status %s -> %d: %v", execID, r.Status, r.Body)
	}
	return r.Body
}

func (f *duneFixture) results(execID string, query map[string]string) starlark.Response {
	f.t.Helper()
	return f.call("exec", "on_get_results", "GET", "/api/v1/execution/"+execID+"/results",
		map[string]string{"execution_id": execID}, query, nil, duneAuth)
}

func TestDuneAuthAndExecutionLifecycle(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newDuneFixture(t, base)

	// ===== the api-key gate rejects missing, empty and non-bearer authorization =====
	// The gate is presence-only (any non-empty bearer validates, per the
	// manifest's uniform token_scheme); everything else is the 401 envelope.
	if r := f.call("auth", "on_validate", "GET", "/api/v1/auth/validate", nil, nil, nil, ""); r.Status != 401 || r.Body["error"] != "Invalid API key" {
		t.Fatalf("validate without auth -> %d %v, want 401 {error: Invalid API key}", r.Status, r.Body)
	}
	if r := f.call("auth", "on_validate", "GET", "/api/v1/auth/validate", nil, nil, nil, "Bearer "); r.Status != 401 {
		t.Fatalf("validate with empty bearer -> %d, want 401", r.Status)
	}
	if r := f.call("auth", "on_validate", "GET", "/api/v1/auth/validate", nil, nil, nil, "Basic something"); r.Status != 401 {
		t.Fatalf("validate with non-bearer auth -> %d, want 401", r.Status)
	}
	ok := f.call("auth", "on_validate", "GET", "/api/v1/auth/validate", nil, nil, nil, "Bearer any-token-at-all")
	if ok.Status != 200 || ok.Body["valid"] != true {
		t.Fatalf("validate with any bearer -> %d %v, want 200 {valid: true}", ok.Status, ok.Body)
	}

	// ===== an execution walks PENDING -> EXECUTING -> COMPLETED as the clock advances =====
	// Results stay 404 until the poll derives completion; the stamps and the
	// 35-day result retention are derived from the injectable clock.
	execID := f.execute("12345", map[string]any{"query_parameters": map[string]any{}})
	atStart := f.status(execID)
	if atStart["state"] != "QUERY_STATE_PENDING" || atStart["is_execution_finished"] != false {
		t.Fatalf("status at t=0 = %v", atStart)
	}
	if atStart["execution_started_at"] != nil || atStart["execution_ended_at"] != nil {
		t.Fatalf("pending stamps = %v / %v, want null/null", atStart["execution_started_at"], atStart["execution_ended_at"])
	}
	duneNum(t, atStart["query_id"], 12345, "pending query_id")
	if r := f.results(execID, nil); r.Status != 404 || r.Body["error"] != "Execution is not completed yet" {
		t.Fatalf("results while running -> %d %v, want 404 not-completed-yet", r.Status, r.Body)
	}

	f.vc.Advance(2 * time.Second) // 2s: inside the 1s..3s executing window
	running := f.status(execID)
	if running["state"] != "QUERY_STATE_EXECUTING" || running["is_execution_finished"] != false {
		t.Fatalf("status at 2s = %v, want EXECUTING", running)
	}
	if got, _ := running["execution_started_at"].(string); got != base.Add(1*time.Second).Format(time.RFC3339) {
		t.Fatalf("executing execution_started_at = %v, want %s", running["execution_started_at"], base.Add(1*time.Second).Format(time.RFC3339))
	}
	if running["execution_ended_at"] != nil {
		t.Fatalf("executing execution_ended_at = %v, want null", running["execution_ended_at"])
	}

	f.vc.Advance(2 * time.Second) // 4s: past the 3s window
	done := f.status(execID)
	if done["state"] != "QUERY_STATE_COMPLETED" || done["is_execution_finished"] != true {
		t.Fatalf("status at 4s = %v, want COMPLETED / finished", done)
	}
	if got, _ := done["execution_ended_at"].(string); got != base.Add(3*time.Second).Format(time.RFC3339) {
		t.Fatalf("completed execution_ended_at = %v, want %s", done["execution_ended_at"], base.Add(3*time.Second).Format(time.RFC3339))
	}
	if got, _ := done["expires_at"].(string); got != base.Add(3*time.Second).Add(35*24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("completed expires_at = %v, want finish + 35d retention", done["expires_at"])
	}
	if r := f.results(execID, nil); r.Status != 200 {
		t.Fatalf("results after completion -> %d: %v", r.Status, r.Body)
	}

	// ===== a missing required parameter is the 400 envelope and both SDK parameter shapes resolve =====
	// Query 4242 declares wallet_address as required TEXT; Dune accepts both
	// the plain value and the SDK {type, value} object for it.
	if r := f.call("exec", "on_execute", "POST", "/api/v1/query/4242/execute",
		map[string]string{"query_id": "4242"}, nil, map[string]any{"query_parameters": map[string]any{}}, duneAuth); r.Status != 400 || r.Body["error"] != "Bad Request" {
		t.Fatalf("execute without required param -> %d %v, want 400 {error: Bad Request}", r.Status, r.Body)
	}
	sdkShape := f.execute("4242", map[string]any{"query_parameters": map[string]any{
		"wallet_address": map[string]any{"type": "TEXT", "value": "0xAbC123dEf456"}, "min_usd": 250,
	}})
	plainShape := f.execute("4242", map[string]any{"query_parameters": map[string]any{
		"wallet_address": "0xFeD987aBc321", "min_usd": 250,
	}})
	f.vc.Advance(4 * time.Second)
	resA := f.results(sdkShape, nil)
	resB := f.results(plainShape, nil)
	if resA.Status != 200 || resB.Status != 200 {
		t.Fatalf("parameterized results -> %d / %d", resA.Status, resB.Status)
	}
	duneNum(t, resA.Body["query_id"], 4242, "results query_id (integer)")
	rowsA := duneRows(t, resA)
	rowsB := duneRows(t, resB)
	if len(rowsA) != 8 || len(rowsB) != 8 {
		t.Fatalf("query 4242 rows = %d / %d, want 8 each", len(rowsA), len(rowsB))
	}
	if rowsA[0]["amount_usd"] == rowsB[0]["amount_usd"] {
		t.Fatalf("distinct wallet_address produced identical rows: %v", rowsA[0]["amount_usd"])
	}

	// ===== simulate_fail terminates QUERY_STATE_FAILED and results carry the failure envelope =====
	failID := f.execute("12345", map[string]any{"simulate_fail": true})
	f.vc.Advance(4 * time.Second)
	failed := f.status(failID)
	if failed["state"] != "QUERY_STATE_FAILED" || failed["is_execution_finished"] != true {
		t.Fatalf("failed status = %v, want QUERY_STATE_FAILED / finished", failed)
	}
	if r := f.results(failID, nil); r.Status != 404 || r.Body["error"] != "Execution failed; no results are available" {
		t.Fatalf("failed results -> %d %v, want the no-results 404", r.Status, r.Body)
	}
	if r := f.call("exec", "on_get_results_csv", "GET", "/api/v1/execution/"+failID+"/results/csv",
		map[string]string{"execution_id": failID}, nil, nil, duneAuth); r.Status != 404 {
		t.Fatalf("failed csv -> %d, want 404", r.Status)
	}
	// An unknown execution id answers the object-not-found 404 on both reads.
	for _, h := range []string{"on_get_status", "on_get_results"} {
		if r := f.call("exec", h, "GET", "/api/v1/execution/nope/"+map[string]string{
			"on_get_status": "status", "on_get_results": "results",
		}[h], map[string]string{"execution_id": "nope"}, nil, nil, duneAuth); r.Status != 404 || r.Body["error"] != "Object not found" {
			t.Fatalf("%s unknown execution -> %d %v, want 404 {error: Object not found}", h, r.Status, r.Body)
		}
	}

	// ===== the inline-result route completes synchronously =====
	// POST result returns the terminal envelope immediately, without a poll.
	r := f.call("exec", "on_inline_result", "POST", "/api/v1/query/3971/result",
		map[string]string{"query_id": "3971"}, nil, map[string]any{
			"query_parameters": map[string]any{"token_symbol": "WETH"},
		}, duneAuth)
	if r.Status != 200 || r.Body["state"] != "QUERY_STATE_COMPLETED" || r.Body["is_execution_finished"] != true {
		t.Fatalf("inline result -> %d %v, want COMPLETED / finished", r.Status, r.Body)
	}
	if r.Body["next_uri"] != nil || r.Body["next_offset"] != nil {
		t.Fatalf("inline next_uri/next_offset = %v / %v, want null (single page)", r.Body["next_uri"], r.Body["next_offset"])
	}
	inlineRows := duneRows(t, r)
	if len(inlineRows) != 12 {
		t.Fatalf("inline rows = %d, want 12 (query 3971)", len(inlineRows))
	}
	if inlineRows[0]["token_symbol"] != "WETH" {
		t.Fatalf("inline token_symbol = %v, want WETH (parameter substituted)", inlineRows[0]["token_symbol"])
	}
}

func TestDuneResultsPagingAndCSV(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newDuneFixture(t, base)

	execID := f.execute("3971", map[string]any{"query_parameters": map[string]any{"token_symbol": "WETH"}})
	f.vc.Advance(4 * time.Second) // complete the execution

	// ===== results pages honor limit/offset with a followable next_uri =====
	// Page metadata describes the returned page; total_* the full set; the
	// continuation URL is absolute on the request host so clients follow it.
	p1 := f.results(execID, map[string]string{"limit": "5"})
	if p1.Status != 200 {
		t.Fatalf("page 1 -> %d: %v", p1.Status, p1.Body)
	}
	rows1 := duneRows(t, p1)
	meta1 := duneMeta(t, p1)
	if len(rows1) != 5 {
		t.Fatalf("page 1 rows = %d, want 5 (limit honored)", len(rows1))
	}
	if rows1[0]["token_symbol"] != "WETH" {
		t.Fatalf("page 1 token_symbol = %v, want WETH", rows1[0]["token_symbol"])
	}
	duneNum(t, meta1["row_count"], 5, "page 1 row_count")
	duneNum(t, meta1["total_row_count"], 12, "page 1 total_row_count")
	duneNum(t, meta1["datapoint_count"], 48, "page 1 datapoint_count (12 rows x 4 columns)")
	cols, _ := meta1["column_names"].([]any)
	if len(cols) != 4 {
		t.Fatalf("column_names = %v, want the 4 result columns", meta1["column_names"])
	}
	duneNum(t, p1.Body["next_offset"], 5, "page 1 next_offset")
	nextURI, _ := p1.Body["next_uri"].(string)
	if !strings.Contains(nextURI, "/api/v1/execution/"+execID+"/results?offset=5&limit=5") {
		t.Fatalf("page 1 next_uri = %q, want the offset=5&limit=5 continuation", nextURI)
	}
	if !strings.HasPrefix(nextURI, "http://api.dune.test/") {
		t.Fatalf("page 1 next_uri = %q, want absolute http on the request host", nextURI)
	}
	// Behind a TLS proxy the forwarded proto keeps the continuation followable.
	proxied := f.callHdrs("exec", "on_get_results", "GET", "/api/v1/execution/"+execID+"/results",
		map[string]string{"execution_id": execID}, map[string]string{"limit": "5"},
		nil, duneAuth, map[string]string{"X-Forwarded-Proto": "https"})
	proxiedURI, _ := proxied.Body["next_uri"].(string)
	if !strings.HasPrefix(proxiedURI, "https://api.dune.test/") {
		t.Fatalf("proxied next_uri = %q, want the forwarded https scheme", proxiedURI)
	}

	p2 := f.results(execID, map[string]string{"limit": "5", "offset": "5"})
	rows2 := duneRows(t, p2)
	if len(rows2) != 5 {
		t.Fatalf("page 2 rows = %d, want 5 (offset honored)", len(rows2))
	}
	if rows2[0]["block_time"] == rows1[0]["block_time"] {
		t.Fatalf("page 2 repeated page 1's first row (offset not applied)")
	}
	p3 := f.results(execID, map[string]string{"limit": "5", "offset": "10"})
	rows3 := duneRows(t, p3)
	if len(rows3) != 2 || p3.Body["next_uri"] != nil || p3.Body["next_offset"] != nil {
		t.Fatalf("final page = %d rows, next %v/%v, want 2 rows and no continuation", len(rows3), p3.Body["next_uri"], p3.Body["next_offset"])
	}

	// Same parameters reproduce the same rows: paging never re-rolls the data.
	again := f.execute("3971", map[string]any{"query_parameters": map[string]any{"token_symbol": "WETH"}})
	f.vc.Advance(4 * time.Second)
	replay := duneRows(t, f.results(again, nil))
	if len(replay) != 12 || replay[0]["amount_usd"] != rows1[0]["amount_usd"] {
		t.Fatalf("re-executed rows differ: %v vs %v", replay[0]["amount_usd"], rows1[0]["amount_usd"])
	}

	// ===== the CSV variant streams text/csv for the same page =====
	csv := f.call("exec", "on_get_results_csv", "GET", "/api/v1/execution/"+execID+"/results/csv",
		map[string]string{"execution_id": execID}, map[string]string{"limit": "3"}, nil, duneAuth)
	if csv.Status != 200 {
		t.Fatalf("csv -> %d", csv.Status)
	}
	if ct, _ := csv.Headers["Content-Type"]; ct != "text/csv" {
		t.Fatalf("csv Content-Type = %q, want text/csv", ct)
	}
	lines := strings.Split(strings.TrimSpace(csv.RawBody), "\n")
	if len(lines) != 4 {
		t.Fatalf("csv lines = %d, want 4 (header + 3 rows honoring limit); body %q", len(lines), csv.RawBody)
	}
	if lines[0] != "block_time,protocol,amount_usd,token_symbol" {
		t.Fatalf("csv header = %q, want the result column names", lines[0])
	}
	if !strings.Contains(lines[1], "WETH") {
		t.Fatalf("csv row 1 = %q, want WETH substitution", lines[1])
	}
	if r := f.call("exec", "on_get_results_csv", "GET", "/api/v1/execution/unknown/csv",
		map[string]string{"execution_id": "unknown"}, nil, nil, duneAuth); r.Status != 404 || r.Body["error"] != "Object not found" {
		t.Fatalf("csv unknown execution -> %d %v, want 404 Object not found", r.Status, r.Body)
	}
}

// duneRows pulls result.rows out of a results envelope.
func duneRows(t *testing.T, r starlark.Response) []map[string]any {
	t.Helper()
	result, ok := r.Body["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want object", r.Body["result"])
	}
	raw, _ := result["rows"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for i, row := range raw {
		m, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("rows[%d] is %T, want object", i, row)
		}
		out = append(out, m)
	}
	return out
}

// duneMeta pulls result.metadata out of a results envelope.
func duneMeta(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	result, ok := r.Body["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want object", r.Body["result"])
	}
	meta, ok := result["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %v, want object", result["metadata"])
	}
	return meta
}

// duneNum compares a JSON number regardless of int64/float64 width (stamps
// round-trip through the collection store, where ints come back floats).
func duneNum(t *testing.T, v any, want float64, what string) {
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
		t.Fatalf("%s is %T(%v), want number %v", what, v, v, want)
	}
}
