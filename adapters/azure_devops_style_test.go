package adapters

import (
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

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// Drives the azure-devops-style adapter scripts directly (lib.star preloaded)
// over a shared store and a VIRTUAL clock: the PAT auth gate, project
// continuationToken paging, the work-item patch-document lifecycle plus the
// WIQL subset, git push/items/commits, the derive-on-read pipeline run
// lifecycle (advanced by the clock, no real waiting), and service-hook
// delivery to a live sink.
const (
	adoHost      = "dev.azure.com"
	adoPAT       = "testPAT"
	adoBasicPAT  = "Basic dGVzdFBBVDo=" // base64("testPAT:") — PAT as user, empty password
	adoBearerPAT = "Bearer " + adoPAT
	adoRepoID    = "11111111-0000-0000-0000-000000000001"
)

type adoFixture struct {
	t   *testing.T
	vc  *clock.Clock
	vms map[string]*starlark.VM
}

// adoDelivery is one recorded sink delivery: decoded body + header names.
type adoDelivery struct {
	body       map[string]any
	headerKeys []string
}

func newADOFixture(t *testing.T, start time.Time) *adoFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "azure-devops-style")
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
	return &adoFixture{t: t, vc: vc, vms: map[string]*starlark.VM{
		"projects": load("projects.star"), "pipelines": load("pipelines.star"),
		"git": load("git.star"), "wit": load("workitems.star"),
		"work": load("work.star"), "hooks": load("hooks.star"),
	}}
}

// call invokes one handler. params carries the route captures the engine
// extracts ({org}, {project}, {id}, ...); auth is a full Authorization header
// value ("" sends none).
func (f *adoFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	if params == nil {
		params = map[string]string{}
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: adoHost, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// adoParams builds route captures for a project-scoped route plus extra.
func adoParams(extra map[string]string) map[string]string {
	p := map[string]string{"org": "mock-org", "project": "MyFirstProject"}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// adoOps wraps patch-document operations the way the engine delivers a JSON
// array body to handlers ({"_batch": [...]}).
func adoOps(ops ...map[string]any) map[string]any {
	list := make([]any, len(ops))
	for i, op := range ops {
		list[i] = op
	}
	return map[string]any{"_batch": list}
}

// adoInt64 narrows a response number that may be int64 or float64 (client
// field values round-trip through the JSON store as floats).
func adoInt64(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	t.Fatalf("value %v (%T) is not a number", v, v)
	return 0
}

// adoBodyMap asserts the value is an object and returns it.
func adoBodyMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("value %v (%T) is not an object", v, v)
	}
	return m
}

// TestAzureDevOpsPATAuthGate: the PAT gate rejects missing, unknown, and
// non-PAT credentials with the Azure DevOps 401 envelope, and accepts the
// seeded PAT in both wire forms.
func TestAzureDevOpsPATAuthGate(t *testing.T) {
	f := newADOFixture(t, time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC))

	// ===== a missing, unknown, or non-PAT credential gets the UnauthorizedRequestException 401 envelope =====
	for _, auth := range []string{"", "Bearer bogusPAT", "Basic bm9wZQ==", "Token whatever"} {
		r := f.call("projects", "on_list_projects", "GET", "/mock-org/_apis/projects", nil, nil, nil, auth)
		if r.Status != 401 {
			t.Fatalf("auth %q -> %d, want 401", auth, r.Status)
		}
		if r.Body["typeName"] != "Microsoft.TeamFoundation.Framework.Server.UnauthorizedRequestException" {
			t.Fatalf("auth %q typeName = %v", auth, r.Body["typeName"])
		}
		if r.Body["eventId"] != int64(3000) {
			t.Fatalf("auth %q eventId = %v (%T), want 3000", auth, r.Body["eventId"], r.Body["eventId"])
		}
		if msg, _ := r.Body["message"].(string); !strings.HasPrefix(msg, "Access Denied:") {
			t.Fatalf("auth %q message = %q, want the Access Denied prefix", auth, msg)
		}
	}

	// ===== both PAT wire forms — Basic base64(PAT:) and Bearer — authenticate =====
	for _, auth := range []string{adoBasicPAT, adoBearerPAT} {
		r := f.call("projects", "on_list_projects", "GET", "/mock-org/_apis/projects", nil, nil, nil, auth)
		if r.Status != 200 {
			t.Fatalf("auth %q -> %d: %v", auth, r.Status, r.Body)
		}
		if r.Body["count"] != int64(2) {
			t.Fatalf("auth %q count = %v, want the 2 seeded projects", auth, r.Body["count"])
		}
	}
}

// TestAzureDevOpsProjectsAndContinuationPaging: the org-level projects list
// returns the seeded projects in the Azure DevOps shape, pages under
// OData $top/$skip with a continuationToken, and 400s on a bad token.
func TestAzureDevOpsProjectsAndContinuationPaging(t *testing.T) {
	f := newADOFixture(t, time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC))

	// ===== the projects list returns the two seeded projects in the Azure DevOps shape =====
	r := f.call("projects", "on_list_projects", "GET", "/mock-org/_apis/projects",
		map[string]string{"org": "mock-org"}, nil, nil, adoBasicPAT)
	if r.Status != 200 {
		t.Fatalf("projects -> %d: %v", r.Status, r.Body)
	}
	value, _ := r.Body["value"].([]any)
	if len(value) != 2 || r.Body["count"] != int64(2) {
		t.Fatalf("projects = %d items, count %v, want 2/2", len(value), r.Body["count"])
	}
	if _, has := r.Body["continuationToken"]; has {
		t.Fatalf("unpaged list carried a continuationToken: %v", r.Body)
	}
	var proj map[string]any
	for _, v := range value {
		if adoBodyMap(t, v)["name"] == "MyFirstProject" {
			proj = adoBodyMap(t, v)
		}
	}
	if proj == nil {
		t.Fatalf("MyFirstProject missing from %v", value)
	}
	if proj["id"] != "00000000-0000-0000-0000-000000000001" ||
		proj["state"] != "wellFormed" || proj["visibility"] != "private" {
		t.Fatalf("project envelope = %v", proj)
	}
	if proj["revision"] != int64(1) {
		t.Fatalf("project revision = %v (%T), want int64 1 (store floats must be coerced)", proj["revision"], proj["revision"])
	}
	if !strings.HasPrefix(proj["url"].(string), "https://dev.azure.com/mock-org/_apis/projects/") {
		t.Fatalf("project url = %v", proj["url"])
	}

	// ===== $top/$skip walk pages each project exactly once via continuationToken, and a bad token 400s =====
	p1 := f.call("projects", "on_list_projects", "GET", "/mock-org/_apis/projects",
		map[string]string{"org": "mock-org"}, map[string]string{"$top": "1"}, nil, adoBasicPAT)
	if p1.Status != 200 || len(p1.Body["value"].([]any)) != 1 {
		t.Fatalf("page 1 -> %d %v", p1.Status, p1.Body)
	}
	if tok, _ := p1.Body["continuationToken"].(string); tok != "1" {
		t.Fatalf("page 1 continuationToken = %v, want \"1\"", p1.Body["continuationToken"])
	}
	p2 := f.call("projects", "on_list_projects", "GET", "/mock-org/_apis/projects",
		map[string]string{"org": "mock-org"}, map[string]string{"$top": "1", "$skip": "1"}, nil, adoBasicPAT)
	if p2.Status != 200 || len(p2.Body["value"].([]any)) != 1 {
		t.Fatalf("page 2 -> %d %v", p2.Status, p2.Body)
	}
	if _, has := p2.Body["continuationToken"]; has {
		t.Fatalf("final page carried a continuationToken: %v", p2.Body)
	}
	seen := map[string]int{
		adoBodyMap(t, p1.Body["value"].([]any)[0])["name"].(string): 1,
		adoBodyMap(t, p2.Body["value"].([]any)[0])["name"].(string): 1,
	}
	if len(seen) != 2 {
		t.Fatalf("paged walk covered %v, want both projects exactly once", seen)
	}

	bad := f.call("projects", "on_list_projects", "GET", "/mock-org/_apis/projects",
		map[string]string{"org": "mock-org"}, map[string]string{"$top": "1", "$skip": "not-a-token"}, nil, adoBasicPAT)
	if bad.Status != 400 || bad.Body["message"] != "Invalid continuation token." {
		t.Fatalf("bad $skip -> %d %v, want 400 Invalid continuation token.", bad.Status, bad.Body)
	}
}

// TestAzureDevOpsWorkItemPatchDocumentLifecycle: work items are created on
// the typed route from a JSON patch document (with area/iteration defaults
// and $-type normalization), updated with rev bumps and audit refreshes, and
// validated with the WorkItemTracking fault envelopes.
func TestAzureDevOpsWorkItemPatchDocumentLifecycle(t *testing.T) {
	base := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	f := newADOFixture(t, base)

	// ===== creating on the typed route applies the patch document and defaults area/iteration =====
	created := f.call("wit", "on_create_workitem", "POST", "/mock-org/MyFirstProject/_apis/wit/workitems/Task",
		adoParams(map[string]string{"type": "Task"}), nil, adoOps(
			map[string]any{"op": "add", "path": "/fields/System.Title", "value": "Write conformance suite"},
			map[string]any{"op": "add", "path": "/fields/System.Description", "value": "From the VM suite"},
			map[string]any{"op": "add", "path": "/fields/Microsoft.VSTS.Common.Priority", "value": 2},
		), adoBasicPAT)
	if created.Status != 200 {
		t.Fatalf("create work item -> %d: %v", created.Status, created.Body)
	}
	if id := adoInt64(t, created.Body["id"]); id != 2 {
		t.Fatalf("created id = %v, want 2 (seeded item is 1)", created.Body["id"])
	}
	if created.Body["rev"] != int64(1) {
		t.Fatalf("created rev = %v (%T), want int64 1", created.Body["rev"], created.Body["rev"])
	}
	fields := adoBodyMap(t, created.Body["fields"])
	for k, want := range map[string]any{
		"System.Title":         "Write conformance suite",
		"System.WorkItemType":  "Task",
		"System.State":         "New",
		"System.Reason":        "New",
		"System.AreaPath":      "MyFirstProject",
		"System.IterationPath": "MyFirstProject",
		"System.TeamProject":   "MyFirstProject",
		"System.CreatedDate":   "2026-08-14T09:00:00Z",
		"System.ChangedBy":     "Test User <test@example.com>",
	} {
		if fields[k] != want {
			t.Fatalf("created fields[%q] = %v, want %v", k, fields[k], want)
		}
	}
	if adoInt64(t, fields["Microsoft.VSTS.Common.Priority"]) != 2 {
		t.Fatalf("created priority = %v", fields["Microsoft.VSTS.Common.Priority"])
	}

	// ===== the fully-qualified $type route normalizes to the bare type =====
	bug := f.call("wit", "on_create_workitem", "POST", "/mock-org/MyFirstProject/_apis/wit/workitems/$Microsoft.VSTS.WorkItemTypes.Bug",
		adoParams(map[string]string{"type": "$Microsoft.VSTS.WorkItemTypes.Bug"}), nil, adoOps(
			map[string]any{"op": "add", "path": "/fields/System.Title", "value": "Normalized type"},
		), adoBasicPAT)
	if bug.Status != 200 {
		t.Fatalf("create $-typed work item -> %d: %v", bug.Status, bug.Body)
	}
	if got := adoBodyMap(t, bug.Body["fields"])["System.WorkItemType"]; got != "Bug" {
		t.Fatalf("$-typed create WorkItemType = %v, want Bug", got)
	}

	// ===== read-back returns the integer id; an unknown id is the WIT 404 envelope =====
	got := f.call("wit", "on_get_workitem", "GET", "/mock-org/MyFirstProject/_apis/wit/workitems/2",
		adoParams(map[string]string{"id": "2"}), nil, nil, adoBasicPAT)
	if got.Status != 200 || adoInt64(t, got.Body["id"]) != 2 || got.Body["rev"] != int64(1) {
		t.Fatalf("get work item -> %d %v", got.Status, got.Body)
	}
	if adoBodyMap(t, got.Body["fields"])["System.Title"] != "Write conformance suite" {
		t.Fatalf("round-tripped title = %v", got.Body["fields"])
	}
	missing := f.call("wit", "on_get_workitem", "GET", "/mock-org/MyFirstProject/_apis/wit/workitems/999",
		adoParams(map[string]string{"id": "999"}), nil, nil, adoBasicPAT)
	if missing.Status != 404 || missing.Body["typeKey"] != "WorkItemDoesNotExistException" {
		t.Fatalf("unknown work item -> %d %v, want 404 WorkItemDoesNotExistException", missing.Status, missing.Body)
	}
	if !strings.Contains(missing.Body["message"].(string), "999") {
		t.Fatalf("404 message = %v, want it to name the id", missing.Body["message"])
	}

	// ===== a patch bumps rev, applies field ops, and refreshes audit fields =====
	f.vc.Advance(time.Minute)
	patched := f.call("wit", "on_update_workitem", "PATCH", "/mock-org/MyFirstProject/_apis/wit/workitems/2",
		adoParams(map[string]string{"id": "2"}), nil, adoOps(
			map[string]any{"op": "replace", "path": "/fields/System.State", "value": "Resolved"},
			map[string]any{"op": "replace", "path": "/fields/Microsoft.VSTS.Common.Priority", "value": 3},
		), adoBasicPAT)
	if patched.Status != 200 {
		t.Fatalf("patch work item -> %d: %v", patched.Status, patched.Body)
	}
	if patched.Body["rev"] != int64(2) {
		t.Fatalf("patched rev = %v, want 2", patched.Body["rev"])
	}
	pf := adoBodyMap(t, patched.Body["fields"])
	if pf["System.State"] != "Resolved" {
		t.Fatalf("patched state = %v", pf["System.State"])
	}
	if adoInt64(t, pf["Microsoft.VSTS.Common.Priority"]) != 3 {
		t.Fatalf("patched priority = %v", pf["Microsoft.VSTS.Common.Priority"])
	}
	if pf["System.ChangedDate"] != "2026-08-14T09:01:00Z" {
		t.Fatalf("patched ChangedDate = %v, want the virtual clock's now", pf["System.ChangedDate"])
	}
	// A read-only "test" op that passes leaves rev alone.
	testPass := f.call("wit", "on_update_workitem", "PATCH", "/mock-org/MyFirstProject/_apis/wit/workitems/2",
		adoParams(map[string]string{"id": "2"}), nil, adoOps(
			map[string]any{"op": "test", "path": "/fields/System.State", "value": "Resolved"},
		), adoBasicPAT)
	if testPass.Status != 200 || testPass.Body["rev"] != int64(2) {
		t.Fatalf("passing test op -> %d rev %v, want 200 rev 2", testPass.Status, testPass.Body["rev"])
	}

	// ===== relations are added at - and removed by numeric index only =====
	link := map[string]any{"rel": "System.LinkTypes.Hierarchy-forward", "url": "vstfs:///WorkItemTracking/WorkItem/1"}
	withRel := f.call("wit", "on_update_workitem", "PATCH", "/mock-org/MyFirstProject/_apis/wit/workitems/2",
		adoParams(map[string]string{"id": "2"}), nil, adoOps(
			map[string]any{"op": "add", "path": "/relations/-", "value": link},
		), adoBasicPAT)
	if withRel.Status != 200 || withRel.Body["rev"] != int64(3) {
		t.Fatalf("add relation -> %d rev %v, want 200 rev 3", withRel.Status, withRel.Body["rev"])
	}
	rels, _ := withRel.Body["relations"].([]any)
	if len(rels) != 1 || adoBodyMap(t, rels[0])["rel"] != "System.LinkTypes.Hierarchy-forward" {
		t.Fatalf("relations = %v, want the one added link", withRel.Body["relations"])
	}
	for _, path := range []string{"/relations/x", "/relations/7"} {
		bad := f.call("wit", "on_update_workitem", "PATCH", "/mock-org/MyFirstProject/_apis/wit/workitems/2",
			adoParams(map[string]string{"id": "2"}), nil, adoOps(
				map[string]any{"op": "remove", "path": path},
			), adoBasicPAT)
		if bad.Status != 400 || bad.Body["typeKey"] != "PatchOperationFailedException" {
			t.Fatalf("remove %s -> %d %v, want 400 PatchOperationFailedException", path, bad.Status, bad.Body)
		}
	}
	unlinked := f.call("wit", "on_update_workitem", "PATCH", "/mock-org/MyFirstProject/_apis/wit/workitems/2",
		adoParams(map[string]string{"id": "2"}), nil, adoOps(
			map[string]any{"op": "remove", "path": "/relations/0"},
		), adoBasicPAT)
	if unlinked.Status != 200 || unlinked.Body["rev"] != int64(4) {
		t.Fatalf("remove relation 0 -> %d rev %v, want 200 rev 4", unlinked.Status, unlinked.Body["rev"])
	}
	if _, has := unlinked.Body["relations"]; has {
		t.Fatalf("relations after remove = %v, want omitted", unlinked.Body["relations"])
	}

	// ===== malformed patches answer the PatchOperationFailed / Argument envelopes =====
	faults := []struct {
		name string
		ops  map[string]any
		key  string
	}{
		{"remove required field", adoOps(map[string]any{"op": "remove", "path": "/fields/System.Title"}), "PatchOperationFailedException"},
		{"unknown op", adoOps(map[string]any{"op": "bogus", "path": "/fields/System.Title", "value": "x"}), "PatchOperationFailedException"},
		{"failing test op", adoOps(map[string]any{"op": "test", "path": "/fields/System.State", "value": "New"}), "PatchOperationFailedException"},
		{"unsupported path", adoOps(map[string]any{"op": "add", "path": "/bogus", "value": 1}), "PatchOperationFailedException"},
	}
	for _, ft := range faults {
		r := f.call("wit", "on_update_workitem", "PATCH", "/mock-org/MyFirstProject/_apis/wit/workitems/2",
			adoParams(map[string]string{"id": "2"}), nil, ft.ops, adoBasicPAT)
		if r.Status != 400 || r.Body["typeKey"] != ft.key {
			t.Fatalf("%s -> %d %v, want 400 %s", ft.name, r.Status, r.Body, ft.key)
		}
	}
	patchMissing := f.call("wit", "on_update_workitem", "PATCH", "/mock-org/MyFirstProject/_apis/wit/workitems/999",
		adoParams(map[string]string{"id": "999"}), nil, adoOps(
			map[string]any{"op": "replace", "path": "/fields/System.State", "value": "Closed"},
		), adoBasicPAT)
	if patchMissing.Status != 404 {
		t.Fatalf("patch unknown work item -> %d, want 404", patchMissing.Status)
	}

	// ===== ids= returns the asked work items and fields= projects to those columns =====
	list := f.call("wit", "on_list_workitems", "GET", "/mock-org/MyFirstProject/_apis/wit/workitems",
		adoParams(nil), map[string]string{"ids": "1,2", "fields": "System.Title,System.State"}, nil, adoBasicPAT)
	if list.Status != 200 || list.Body["count"] != int64(2) {
		t.Fatalf("list by ids -> %d %v", list.Status, list.Body)
	}
	for _, v := range list.Body["value"].([]any) {
		fl := adoBodyMap(t, adoBodyMap(t, v)["fields"])
		if len(fl) != 2 || fl["System.State"] == nil || fl["System.Title"] == nil {
			t.Fatalf("projected fields = %v, want exactly Title+State", fl)
		}
	}
	if r := f.call("wit", "on_list_workitems", "GET", "/mock-org/MyFirstProject/_apis/wit/workitems",
		adoParams(nil), map[string]string{"ids": "999"}, nil, adoBasicPAT); r.Status != 404 {
		t.Fatalf("unknown ids -> %d, want 404", r.Status)
	}
	if r := f.call("wit", "on_list_workitems", "GET", "/mock-org/MyFirstProject/_apis/wit/workitems",
		adoParams(nil), nil, nil, adoBasicPAT); r.Status != 400 || r.Body["typeKey"] != "ArgumentException" {
		t.Fatalf("missing ids -> %d %v, want 400 ArgumentException", r.Status, r.Body)
	}
}

// TestAzureDevOpsWIQLQuerySubset: the WIQL subset ANDs predicates over
// strings and numbers, CONTAINS matches substrings, ORDER BY sorts with $top
// capping, and unsupported queries answer 400.
func TestAzureDevOpsWIQLQuerySubset(t *testing.T) {
	f := newADOFixture(t, time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC))

	mkTask := func(title string, priority int) {
		f.t.Helper()
		r := f.call("wit", "on_create_workitem", "POST", "/mock-org/MyFirstProject/_apis/wit/workitems/Task",
			adoParams(map[string]string{"type": "Task"}), nil, adoOps(
				map[string]any{"op": "add", "path": "/fields/System.Title", "value": title},
				map[string]any{"op": "add", "path": "/fields/Microsoft.VSTS.Common.Priority", "value": priority},
			), adoBasicPAT)
		if r.Status != 200 {
			f.t.Fatalf("create %q -> %d: %v", title, r.Status, r.Body)
		}
	}
	mkTask("Fix login bug", 2) // id 2 at 09:00:00
	f.vc.Advance(2 * time.Second)
	mkTask("Fix signup bug", 4) // id 3 at 09:00:02

	wiql := func(query string, top string) []any {
		f.t.Helper()
		queryParams := map[string]string{}
		if top != "" {
			queryParams["$top"] = top
		}
		r := f.call("wit", "on_wiql", "POST", "/mock-org/MyFirstProject/_apis/wit/wiql",
			adoParams(nil), queryParams, map[string]any{"query": query}, adoBasicPAT)
		if r.Status != 200 {
			f.t.Fatalf("wiql %q -> %d: %v", query, r.Status, r.Body)
		}
		items, _ := r.Body["workItems"].([]any)
		if r.Body["count"] != int64(len(items)) {
			f.t.Fatalf("wiql %q count = %v, want %d", query, r.Body["count"], len(items))
		}
		return items
	}
	ids := func(items []any) []int64 {
		out := []int64{}
		for _, it := range items {
			out = append(out, adoInt64(t, adoBodyMap(t, it)["id"]))
		}
		return out
	}

	// ===== WIQL ANDs string and numeric predicates and matches CONTAINS =====
	got := ids(wiql("SELECT [System.Id] FROM WorkItems WHERE [System.WorkItemType] = 'Task' AND [Microsoft.VSTS.Common.Priority] > 2", ""))
	if len(got) != 1 || got[0] != 3 {
		t.Fatalf("AND + numeric predicate matched %v, want [3] (only the priority-4 task)", got)
	}
	if got := ids(wiql("SELECT [System.Id] FROM WorkItems WHERE [System.Title] CONTAINS 'signup'", "")); len(got) != 1 || got[0] != 3 {
		t.Fatalf("CONTAINS matched %v, want [3]", got)
	}
	if got := ids(wiql("SELECT [System.Id] FROM WorkItems WHERE [System.State] <> 'Active' AND [System.Id] = 1", "")); len(got) != 0 {
		t.Fatalf("<> excluded the seeded Active bug: %v", got)
	}

	// ===== ORDER BY sorts by the field and $top caps the result =====
	if got := ids(wiql("SELECT [System.Id] FROM WorkItems WHERE [System.Id] > 1 ORDER BY [System.CreatedDate] DESC", "")); len(got) != 2 || got[0] != 3 || got[1] != 2 {
		t.Fatalf("ORDER BY DESC = %v, want [3 2] (newest first)", got)
	}
	if got := ids(wiql("SELECT [System.Id] FROM WorkItems WHERE [System.Id] > 1 ORDER BY [System.CreatedDate] DESC", "1")); len(got) != 1 || got[0] != 3 {
		t.Fatalf("$top-capped ORDER BY = %v, want [3]", got)
	}

	// ===== OR and non-WorkItems FROM targets answer the 400 envelope =====
	orq := f.call("wit", "on_wiql", "POST", "/mock-org/MyFirstProject/_apis/wit/wiql",
		adoParams(nil), nil, map[string]any{"query": "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'New' OR [System.Id] = 1"}, adoBasicPAT)
	if orq.Status != 400 || !strings.Contains(orq.Body["message"].(string), "OR is not supported") {
		t.Fatalf("WIQL OR -> %d %v, want 400 OR not supported", orq.Status, orq.Body)
	}
	badFrom := f.call("wit", "on_wiql", "POST", "/mock-org/MyFirstProject/_apis/wit/wiql",
		adoParams(nil), nil, map[string]any{"query": "SELECT [System.Id] FROM Builds"}, adoBasicPAT)
	if badFrom.Status != 400 || !strings.Contains(badFrom.Body["message"].(string), "not a supported WIQL") {
		t.Fatalf("WIQL FROM Builds -> %d %v, want 400 unsupported FROM", badFrom.Status, badFrom.Body)
	}
}

// TestAzureDevOpsGitPushItemsCommits: the repos list is project-scoped, a
// push stores content that items serves at the branch and at a pinned
// commit, the commits list walks the parent chain newest-first, and the git
// fault envelopes cover unknown repos/paths and malformed versions.
func TestAzureDevOpsGitPushItemsCommits(t *testing.T) {
	f := newADOFixture(t, time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC))
	const nullID = "0000000000000000000000000000000000000000"

	// ===== the repos list is scoped to the project =====
	repos := f.call("git", "on_list_repos", "GET", "/mock-org/MyFirstProject/_apis/git/repositories",
		adoParams(nil), nil, nil, adoBasicPAT)
	if repos.Status != 200 || repos.Body["count"] != int64(1) {
		t.Fatalf("repos -> %d %v, want the 1 seeded repo", repos.Status, repos.Body)
	}
	repo := adoBodyMap(t, repos.Body["value"].([]any)[0])
	if repo["id"] != adoRepoID || repo["name"] != "MyFirstProject" || repo["defaultBranch"] != "refs/heads/main" {
		t.Fatalf("repo envelope = %v", repo)
	}
	if repo["size"] != int64(1024) {
		t.Fatalf("repo size = %v (%T), want int64 1024 (store floats must be coerced)", repo["size"], repo["size"])
	}
	if adoBodyMap(t, repo["project"])["name"] != "MyFirstProject" {
		t.Fatalf("repo project = %v", repo["project"])
	}
	if other := f.call("git", "on_list_repos", "GET", "/mock-org/BackendServices/_apis/git/repositories",
		adoParams(map[string]string{"project": "BackendServices"}), nil, nil, adoBasicPAT); other.Body["count"] != int64(0) {
		t.Fatalf("BackendServices repos = %v, want none", other.Body)
	}

	// ===== a push stores content that items serves at the branch and at a pinned commit =====
	push := func(comment, path, content, changeType, oldID string) starlark.Response {
		body := map[string]any{
			"refUpdates": []any{map[string]any{"name": "refs/heads/main", "oldObjectId": oldID}},
			"commits": []any{map[string]any{
				"comment": comment,
				"changes": []any{map[string]any{
					"changeType": changeType,
					"item":       map[string]any{"path": path},
					"newContent": map[string]any{"content": content, "contentType": "rawtext"},
				}},
			}},
		}
		return f.call("git", "on_push", "POST", "/mock-org/MyFirstProject/_apis/git/repositories/"+adoRepoID+"/pushes",
			adoParams(map[string]string{"repoId": adoRepoID}), nil, body, adoBasicPAT)
	}
	p1 := push("Add docs", "/docs.md", "hello docs v1", "add", nullID)
	if p1.Status != 200 {
		t.Fatalf("push 1 -> %d: %v", p1.Status, p1.Body)
	}
	if p1.Body["pushId"] != int64(1) {
		t.Fatalf("pushId = %v (%T), want int64 1 (no seeded pushes)", p1.Body["pushId"], p1.Body["pushId"])
	}
	p1Commits, _ := p1.Body["commits"].([]any)
	if len(p1Commits) != 1 {
		t.Fatalf("push 1 commits = %d, want 1", len(p1Commits))
	}
	commit1, _ := adoBodyMap(t, p1Commits[0])["commitId"].(string)
	if len(commit1) != 40 {
		t.Fatalf("commitId %q is not a 40-hex object id", commit1)
	}
	refUpdates, _ := p1.Body["refUpdates"].([]any)
	ru := adoBodyMap(t, refUpdates[0])
	if ru["newObjectId"] != commit1 || ru["oldObjectId"] != nullID || ru["name"] != "refs/heads/main" {
		t.Fatalf("push 1 refUpdates = %v", ru)
	}

	item := func(path, versionDescriptor string) starlark.Response {
		query := map[string]string{"path": path}
		if versionDescriptor != "" {
			query["versionDescriptor"] = versionDescriptor
		}
		return f.call("git", "on_get_item", "GET", "/mock-org/MyFirstProject/_apis/git/repositories/"+adoRepoID+"/items",
			adoParams(map[string]string{"repoId": adoRepoID}), query, nil, adoBasicPAT)
	}
	atHead := item("/docs.md", "")
	if atHead.Status != 200 || atHead.Body["content"] != "hello docs v1" || atHead.Body["commitId"] != commit1 {
		t.Fatalf("item at head -> %d %v", atHead.Status, atHead.Body)
	}
	if atHead.Body["gitObjectType"] != "blob" || atHead.Body["path"] != "/docs.md" {
		t.Fatalf("item envelope = %v", atHead.Body)
	}
	if len(atHead.Body["objectId"].(string)) != 40 {
		t.Fatalf("item objectId = %v", atHead.Body["objectId"])
	}
	readme := item("/readme.md", "")
	if readme.Status != 200 || readme.Body["content"] != "# MyFirstProject\n\nSeeded readme for local testing.\n" {
		t.Fatalf("seeded readme -> %d %v", readme.Status, readme.Body)
	}

	// A second push edits the file; the branch moves, the pinned commit does not.
	p2 := push("Edit docs", "/docs.md", "hello docs v2", "edit", commit1)
	if p2.Status != 200 {
		t.Fatalf("push 2 -> %d: %v", p2.Status, p2.Body)
	}
	commit2 := adoBodyMap(t, p2.Body["commits"].([]any)[0])["commitId"].(string)
	pinned := item("/docs.md", `{"versionType":"commit","version":"`+commit1+`"}`)
	if pinned.Status != 200 || pinned.Body["content"] != "hello docs v1" || pinned.Body["commitId"] != commit1 {
		t.Fatalf("item at pinned commit -> %d %v", pinned.Status, pinned.Body)
	}
	atBranch := item("/docs.md", `{"versionType":"branch","version":"main"}`)
	if atBranch.Status != 200 || atBranch.Body["content"] != "hello docs v2" || atBranch.Body["commitId"] != commit2 {
		t.Fatalf("item at moved branch -> %d %v", atBranch.Status, atBranch.Body)
	}

	// ===== the commits list walks newest-first along the parent chain and filters by refName =====
	commits := f.call("git", "on_list_commits", "GET", "/mock-org/MyFirstProject/_apis/git/repositories/"+adoRepoID+"/commits",
		adoParams(map[string]string{"repoId": adoRepoID}), nil, nil, adoBasicPAT)
	if commits.Status != 200 || commits.Body["count"] != int64(3) {
		t.Fatalf("commits -> %d %v, want seed + 2 pushes", commits.Status, commits.Body)
	}
	cv, _ := commits.Body["value"].([]any)
	if adoBodyMap(t, cv[0])["comment"] != "Edit docs" || adoBodyMap(t, cv[1])["comment"] != "Add docs" || adoBodyMap(t, cv[2])["comment"] != "Initial commit" {
		t.Fatalf("commit order = %v %v %v, want newest first", cv[0], cv[1], cv[2])
	}
	if adoBodyMap(t, cv[0])["parents"].([]any)[0] != adoBodyMap(t, cv[1])["commitId"] ||
		adoBodyMap(t, cv[1])["parents"].([]any)[0] != adoBodyMap(t, cv[2])["commitId"] {
		t.Fatalf("parent chain broken: %v", cv)
	}
	if parents := adoBodyMap(t, cv[2])["parents"].([]any); len(parents) != 0 {
		t.Fatalf("seed commit parents = %v, want none", parents)
	}
	if r := f.call("git", "on_list_commits", "GET", "/mock-org/MyFirstProject/_apis/git/repositories/"+adoRepoID+"/commits",
		adoParams(map[string]string{"repoId": adoRepoID}), map[string]string{"searchCriteria.refName": "refs/heads/nope"}, nil, adoBasicPAT); r.Body["count"] != int64(0) {
		t.Fatalf("commits for unknown ref = %v, want none", r.Body)
	}

	// ===== git faults: unknown repo, missing path, unknown path, malformed versionDescriptor =====
	unknownRepo := f.call("git", "on_list_commits", "GET", "/mock-org/MyFirstProject/_apis/git/repositories/deadbeef/commits",
		adoParams(map[string]string{"repoId": "deadbeef"}), nil, nil, adoBasicPAT)
	if unknownRepo.Status != 404 || unknownRepo.Body["typeKey"] != "GitRepositoryNotFoundException" {
		t.Fatalf("unknown repo -> %d %v, want 404 GitRepositoryNotFoundException", unknownRepo.Status, unknownRepo.Body)
	}
	noPath := item("", "")
	if noPath.Status != 400 || noPath.Body["typeKey"] != "GitArgumentOutOfRangeException" {
		t.Fatalf("items without path -> %d %v, want 400", noPath.Status, noPath.Body)
	}
	unknownPath := item("/nope.md", "")
	if unknownPath.Status != 404 || unknownPath.Body["typeKey"] != "GitItemNotFoundException" || unknownPath.Body["eventId"] != int64(4096) {
		t.Fatalf("unknown path -> %d %v, want 404 GitItemNotFoundException eventId 4096", unknownPath.Status, unknownPath.Body)
	}
	malformed := item("/docs.md", `{"versionType": commit}`)
	if malformed.Status != 400 || malformed.Body["typeKey"] != "InvalidArgument" {
		t.Fatalf("malformed versionDescriptor -> %d %v, want 400 InvalidArgument (not a 500)", malformed.Status, malformed.Body)
	}
	// Modeled as-is: a non-JSON descriptor is ignored and the default branch serves.
	if r := item("/docs.md", "main-please"); r.Status != 200 || r.Body["content"] != "hello docs v2" {
		t.Fatalf("non-JSON versionDescriptor -> %d %v (modeled: ignored, default branch)", r.Status, r.Body)
	}
}

// TestAzureDevOpsPipelineRunLifecycle: pipelines list/get round-trip with
// their 404s, and a queued run derives inProgress then completed from the
// VIRTUAL clock (persisting each transition so lists agree with polls), with
// simulate_fail driving the result to failed.
func TestAzureDevOpsPipelineRunLifecycle(t *testing.T) {
	base := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	f := newADOFixture(t, base)

	// ===== pipelines list and get round-trip, and unknown ids answer 404 =====
	list := f.call("pipelines", "on_list_pipelines", "GET", "/mock-org/MyFirstProject/_apis/pipelines",
		adoParams(nil), nil, nil, adoBasicPAT)
	if list.Status != 200 || list.Body["count"] != int64(1) {
		t.Fatalf("pipelines -> %d %v", list.Status, list.Body)
	}
	pipe := adoBodyMap(t, list.Body["value"].([]any)[0])
	if adoInt64(t, pipe["id"]) != 1 || pipe["name"] != "MyFirstProject-CI" || pipe["folder"] != "\\" {
		t.Fatalf("pipeline envelope = %v", pipe)
	}
	cfg := adoBodyMap(t, pipe["configuration"])
	if cfg["type"] != "yaml" || cfg["path"] != "/azure-pipelines.yml" {
		t.Fatalf("pipeline configuration = %v", cfg)
	}
	if got := f.call("pipelines", "on_get_pipeline", "GET", "/mock-org/MyFirstProject/_apis/pipelines/1",
		adoParams(map[string]string{"pipelineId": "1"}), nil, nil, adoBasicPAT); got.Status != 200 {
		t.Fatalf("get pipeline -> %d %v", got.Status, got.Body)
	}
	if r := f.call("pipelines", "on_get_pipeline", "GET", "/mock-org/MyFirstProject/_apis/pipelines/999",
		adoParams(map[string]string{"pipelineId": "999"}), nil, nil, adoBasicPAT); r.Status != 404 || r.Body["typeKey"] != "PipelineNotFoundException" {
		t.Fatalf("unknown pipeline -> %d %v", r.Status, r.Body)
	}
	if r := f.call("pipelines", "on_queue_run", "POST", "/mock-org/MyFirstProject/_apis/pipelines/999/runs",
		adoParams(map[string]string{"pipelineId": "999"}), nil, map[string]any{}, adoBasicPAT); r.Status != 404 {
		t.Fatalf("queue run on unknown pipeline -> %d, want 404", r.Status)
	}

	// ===== a queued run derives inProgress then completed from the clock, once each =====
	queued := f.call("pipelines", "on_queue_run", "POST", "/mock-org/MyFirstProject/_apis/pipelines/1/runs",
		adoParams(map[string]string{"pipelineId": "1"}), nil, map[string]any{}, adoBasicPAT)
	if queued.Status != 200 {
		t.Fatalf("queue run -> %d: %v", queued.Status, queued.Body)
	}
	if adoInt64(t, queued.Body["id"]) != 1 || queued.Body["state"] != "queued" || queued.Body["result"] != nil {
		t.Fatalf("queued run = %v", queued.Body)
	}
	if queued.Body["name"] != "20260814.1" {
		t.Fatalf("run name = %v, want the real yyyymmdd.N form", queued.Body["name"])
	}
	if queued.Body["createdDate"] != "2026-08-14T09:00:00Z" {
		t.Fatalf("run createdDate = %v", queued.Body["createdDate"])
	}
	qp := adoBodyMap(t, queued.Body["pipeline"])
	if adoInt64(t, qp["id"]) != 1 || qp["name"] != "MyFirstProject-CI" {
		t.Fatalf("run pipeline = %v", qp)
	}
	res := adoBodyMap(t, queued.Body["resources"])
	if adoBodyMap(t, adoBodyMap(t, res["repositories"])["self"])["refName"] != "refs/heads/main" {
		t.Fatalf("default run resources = %v", res)
	}

	f.vc.Advance(2 * time.Second) // >1s, <3s after queue
	inProgress := f.call("pipelines", "on_get_run", "GET", "/mock-org/MyFirstProject/_apis/pipelines/1/runs/1",
		adoParams(map[string]string{"pipelineId": "1", "runId": "1"}), nil, nil, adoBasicPAT)
	if inProgress.Status != 200 || inProgress.Body["state"] != "inProgress" || inProgress.Body["result"] != nil {
		t.Fatalf("run at 2s -> %d %v, want inProgress", inProgress.Status, inProgress.Body)
	}
	// The transition persisted: a fresh list read agrees with the poll.
	listed := f.call("pipelines", "on_list_runs", "GET", "/mock-org/MyFirstProject/_apis/pipelines/1/runs",
		adoParams(map[string]string{"pipelineId": "1"}), nil, nil, adoBasicPAT)
	lr := adoBodyMap(t, listed.Body["value"].([]any)[0])
	if lr["state"] != "inProgress" || listed.Body["count"] != int64(1) {
		t.Fatalf("runs list after transition = %v (lists must agree with polls)", listed.Body)
	}

	// A second run queued later has its own clock anchor.
	failer := f.call("pipelines", "on_queue_run", "POST", "/mock-org/MyFirstProject/_apis/pipelines/1/runs",
		adoParams(map[string]string{"pipelineId": "1"}), nil,
		map[string]any{"templateParameters": map[string]any{"simulate_fail": "true"}}, adoBasicPAT)
	if failer.Status != 200 || adoInt64(t, failer.Body["id"]) != 2 || failer.Body["state"] != "queued" {
		t.Fatalf("failure-injected queue -> %d %v", failer.Status, failer.Body)
	}

	f.vc.Advance(2 * time.Second) // run 1 is 4s old; run 2 is 2s old
	done := f.call("pipelines", "on_get_run", "GET", "/mock-org/MyFirstProject/_apis/pipelines/1/runs/1",
		adoParams(map[string]string{"pipelineId": "1", "runId": "1"}), nil, nil, adoBasicPAT)
	if done.Body["state"] != "completed" || done.Body["result"] != "succeeded" {
		t.Fatalf("run at 4s = %v, want completed/succeeded", done.Body)
	}
	if done.Body["finishedDate"] != "2026-08-14T09:00:04Z" {
		t.Fatalf("finishedDate = %v, want stamped at the transition", done.Body["finishedDate"])
	}
	if young := f.call("pipelines", "on_get_run", "GET", "/mock-org/MyFirstProject/_apis/pipelines/1/runs/2",
		adoParams(map[string]string{"pipelineId": "1", "runId": "2"}), nil, nil, adoBasicPAT); young.Body["state"] != "inProgress" {
		t.Fatalf("younger run at the same read = %v, want inProgress (per-run anchors)", young.Body)
	}

	// ===== simulate_fail drives the completed result to failed =====
	f.vc.Advance(2 * time.Second)
	failed := f.call("pipelines", "on_get_run", "GET", "/mock-org/MyFirstProject/_apis/pipelines/1/runs/2",
		adoParams(map[string]string{"pipelineId": "1", "runId": "2"}), nil, nil, adoBasicPAT)
	if failed.Body["state"] != "completed" || failed.Body["result"] != "failed" {
		t.Fatalf("failure-injected run = %v, want completed/failed", failed.Body)
	}
	if r := f.call("pipelines", "on_get_run", "GET", "/mock-org/MyFirstProject/_apis/pipelines/1/runs/999",
		adoParams(map[string]string{"pipelineId": "1", "runId": "999"}), nil, nil, adoBasicPAT); r.Status != 404 || r.Body["typeKey"] != "RunNotFoundException" {
		t.Fatalf("unknown run -> %d %v", r.Status, r.Body)
	}
}

// TestAzureDevOpsServiceHookDelivery: registering a webHooks consumer
// subscription makes work-item creation deliver the real service-hook
// envelope — unsigned — to the registered URL.
func TestAzureDevOpsServiceHookDelivery(t *testing.T) {
	f := newADOFixture(t, time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC))

	var mu sync.Mutex
	var deliveries []adoDelivery
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		keys := make([]string, 0, len(r.Header))
		for k := range r.Header {
			keys = append(keys, k)
		}
		mu.Lock()
		deliveries = append(deliveries, adoDelivery{body: decoded, headerKeys: keys})
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer sink.Close()

	// ===== a registered subscription receives the service-hook envelope when a work item is created =====
	sub := f.call("hooks", "on_create_subscription", "POST", "/mock-org/_apis/hooks/subscriptions",
		map[string]string{"org": "mock-org"}, nil, map[string]any{
			"consumerId":       "webHooks",
			"consumerActionId": "httpRequest",
			"eventType":        "workitem.created",
			"consumerInputs":   map[string]any{"url": sink.URL + "/hook", "httpMethod": "POST"},
			"publisherInputs":  map[string]any{},
			"resourceVersion":  "1.0",
		}, adoBasicPAT)
	if sub.Status != 200 {
		t.Fatalf("create subscription -> %d: %v", sub.Status, sub.Body)
	}
	subID, _ := sub.Body["id"].(string)
	if subID == "" || sub.Body["eventType"] != "workitem.created" || sub.Body["status"] != "enabled" {
		t.Fatalf("subscription envelope = %v", sub.Body)
	}
	if adoBodyMap(t, sub.Body["consumerInputs"])["url"] != sink.URL+"/hook" {
		t.Fatalf("subscription consumerInputs = %v", sub.Body["consumerInputs"])
	}

	created := f.call("wit", "on_create_workitem", "POST", "/mock-org/MyFirstProject/_apis/wit/workitems/Task",
		adoParams(map[string]string{"type": "Task"}), nil, adoOps(
			map[string]any{"op": "add", "path": "/fields/System.Title", "value": "Hook me"},
		), adoBasicPAT)
	if created.Status != 200 {
		t.Fatalf("create work item -> %d: %v", created.Status, created.Body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 1 {
		t.Fatalf("sink received %d deliveries, want exactly 1", len(deliveries))
	}
	d := deliveries[0]
	if d.body["type"] != "workitem.created" {
		t.Fatalf("delivery type = %v, want workitem.created", d.body["type"])
	}
	payload := adoBodyMap(t, d.body["payload"])
	if payload["subscriptionId"] != subID || payload["eventType"] != "workitem.created" || payload["publisherId"] != "tfs" {
		t.Fatalf("service-hook payload header = %v", payload)
	}
	if adoInt64(t, payload["notificationId"]) < 1 {
		t.Fatalf("notificationId = %v, want a positive sequence", payload["notificationId"])
	}
	resource := adoBodyMap(t, payload["resource"])
	if adoInt64(t, resource["id"]) != adoInt64(t, created.Body["id"]) {
		t.Fatalf("payload resource id = %v, want the created work item", resource["id"])
	}
	if adoBodyMap(t, resource["fields"])["System.Title"] != "Hook me" {
		t.Fatalf("payload resource fields = %v", resource["fields"])
	}
	if text := adoBodyMap(t, payload["message"])["text"].(string); !strings.Contains(text, "created") {
		t.Fatalf("delivery message = %q, want a creation notice", text)
	}
	// Deliveries are unsigned by design (Azure DevOps signs nothing here).
	for _, k := range d.headerKeys {
		if strings.Contains(strings.ToLower(k), "sign") {
			t.Fatalf("delivery carried signature header %q — service hooks are unsigned by design", k)
		}
	}
}

// TestAzureDevOpsIterations: the team-settings iterations endpoint lists the
// two seeded sprints with their timeFrame attributes and pages under $top.
func TestAzureDevOpsIterations(t *testing.T) {
	f := newADOFixture(t, time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC))

	// ===== iterations list the two sprints and page under $top =====
	r := f.call("work", "on_iterations", "GET", "/mock-org/MyFirstProject/_apis/work/teamsettings/iterations",
		adoParams(nil), nil, nil, adoBasicPAT)
	if r.Status != 200 || r.Body["count"] != int64(2) {
		t.Fatalf("iterations -> %d %v", r.Status, r.Body)
	}
	value, _ := r.Body["value"].([]any)
	s1, s2 := adoBodyMap(t, value[0]), adoBodyMap(t, value[1])
	if s1["name"] != "Sprint 1" || s1["path"] != "MyFirstProject\\Sprint 1" {
		t.Fatalf("Sprint 1 = %v", s1)
	}
	if adoBodyMap(t, s1["attributes"])["timeFrame"] != "current" || adoBodyMap(t, s2["attributes"])["timeFrame"] != "future" {
		t.Fatalf("iteration attributes = %v / %v", s1["attributes"], s2["attributes"])
	}
	paged := f.call("work", "on_iterations", "GET", "/mock-org/MyFirstProject/_apis/work/teamsettings/iterations",
		adoParams(nil), map[string]string{"$top": "1"}, nil, adoBasicPAT)
	if paged.Status != 200 || len(paged.Body["value"].([]any)) != 1 || paged.Body["continuationToken"] != "1" {
		t.Fatalf("paged iterations -> %d %v", paged.Status, paged.Body)
	}
}
