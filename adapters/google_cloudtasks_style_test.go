package adapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// These tests drive the google-cloudtasks-style adapter scripts directly
// (lib.star preloaded) over a shared store and a VIRTUAL clock: one VM per
// handler script, all observing the same collections/kv, exactly like the
// engine. The virtual clock pins the retry/backoff schedule arithmetic
// (failing-worker profile) without sleeping.

const (
	ctProject  = "demo"
	ctLocation = "us-central1"
	ctQueue    = "orders"
)

func ctQueueName() string {
	return "projects/" + ctProject + "/locations/" + ctLocation + "/queues/" + ctQueue
}

// ctFixture is one shared store + virtual clock + one VM per script.
type ctFixture struct {
	t   *testing.T
	vc  *clock.Clock
	vmL *starlark.VM // locations.star
	vmQ *starlark.VM // queues.star
	vmT *starlark.VM // tasks.star
}

func newCTFixture(t *testing.T, start time.Time, active func() string) *ctFixture {
	t.Helper()
	dir := repoAdaptersDir(t)
	root := filepath.Join(dir, "google-cloudtasks-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}

	tmp := t.TempDir()
	store, err := primitives.Open(filepath.Join(tmp, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	kvStore, err := kv.Open(filepath.Join(tmp, "s.kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kvStore.Close() })
	blobStore, err := blob.Open(filepath.Join(tmp, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store:         store,
		KV:            kvStore,
		Blob:          blobStore,
		Clock:         vc,
		ServiceName:   "test",
		ActiveProfile: active,
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

	return &ctFixture{
		t:   t,
		vc:  vc,
		vmL: load("locations.star"),
		vmQ: load("queues.star"),
		vmT: load("tasks.star"),
	}
}

// call invokes a handler with bearer auth, path params, query params, and
// a marshalled JSON body (nil for none).
func (f *ctFixture) call(vm *starlark.VM, handler, method, path string, params, query map[string]string, body any) starlark.Response {
	f.t.Helper()
	raw := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		raw = string(b)
	}
	if query == nil {
		query = map[string]string{}
	}
	resp, err := vm.Call(handler, starlark.Request{
		Method:  method,
		Path:    path,
		Host:    "cloudtasks.stunt.test",
		Headers: map[string]string{"Authorization": "Bearer ya29.test-token"},
		Body:    jsonMap(body),
		RawBody: raw,
		Params:  params,
		Query:   query,
	})
	if err != nil {
		f.t.Fatalf("%s: %v", handler, err)
	}
	return resp
}

func jsonMap(body any) map[string]any {
	if body == nil {
		return nil
	}
	switch v := body.(type) {
	case map[string]any:
		return v
	case map[string]string:
		out := map[string]any{}
		for k, s := range v {
			out[k] = s
		}
		return out
	default:
		b, _ := json.Marshal(body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
}

// --- assertion helpers ---

// ctNum coerces handler-produced numbers (Starlark ints arrive as int64,
// floats as float64) for comparison.
func ctNum(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case float64:
		return n
	}
	return -1
}

func ctBody(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	if r.Body == nil {
		t.Fatalf("expected object body, got status %d body %q", r.Status, r.RawBody)
	}
	return r.Body
}

func ctErrStatus(t *testing.T, r starlark.Response) string {
	t.Helper()
	e, ok := ctBody(t, r)["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope, got %v", r.Body)
	}
	s, _ := e["status"].(string)
	return s
}

// ctCreateQueue makes the fixture's queue, returning its entity.
func (f *ctFixture) ctCreateQueue(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	r := f.call(f.vmQ, "on_create_queue", "POST", "/v2/projects/demo/locations/us-central1/queues",
		map[string]string{"project": ctProject, "location": ctLocation},
		map[string]string{"queueId": ctQueue}, body)
	if r.Status != 200 {
		t.Fatalf("create queue: status %d body %v", r.Status, r.Body)
	}
	return ctBody(t, r)
}

// ctCreateTask posts a task with the given httpRequest overrides.
func (f *ctFixture) ctCreateTask(t *testing.T, task map[string]any) (starlark.Response, map[string]any) {
	t.Helper()
	r := f.call(f.vmT, "on_create_task", "POST", "/v2/"+ctQueueName()+"/tasks",
		map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue}, nil,
		map[string]any{"task": task})
	return r, ctBody(t, r)
}

// --- locations ---

func TestCTLocations(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)

	r := f.call(f.vmL, "on_list_locations", "GET", "/v2/projects/demo/locations",
		map[string]string{"project": ctProject}, nil, nil)
	if r.Status != 200 {
		t.Fatalf("list locations: %d %v", r.Status, r.Body)
	}
	locs := ctBody(t, r)["locations"].([]any)
	if len(locs) < 20 {
		t.Fatalf("expected a broad region list, got %d", len(locs))
	}
	first := locs[0].(map[string]any)
	if first["name"] != "projects/demo/locations/asia-east1" || first["locationId"] != "asia-east1" {
		t.Fatalf("unexpected location entity: %v", first)
	}

	r = f.call(f.vmL, "on_get_location", "GET", "/v2/projects/demo/locations/us-central1",
		map[string]string{"project": ctProject, "location": ctLocation}, nil, nil)
	if r.Status != 200 || ctBody(t, r)["locationId"] != ctLocation {
		t.Fatalf("get location: %d %v", r.Status, r.Body)
	}

	r = f.call(f.vmL, "on_get_location", "GET", "/v2/projects/demo/locations/mars",
		map[string]string{"project": ctProject, "location": "mars"}, nil, nil)
	if r.Status != 404 || ctErrStatus(t, r) != "NOT_FOUND" {
		t.Fatalf("unknown location: %d %v", r.Status, r.Body)
	}
}

func TestCTCmekConfig(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)

	r := f.call(f.vmL, "on_get_cmek", "GET", "/v2/projects/demo/locations/us-central1/cmekConfig",
		map[string]string{"project": ctProject, "location": ctLocation}, nil, nil)
	if r.Status != 200 || ctBody(t, r)["kmsKey"] != "" {
		t.Fatalf("default cmek: %d %v", r.Status, r.Body)
	}

	key := "projects/demo/locations/us/keyRings/kr/cryptoKeys/k1"
	r = f.call(f.vmL, "on_update_cmek", "PATCH", "/v2/projects/demo/locations/us-central1/cmekConfig",
		map[string]string{"project": ctProject, "location": ctLocation}, nil,
		map[string]any{"kmsKey": key})
	if r.Status != 200 || ctBody(t, r)["kmsKey"] != key {
		t.Fatalf("update cmek: %d %v", r.Status, r.Body)
	}

	r = f.call(f.vmL, "on_get_cmek", "GET", "/v2/projects/demo/locations/us-central1/cmekConfig",
		map[string]string{"project": ctProject, "location": ctLocation}, nil, nil)
	if ctBody(t, r)["kmsKey"] != key {
		t.Fatalf("cmek not persisted: %v", r.Body)
	}
}

// --- queues ---

func TestCTQueueCreate(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)

	q := f.ctCreateQueue(t, map[string]any{})
	if q["name"] != ctQueueName() || q["state"] != "RUNNING" {
		t.Fatalf("bad queue entity: %v", q)
	}
	rl := q["rateLimits"].(map[string]any)
	if ctNum(rl["maxDispatchesPerSecond"]) != 500 || ctNum(rl["maxBurstSize"]) != 100 || ctNum(rl["maxConcurrentDispatches"]) != 1000 {
		t.Fatalf("bad default rateLimits: %v", rl)
	}
	rc := q["retryConfig"].(map[string]any)
	if ctNum(rc["maxAttempts"]) != 100 || rc["minBackoff"] != "0.100s" || rc["maxBackoff"] != "3600s" || ctNum(rc["maxDoublings"]) != 16 {
		t.Fatalf("bad default retryConfig: %v", rc)
	}

	// Duplicate -> 409 ALREADY_EXISTS.
	r := f.call(f.vmQ, "on_create_queue", "POST", "/v2/projects/demo/locations/us-central1/queues",
		map[string]string{"project": ctProject, "location": ctLocation},
		map[string]string{"queueId": ctQueue}, map[string]any{})
	if r.Status != 409 || ctErrStatus(t, r) != "ALREADY_EXISTS" {
		t.Fatalf("duplicate queue: %d %v", r.Status, r.Body)
	}

	// Unknown location -> 404.
	r = f.call(f.vmQ, "on_create_queue", "POST", "/v2/projects/demo/locations/mars/queues",
		map[string]string{"project": ctProject, "location": "mars"},
		map[string]string{"queueId": "x"}, map[string]any{})
	if r.Status != 404 || ctErrStatus(t, r) != "NOT_FOUND" {
		t.Fatalf("bad location: %d %v", r.Status, r.Body)
	}

	// Invalid queue ID -> 400.
	r = f.call(f.vmQ, "on_create_queue", "POST", "/v2/projects/demo/locations/us-central1/queues",
		map[string]string{"project": ctProject, "location": ctLocation},
		map[string]string{"queueId": "bad_id"}, map[string]any{})
	if r.Status != 400 || ctErrStatus(t, r) != "INVALID_ARGUMENT" {
		t.Fatalf("bad queue id: %d %v", r.Status, r.Body)
	}

	// name derived from queue.name when queueId is absent.
	r = f.call(f.vmQ, "on_create_queue", "POST", "/v2/projects/demo/locations/us-central1/queues",
		map[string]string{"project": ctProject, "location": ctLocation}, nil,
		map[string]any{"name": "projects/demo/locations/us-central1/queues/from-name"})
	if r.Status != 200 || ctBody(t, r)["name"] != "projects/demo/locations/us-central1/queues/from-name" {
		t.Fatalf("name-derived create: %d %v", r.Status, r.Body)
	}

	// mismatched name -> 400.
	r = f.call(f.vmQ, "on_create_queue", "POST", "/v2/projects/demo/locations/us-central1/queues",
		map[string]string{"project": ctProject, "location": ctLocation},
		map[string]string{"queueId": "zzz"},
		map[string]any{"name": "projects/demo/locations/us-central1/queues/other"})
	if r.Status != 400 {
		t.Fatalf("name mismatch: %d %v", r.Status, r.Body)
	}

	// rate above the documented cap -> 400.
	r = f.call(f.vmQ, "on_create_queue", "POST", "/v2/projects/demo/locations/us-central1/queues",
		map[string]string{"project": ctProject, "location": ctLocation},
		map[string]string{"queueId": "cap"},
		map[string]any{"rateLimits": map[string]any{"maxDispatchesPerSecond": 900}})
	if r.Status != 400 {
		t.Fatalf("rate cap: %d %v", r.Status, r.Body)
	}
}

func TestCTQueueListFilterPauseResume(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)
	f.ctCreateQueue(t, map[string]any{})
	// A second queue in a DIFFERENT location: the location prefix must not
	// cross-match.
	r := f.call(f.vmQ, "on_create_queue", "POST", "/v2/projects/demo/locations/us-east1/queues",
		map[string]string{"project": ctProject, "location": "us-east1"},
		map[string]string{"queueId": ctQueue}, map[string]any{})
	if r.Status != 200 {
		t.Fatalf("second queue: %d %v", r.Status, r.Body)
	}

	r = f.call(f.vmQ, "on_list_queues", "GET", "/v2/projects/demo/locations/us-central1/queues",
		map[string]string{"project": ctProject, "location": ctLocation}, nil, nil)
	queues := ctBody(t, r)["queues"].([]any)
	if len(queues) != 1 || queues[0].(map[string]any)["name"] != ctQueueName() {
		t.Fatalf("location-scoped list leaked: %v", queues)
	}

	// Pause, then filter state = PAUSED finds it.
	qv := func(verb string, params map[string]string) starlark.Response {
		params["queue_verb"] = ctQueue + ":" + verb
		return f.call(f.vmQ, "on_queue_verb", "POST", "/v2/x", params, nil, map[string]any{})
	}
	base := map[string]string{"project": ctProject, "location": ctLocation}
	r = qv("pause", map[string]string{"project": ctProject, "location": ctLocation})
	if r.Status != 200 || ctBody(t, r)["state"] != "PAUSED" {
		t.Fatalf("pause: %d %v", r.Status, r.Body)
	}
	// Pausing twice is idempotent.
	if r = qv("pause", map[string]string{"project": ctProject, "location": ctLocation}); r.Status != 200 {
		t.Fatalf("pause twice: %d %v", r.Status, r.Body)
	}

	r = f.call(f.vmQ, "on_list_queues", "GET", "/v2/projects/demo/locations/us-central1/queues",
		map[string]string{"project": ctProject, "location": ctLocation},
		map[string]string{"filter": "state = PAUSED"}, nil)
	if got := len(ctBody(t, r)["queues"].([]any)); got != 1 {
		t.Fatalf("state filter: %d queues", got)
	}

	r = f.call(f.vmQ, "on_list_queues", "GET", "/v2/projects/demo/locations/us-central1/queues",
		map[string]string{"project": ctProject, "location": ctLocation},
		map[string]string{"filter": "state = RUNNING"}, nil)
	if got := len(ctBody(t, r)["queues"].([]any)); got != 0 {
		t.Fatalf("state filter RUNNING: %d queues", got)
	}

	if r = qv("resume", base); r.Status != 200 || ctBody(t, r)["state"] != "RUNNING" {
		t.Fatalf("resume: %d %v", r.Status, r.Body)
	}

	// Unknown verb -> 404.
	r = f.call(f.vmQ, "on_queue_verb", "POST", "/v2/x",
		map[string]string{"project": ctProject, "location": ctLocation, "queue_verb": ctQueue + ":explode"},
		nil, map[string]any{})
	if r.Status != 404 {
		t.Fatalf("unknown verb: %d %v", r.Status, r.Body)
	}
}

func TestCTQueuePatch(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)
	f.ctCreateQueue(t, map[string]any{})
	params := map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue}
	path := "/v2/" + ctQueueName()

	// Dotted mask sets one leaf; burst re-derives (output only).
	r := f.call(f.vmQ, "on_patch_queue", "PATCH", path, params,
		map[string]string{"updateMask": "rateLimits.maxDispatchesPerSecond"},
		map[string]any{"rateLimits": map[string]any{"maxDispatchesPerSecond": 10}})
	if r.Status != 200 {
		t.Fatalf("patch: %d %v", r.Status, r.Body)
	}
	rl := ctBody(t, r)["rateLimits"].(map[string]any)
	if ctNum(rl["maxDispatchesPerSecond"]) != 10 || ctNum(rl["maxBurstSize"]) != 10 {
		t.Fatalf("patched rateLimits: %v", rl)
	}

	// Bare mask replaces the message.
	r = f.call(f.vmQ, "on_patch_queue", "PATCH", path, params,
		map[string]string{"updateMask": "retryConfig"},
		map[string]any{"retryConfig": map[string]any{"maxAttempts": 3, "minBackoff": "5s"}})
	rc := ctBody(t, r)["retryConfig"].(map[string]any)
	if ctNum(rc["maxAttempts"]) != 3 || rc["minBackoff"] != "5s" || rc["maxBackoff"] != "3600s" {
		t.Fatalf("patched retryConfig: %v", rc)
	}

	// The patch survives a re-read (round-trip through the store).
	r = f.call(f.vmQ, "on_get_queue", "GET", path, params, nil, nil)
	if ctBody(t, r)["retryConfig"].(map[string]any)["maxAttempts"] != float64(3) {
		t.Fatalf("patch not persisted: %v", r.Body)
	}

	// state in the mask is rejected.
	r = f.call(f.vmQ, "on_patch_queue", "PATCH", path, params,
		map[string]string{"updateMask": "state"}, map[string]any{"state": "PAUSED"})
	if r.Status != 400 {
		t.Fatalf("state mask: %d %v", r.Status, r.Body)
	}

	// Immutable name.
	r = f.call(f.vmQ, "on_patch_queue", "PATCH", path, params, nil,
		map[string]any{"name": "projects/demo/locations/us-central1/queues/elsewhere"})
	if r.Status != 400 {
		t.Fatalf("name immutable: %d %v", r.Status, r.Body)
	}

	// Unknown mask field.
	r = f.call(f.vmQ, "on_patch_queue", "PATCH", path, params,
		map[string]string{"updateMask": "bogus"}, map[string]any{})
	if r.Status != 400 {
		t.Fatalf("unknown mask field: %d %v", r.Status, r.Body)
	}
}

func TestCTQueueIam(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)
	f.ctCreateQueue(t, map[string]any{})
	params := map[string]string{"project": ctProject, "location": ctLocation, "queue_verb": ctQueue + ":getIamPolicy"}

	r := f.call(f.vmQ, "on_queue_verb", "POST", "/v2/x", params, nil, map[string]any{})
	if r.Status != 200 {
		t.Fatalf("getIamPolicy: %d %v", r.Status, r.Body)
	}
	p1 := ctBody(t, r)
	etag1, _ := p1["etag"].(string)

	// The default etag is stable across reads.
	r = f.call(f.vmQ, "on_queue_verb", "POST", "/v2/x", params, nil, map[string]any{})
	if ctBody(t, r)["etag"] != etag1 {
		t.Fatalf("etag unstable: %v vs %v", ctBody(t, r)["etag"], etag1)
	}

	// setIamPolicy with the fresh etag wins.
	setParams := map[string]string{"project": ctProject, "location": ctLocation, "queue_verb": ctQueue + ":setIamPolicy"}
	r = f.call(f.vmQ, "on_queue_verb", "POST", "/v2/x", setParams, nil,
		map[string]any{"policy": map[string]any{
			"bindings": []any{map[string]any{"role": "roles/cloudtasks.enqueuer", "members": []any{"user:t@example.com"}}},
			"etag":     etag1,
		}})
	if r.Status != 200 {
		t.Fatalf("setIamPolicy: %d %v", r.Status, r.Body)
	}

	// The stale etag now conflicts.
	r = f.call(f.vmQ, "on_queue_verb", "POST", "/v2/x", setParams, nil,
		map[string]any{"policy": map[string]any{"etag": etag1}})
	if r.Status != 409 || ctErrStatus(t, r) != "ABORTED" {
		t.Fatalf("stale etag: %d %v", r.Status, r.Body)
	}

	// testIamPermissions echoes.
	testParams := map[string]string{"project": ctProject, "location": ctLocation, "queue_verb": ctQueue + ":testIamPermissions"}
	r = f.call(f.vmQ, "on_queue_verb", "POST", "/v2/x", testParams, nil,
		map[string]any{"permissions": []any{"cloudtasks.tasks.create"}})
	if r.Status != 200 {
		t.Fatalf("testIamPermissions: %d %v", r.Status, r.Body)
	}
	perms := ctBody(t, r)["permissions"].([]any)
	if len(perms) != 1 || perms[0] != "cloudtasks.tasks.create" {
		t.Fatalf("permissions echo: %v", perms)
	}
}

// --- tasks ---

func TestCTTaskCreateValidation(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)
	f.ctCreateQueue(t, map[string]any{})

	cases := []struct {
		name string
		task map[string]any
		want string
	}{
		{"no message_type", map[string]any{}, "INVALID_ARGUMENT"},
		{"bad url", map[string]any{"httpRequest": map[string]any{"url": "ftp://x"}}, "INVALID_ARGUMENT"},
		{"body on GET", map[string]any{"httpRequest": map[string]any{"url": "https://w.example/", "httpMethod": "GET", "body": "eA=="}}, "INVALID_ARGUMENT"},
		{"both targets", map[string]any{
			"httpRequest":          map[string]any{"url": "https://w.example/"},
			"appEngineHttpRequest": map[string]any{"relativeUri": "/x"},
		}, "INVALID_ARGUMENT"},
		{"appengine uri", map[string]any{"appEngineHttpRequest": map[string]any{"relativeUri": "nope"}}, "INVALID_ARGUMENT"},
	}
	for _, tc := range cases {
		r, _ := f.ctCreateTask(t, tc.task)
		if r.Status != 400 || ctErrStatus(t, r) != tc.want {
			t.Fatalf("%s: %d %v", tc.name, r.Status, r.Body)
		}
	}

	// Queue must exist.
	r := f.call(f.vmT, "on_create_task", "POST", "/v2/projects/demo/locations/us-central1/queues/ghost/tasks",
		map[string]string{"project": ctProject, "location": ctLocation, "queue": "ghost"}, nil,
		map[string]any{"task": map[string]any{"httpRequest": map[string]any{"url": "https://w.example/"}}})
	if r.Status != 404 || ctErrStatus(t, r) != "NOT_FOUND" {
		t.Fatalf("ghost queue: %d %v", r.Status, r.Body)
	}
}

func TestCTTaskCreateViewsAndDedup(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)
	f.ctCreateQueue(t, map[string]any{})

	// FULL view round-trips the payload.
	r, body := f.ctCreateTask(t, map[string]any{
		"name": ctQueueName() + "/tasks/email-1",
		"httpRequest": map[string]any{
			"url": "https://worker.example/h", "httpMethod": "POST",
			"headers": map[string]any{"Content-Type": "application/json"},
			"body":    "eyJvayI6dHJ1ZX0=",
			"oidcToken": map[string]any{
				"serviceAccountEmail": "sa@demo.iam.gserviceaccount.com",
				"audience":            "https://worker.example",
			},
		},
		"scheduleTime": "2030-01-01T00:00:00Z",
	})
	// (create defaults to BASIC; ask for FULL via the query param)
	r = f.call(f.vmT, "on_create_task", "POST", "/v2/"+ctQueueName()+"/tasks",
		map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue},
		map[string]string{"responseView": "FULL"},
		map[string]any{"task": map[string]any{
			"name":        ctQueueName() + "/tasks/email-2",
			"httpRequest": map[string]any{"url": "https://worker.example/h", "body": "eA=="},
		}})
	if r.Status != 200 {
		t.Fatalf("create FULL: %d %v", r.Status, r.Body)
	}
	body = ctBody(t, r)
	hr := body["httpRequest"].(map[string]any)
	if hr["body"] != "eA==" || body["view"] != "FULL" {
		t.Fatalf("FULL view dropped payload: %v", body)
	}
	_ = body

	// BASIC omits the body but keeps the rest.
	r = f.call(f.vmT, "on_get_task", "GET", "/v2/"+ctQueueName()+"/tasks/email-2",
		map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue, "task": "email-2"}, nil, nil)
	if r.Status != 200 {
		t.Fatalf("get: %d %v", r.Status, r.Body)
	}
	b := ctBody(t, r)
	if _, has := b["httpRequest"].(map[string]any)["body"]; has {
		t.Fatalf("BASIC view leaked body: %v", b)
	}
	if b["view"] != "BASIC" {
		t.Fatalf("view field: %v", b["view"])
	}

	// Custom-ID duplicate -> 409.
	r, _ = f.ctCreateTask(t, map[string]any{
		"name":        ctQueueName() + "/tasks/email-2",
		"httpRequest": map[string]any{"url": "https://w.example/"},
	})
	if r.Status != 409 || ctErrStatus(t, r) != "ALREADY_EXISTS" {
		t.Fatalf("dup task: %d %v", r.Status, r.Body)
	}

	// Generated IDs are unique 19-digit decimals.
	r, b1 := f.ctCreateTask(t, map[string]any{"httpRequest": map[string]any{"url": "https://w.example/"}})
	r2, b2 := f.ctCreateTask(t, map[string]any{"httpRequest": map[string]any{"url": "https://w.example/"}})
	if b1["name"] == b2["name"] {
		t.Fatalf("generated ids collided: %v", b1["name"])
	}
	if r.Status != 200 || r2.Status != 200 {
		t.Fatalf("generated creates: %d %d", r.Status, r2.Status)
	}

	// Past scheduleTime clamps to now.
	r, b3 := f.ctCreateTask(t, map[string]any{
		"httpRequest":  map[string]any{"url": "https://w.example/"},
		"scheduleTime": "2020-01-01T00:00:00Z",
	})
	if b3["scheduleTime"].(string) < b3["createTime"].(string) {
		t.Fatalf("past schedule not clamped: %v", b3)
	}
}

func TestCTTaskListPagination(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)
	f.ctCreateQueue(t, map[string]any{})
	params := map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue}

	for i := 0; i < 5; i++ {
		r, _ := f.ctCreateTask(t, map[string]any{"httpRequest": map[string]any{"url": "https://w.example/"}})
		if r.Status != 200 {
			t.Fatalf("create %d: %d %v", i, r.Status, r.Body)
		}
	}

	r := f.call(f.vmT, "on_list_tasks", "GET", "/v2/"+ctQueueName()+"/tasks", params,
		map[string]string{"pageSize": "2"}, nil)
	b := ctBody(t, r)
	if len(b["tasks"].([]any)) != 2 || b["nextPageToken"] != "2" {
		t.Fatalf("page 1: %v", b)
	}

	r = f.call(f.vmT, "on_list_tasks", "GET", "/v2/"+ctQueueName()+"/tasks", params,
		map[string]string{"pageSize": "2", "pageToken": "4"}, nil)
	b = ctBody(t, r)
	if len(b["tasks"].([]any)) != 1 {
		t.Fatalf("last page: %v", b)
	}
	if _, has := b["nextPageToken"]; has {
		t.Fatalf("unexpected token on last page: %v", b)
	}

	// Malformed token -> 400.
	r = f.call(f.vmT, "on_list_tasks", "GET", "/v2/"+ctQueueName()+"/tasks", params,
		map[string]string{"pageToken": "zzz"}, nil)
	if r.Status != 400 {
		t.Fatalf("bad token: %d %v", r.Status, r.Body)
	}
}

func TestCTTaskRunSuccessDeletes(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)
	f.ctCreateQueue(t, map[string]any{})

	r, _ := f.ctCreateTask(t, map[string]any{
		"name":        ctQueueName() + "/tasks/job-1",
		"httpRequest": map[string]any{"url": "https://w.example/"},
	})
	if r.Status != 200 {
		t.Fatalf("create: %d %v", r.Status, r.Body)
	}

	verb := map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue, "task_verb": "job-1:run"}
	r = f.call(f.vmT, "on_task_verb", "POST", "/v2/x", verb, nil,
		map[string]any{"responseView": "FULL"})
	if r.Status != 200 {
		t.Fatalf("run: %d %v", r.Status, r.Body)
	}
	b := ctBody(t, r)
	if ctNum(b["dispatchCount"]) != 1 || ctNum(b["responseCount"]) != 0 {
		t.Fatalf("post-run counters: %v", b)
	}
	last := b["lastAttempt"].(map[string]any)
	if _, has := last["responseStatus"]; has {
		t.Fatalf("success run must not carry responseStatus: %v", last)
	}
	first := b["firstAttempt"].(map[string]any)
	if _, has := first["dispatchTime"]; !has {
		t.Fatalf("firstAttempt.dispatchTime missing: %v", first)
	}

	// The completed task is gone; re-run -> 404; the ID is tombstoned.
	get := map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue, "task": "job-1"}
	r = f.call(f.vmT, "on_get_task", "GET", "/v2/x", get, nil, nil)
	if r.Status != 404 {
		t.Fatalf("get after success run: %d %v", r.Status, r.Body)
	}
	r = f.call(f.vmT, "on_task_verb", "POST", "/v2/x", verb, nil, map[string]any{})
	if r.Status != 404 {
		t.Fatalf("re-run completed: %d %v", r.Status, r.Body)
	}
	r, _ = f.ctCreateTask(t, map[string]any{
		"name":        ctQueueName() + "/tasks/job-1",
		"httpRequest": map[string]any{"url": "https://w.example/"},
	})
	if r.Status != 409 {
		t.Fatalf("tombstone recreate: %d %v", r.Status, r.Body)
	}
}

func TestCTTaskRunFailingWorkerRetries(t *testing.T) {
	// maxAttempts=3, minBackoff=10s, maxDoublings=2: retries at +10s, +20s,
	// then the third run exhausts attempts and permanently fails.
	f := newCTFixture(t, time.Unix(1770000000, 0), func() string { return "failing-worker" })
	f.ctCreateQueue(t, map[string]any{"retryConfig": map[string]any{"maxAttempts": 3, "minBackoff": "10s", "maxDoublings": 2}})

	r, _ := f.ctCreateTask(t, map[string]any{
		"name":        ctQueueName() + "/tasks/flaky",
		"httpRequest": map[string]any{"url": "https://w.example/", "body": "eA=="},
	})
	if r.Status != 200 {
		t.Fatalf("create: %d %v", r.Status, r.Body)
	}
	created := ctBody(t, r)["createTime"].(string)

	verb := map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue, "task_verb": "flaky:run"}
	r = f.call(f.vmT, "on_task_verb", "POST", "/v2/x", verb, nil,
		map[string]any{"responseView": "FULL"})
	if r.Status != 200 {
		t.Fatalf("run 1: %d %v", r.Status, r.Body)
	}
	b := ctBody(t, r)
	if ctNum(b["dispatchCount"]) != 1 || ctNum(b["responseCount"]) != 1 {
		t.Fatalf("run 1 counters: %v", b)
	}
	last := b["lastAttempt"].(map[string]any)
	status := last["responseStatus"].(map[string]any)
	if ctNum(status["code"]) != 500 {
		t.Fatalf("run 1 responseStatus: %v", status)
	}
	// Rescheduled: minBackoff=10s after createTime.
	if sched := b["scheduleTime"].(string); sched <= created {
		t.Fatalf("run 1 schedule not advanced: %v", sched)
	}

	// Run 2: still queued, +20s (doubled once).
	f.vc.Advance(10 * time.Second)
	r = f.call(f.vmT, "on_task_verb", "POST", "/v2/x", verb, nil, map[string]any{})
	b = ctBody(t, r)
	if ctNum(b["dispatchCount"]) != 2 {
		t.Fatalf("run 2 counters: %v", b)
	}
	prev := b["scheduleTime"].(string)
	if prev <= created {
		t.Fatalf("run 2 schedule not advanced: %v", prev)
	}

	// Run 3: attempts exhausted -> permanent failure, task deleted.
	f.vc.Advance(20 * time.Second)
	r = f.call(f.vmT, "on_task_verb", "POST", "/v2/x", verb, nil, map[string]any{})
	if r.Status != 200 {
		t.Fatalf("run 3: %d %v", r.Status, r.Body)
	}
	b = ctBody(t, r)
	if ctNum(b["dispatchCount"]) != 3 {
		t.Fatalf("run 3 counters: %v", b)
	}
	get := map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue, "task": "flaky"}
	if r = f.call(f.vmT, "on_get_task", "GET", "/v2/x", get, nil, nil); r.Status != 404 {
		t.Fatalf("permanently failed task still present: %d %v", r.Status, r.Body)
	}
}

func TestCTTaskRunFailingWorkerMaxRetryDuration(t *testing.T) {
	// Unlimited attempts but a 25s retry window measured from the first
	// attempt: after it passes, the next run permanently fails the task.
	f := newCTFixture(t, time.Unix(1770000000, 0), func() string { return "failing-worker" })
	f.ctCreateQueue(t, map[string]any{"retryConfig": map[string]any{
		"maxAttempts": -1, "maxRetryDuration": "25s", "minBackoff": "5s", "maxBackoff": "5s",
	}})
	r, _ := f.ctCreateTask(t, map[string]any{
		"name":        ctQueueName() + "/tasks/windowed",
		"httpRequest": map[string]any{"url": "https://w.example/"},
	})
	if r.Status != 200 {
		t.Fatalf("create: %d %v", r.Status, r.Body)
	}

	verb := map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue, "task_verb": "windowed:run"}
	for i := 0; i < 3; i++ {
		f.vc.Advance(5 * time.Second)
		if r = f.call(f.vmT, "on_task_verb", "POST", "/v2/x", verb, nil, map[string]any{}); r.Status != 200 {
			t.Fatalf("run %d: %d %v", i+1, r.Status, r.Body)
		}
	}
	// 20s elapsed since create; the first attempt was at +5s, so the age
	// limit (25s from first attempt) is not yet exhausted — task survives.
	get := map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue, "task": "windowed"}
	if r = f.call(f.vmT, "on_get_task", "GET", "/v2/x", get, nil, nil); r.Status != 200 {
		t.Fatalf("task should still be queued: %d %v", r.Status, r.Body)
	}
	// Push past the window: the next run permanently fails it.
	f.vc.Advance(15 * time.Second)
	if r = f.call(f.vmT, "on_task_verb", "POST", "/v2/x", verb, nil, map[string]any{}); r.Status != 200 {
		t.Fatalf("final run: %d %v", r.Status, r.Body)
	}
	if r = f.call(f.vmT, "on_get_task", "GET", "/v2/x", get, nil, nil); r.Status != 404 {
		t.Fatalf("task should be permanently failed: %d %v", r.Status, r.Body)
	}
}

func TestCTTaskDeleteAndTombstone(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)
	f.ctCreateQueue(t, map[string]any{})

	r, _ := f.ctCreateTask(t, map[string]any{
		"name":        ctQueueName() + "/tasks/gone",
		"httpRequest": map[string]any{"url": "https://w.example/"},
	})
	if r.Status != 200 {
		t.Fatalf("create: %d %v", r.Status, r.Body)
	}

	del := map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue, "task": "gone"}
	r = f.call(f.vmT, "on_delete_task", "DELETE", "/v2/x", del, nil, nil)
	if r.Status != 200 {
		t.Fatalf("delete: %d %v", r.Status, r.Body)
	}

	// Second delete -> 404; recreate within the window -> 409.
	if r = f.call(f.vmT, "on_delete_task", "DELETE", "/v2/x", del, nil, nil); r.Status != 404 {
		t.Fatalf("double delete: %d %v", r.Status, r.Body)
	}
	r, _ = f.ctCreateTask(t, map[string]any{
		"name":        ctQueueName() + "/tasks/gone",
		"httpRequest": map[string]any{"url": "https://w.example/"},
	})
	if r.Status != 409 {
		t.Fatalf("tombstone recreate: %d %v", r.Status, r.Body)
	}

	// The tombstone expires after 24h (86400s).
	f.vc.Advance(86401 * time.Second)
	r, _ = f.ctCreateTask(t, map[string]any{
		"name":        ctQueueName() + "/tasks/gone",
		"httpRequest": map[string]any{"url": "https://w.example/"},
	})
	if r.Status != 200 {
		t.Fatalf("tombstone should expire: %d %v", r.Status, r.Body)
	}
}

func TestCTPurge(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)
	f.ctCreateQueue(t, map[string]any{})

	r, _ := f.ctCreateTask(t, map[string]any{"httpRequest": map[string]any{"url": "https://w.example/"}})
	if r.Status != 200 {
		t.Fatalf("create: %d %v", r.Status, r.Body)
	}

	purge := map[string]string{"project": ctProject, "location": ctLocation, "queue_verb": ctQueue + ":purge"}
	r = f.call(f.vmQ, "on_queue_verb", "POST", "/v2/x", purge, nil, map[string]any{})
	if r.Status != 200 {
		t.Fatalf("purge: %d %v", r.Status, r.Body)
	}

	params := map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue}
	r = f.call(f.vmT, "on_list_tasks", "GET", "/v2/"+ctQueueName()+"/tasks", params, nil, nil)
	if got := len(ctBody(t, r)["tasks"].([]any)); got != 0 {
		t.Fatalf("purge left %d tasks", got)
	}

	// purgeTime is stamped; a task created after the purge survives.
	r = f.call(f.vmQ, "on_get_queue", "GET", "/v2/"+ctQueueName(),
		map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue}, nil, nil)
	if _, has := ctBody(t, r)["purgeTime"]; !has {
		t.Fatalf("purgeTime not stamped: %v", r.Body)
	}
	r, _ = f.ctCreateTask(t, map[string]any{"httpRequest": map[string]any{"url": "https://w.example/"}})
	if r.Status != 200 {
		t.Fatalf("create after purge: %d %v", r.Status, r.Body)
	}
	r = f.call(f.vmT, "on_list_tasks", "GET", "/v2/"+ctQueueName()+"/tasks", params, nil, nil)
	if got := len(ctBody(t, r)["tasks"].([]any)); got != 1 {
		t.Fatalf("post-purge task missing: %d", got)
	}
}

func TestCTBuffer(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)
	// The queue needs an httpTarget with a uriOverride host.
	f.ctCreateQueue(t, map[string]any{"httpTarget": map[string]any{
		"uriOverride": map[string]any{
			"host":          "worker.example",
			"scheme":        "HTTPS",
			"pathOverride":  map[string]any{"path": "/run"},
			"queryOverride": map[string]any{"queryParams": "src=buffer"},
		},
	}})

	// Without httpTarget -> 400 (fresh queue in another location).
	r := f.call(f.vmQ, "on_create_queue", "POST", "/v2/projects/demo/locations/us-east1/queues",
		map[string]string{"project": ctProject, "location": "us-east1"},
		map[string]string{"queueId": "plain"}, map[string]any{})
	if r.Status != 200 {
		t.Fatalf("plain queue: %d %v", r.Status, r.Body)
	}
	r = f.call(f.vmT, "on_buffer_task", "POST", "/v2/x",
		map[string]string{"project": ctProject, "location": "us-east1", "queue": "plain"}, nil,
		map[string]any{"body": map[string]any{"contentType": "application/json", "data": "e30="}})
	if r.Status != 400 || ctErrStatus(t, r) != "INVALID_ARGUMENT" {
		t.Fatalf("buffer without httpTarget: %d %v", r.Status, r.Body)
	}

	// Generated-ID form.
	params := map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue}
	r = f.call(f.vmT, "on_buffer_task", "POST", "/v2/x", params, nil,
		map[string]any{"body": map[string]any{"contentType": "application/json", "data": "e30="}})
	if r.Status != 200 {
		t.Fatalf("buffer: %d %v", r.Status, r.Body)
	}
	task := ctBody(t, r)["task"].(map[string]any)
	hr := task["httpRequest"].(map[string]any)
	if hr["url"] != "https://worker.example/run?src=buffer" {
		t.Fatalf("buffer url: %v", hr["url"])
	}
	if hr["body"] != "e30=" || hr["httpMethod"] != "POST" {
		t.Fatalf("buffer request: %v", hr)
	}
	if task["view"] != "FULL" {
		t.Fatalf("buffer view: %v", task["view"])
	}

	// Custom-ID form, and its duplicate.
	r = f.call(f.vmT, "on_task_verb", "POST", "/v2/x",
		map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue, "task_verb": "buf-42:buffer"},
		nil, map[string]any{"body": map[string]any{"data": "eA=="}})
	if r.Status != 200 {
		t.Fatalf("buffer custom id: %d %v", r.Status, r.Body)
	}
	if ctBody(t, r)["task"].(map[string]any)["name"] != ctQueueName()+"/tasks/buf-42" {
		t.Fatalf("buffer custom id name: %v", ctBody(t, r))
	}
	r = f.call(f.vmT, "on_task_verb", "POST", "/v2/x",
		map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue, "task_verb": "buf-42:buffer"},
		nil, map[string]any{"body": map[string]any{"data": "eA=="}})
	if r.Status != 409 {
		t.Fatalf("buffer dup: %d %v", r.Status, r.Body)
	}
}

func TestCTDeleteQueueCascades(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)
	f.ctCreateQueue(t, map[string]any{})
	r, _ := f.ctCreateTask(t, map[string]any{"httpRequest": map[string]any{"url": "https://w.example/"}})
	if r.Status != 200 {
		t.Fatalf("create: %d %v", r.Status, r.Body)
	}

	r = f.call(f.vmQ, "on_delete_queue", "DELETE", "/v2/"+ctQueueName(),
		map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue}, nil, nil)
	if r.Status != 200 {
		t.Fatalf("delete queue: %d %v", r.Status, r.Body)
	}
	if r = f.call(f.vmQ, "on_delete_queue", "DELETE", "/v2/"+ctQueueName(),
		map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue}, nil, nil); r.Status != 404 {
		t.Fatalf("double delete queue: %d %v", r.Status, r.Body)
	}

	// Recreating the queue starts empty; task names were removed with it.
	f.ctCreateQueue(t, map[string]any{})
	params := map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue}
	r = f.call(f.vmT, "on_list_tasks", "GET", "/v2/"+ctQueueName()+"/tasks", params, nil, nil)
	if got := len(ctBody(t, r)["tasks"].([]any)); got != 0 {
		t.Fatalf("cascade left %d tasks", got)
	}
}

func TestCTMalformedJSON(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)
	f.ctCreateQueue(t, map[string]any{})

	// A body that is present but not valid JSON gets the API's message.
	params := map[string]string{"project": ctProject, "location": ctLocation, "queue": ctQueue}
	resp, err := f.vmT.Call("on_create_task", starlark.Request{
		Method:  "POST",
		Path:    "/v2/" + ctQueueName() + "/tasks",
		Host:    "cloudtasks.stunt.test",
		Headers: map[string]string{"Authorization": "Bearer t"},
		RawBody: `{"task":{"httpRequest":{"url":"https://w.example/p"}}`,
		Params:  params,
		Query:   map[string]string{},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Status != 400 || ctErrStatus(t, resp) != "INVALID_ARGUMENT" {
		t.Fatalf("malformed JSON: %d %v", resp.Status, resp.Body)
	}
}

func TestCTAuthRequired(t *testing.T) {
	f := newCTFixture(t, time.Unix(1770000000, 0), nil)

	resp, err := f.vmL.Call("on_list_locations", starlark.Request{
		Method: "GET",
		Path:   "/v2/projects/demo/locations",
		Host:   "cloudtasks.stunt.test",
		Params: map[string]string{"project": ctProject},
		Query:  map[string]string{},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Status != 401 || ctErrStatus(t, resp) != "UNAUTHENTICATED" {
		t.Fatalf("no bearer: %d %v", resp.Status, resp.Body)
	}
}
