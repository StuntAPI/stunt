package starlark

import (
	"sync"
	"testing"
	"time"

	sk "go.starlark.net/starlark"
)

func TestRespondBuiltin(t *testing.T) {
	src := `
def on_post(req):
    return respond(201, {"id": req["body"]["name"]})
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_post", Request{
		Method: "POST",
		Body:   map[string]any{"name": "Sam"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if resp.Status != 201 {
		t.Fatalf("Status = %d, want 201", resp.Status)
	}
	if resp.Body["id"] != "Sam" {
		t.Fatalf("Body[id] = %v, want Sam", resp.Body["id"])
	}
}

func TestRawDictReturn(t *testing.T) {
	src := `
def on_get(req):
    return {"status": 200, "body": {"ok": True}}
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_get", Request{Method: "GET"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if resp.Status != 200 {
		t.Fatalf("Status = %d, want 200", resp.Status)
	}
	if resp.Body["ok"] != true {
		t.Fatalf("Body[ok] = %v (%T), want true", resp.Body["ok"], resp.Body["ok"])
	}
}

func TestUndefinedHandler(t *testing.T) {
	src := `
def on_get(req):
    return respond(200, {})
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	_, err = vm.Call("nonexistent", Request{Method: "GET"})
	if err == nil {
		t.Fatal("expected error for undefined handler, got nil")
	}
}

func TestDefaultStatusWhenOmitted(t *testing.T) {
	src := `
def on_get(req):
    return respond(body={"ok": True})
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_get", Request{Method: "GET"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if resp.Status != 200 {
		t.Fatalf("Status = %d, want 200 (default)", resp.Status)
	}
}

func TestSandboxNoForbiddenBuiltin(t *testing.T) {
	// Starlark has no `open` by default. A script that references it fails to
	// load because the name is undefined — the sandbox prevents file I/O.
	src := `
def on_get(req):
    f = open("/etc/passwd")
    return respond(200, {})
`
	_, err := Load(src, nil)
	if err == nil {
		t.Fatal("expected error loading script that references forbidden builtin `open`, got nil")
	}
}

func TestHeadersFromRespond(t *testing.T) {
	src := `
def on_get(req):
    return respond(200, {"ok": True}, {"Content-Type": "application/json"})
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_get", Request{Method: "GET"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if resp.Headers["Content-Type"] != "application/json" {
		t.Fatalf("Headers[Content-Type] = %v, want application/json", resp.Headers["Content-Type"])
	}
}

func TestRequestBodyFields(t *testing.T) {
	src := `
def on_post(req):
    return respond(200, {
        "method": req["method"],
        "path": req["path"],
        "auth": req["headers"]["Authorization"],
    })
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_post", Request{
		Method:  "POST",
		Path:    "/items",
		Headers: map[string]string{"Authorization": "Bearer token123"},
		Body:    map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if resp.Body["method"] != "POST" {
		t.Fatalf("Body[method] = %v, want POST", resp.Body["method"])
	}
	if resp.Body["path"] != "/items" {
		t.Fatalf("Body[path] = %v, want /items", resp.Body["path"])
	}
	if resp.Body["auth"] != "Bearer token123" {
		t.Fatalf("Body[auth] = %v, want Bearer token123", resp.Body["auth"])
	}
}

func TestCustomBuiltin(t *testing.T) {
	// A caller can inject their own builtins alongside respond.
	builtins := sk.StringDict{
		"greet": sk.NewBuiltin("greet", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, _ []sk.Tuple) (sk.Value, error) {
			var name string
			if err := sk.UnpackArgs("greet", args, nil, "name", &name); err != nil {
				return nil, err
			}
			return sk.String("hello " + name), nil
		}),
	}

	src := `
def on_get(req):
    return respond(200, {"msg": greet(req["body"]["name"])})
`
	vm, err := Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_get", Request{
		Method: "GET",
		Body:   map[string]any{"name": "World"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if resp.Body["msg"] != "hello World" {
		t.Fatalf("Body[msg] = %v, want 'hello World'", resp.Body["msg"])
	}
}

// --- C1: concurrent calls must not share thread state ---

// TestConcurrentCall exercises the VM under concurrent requests to prove the
// per-call thread (C1) avoids data races / segfaults.
func TestConcurrentCall(t *testing.T) {
	src := `
def on_get(req):
    return respond(200, {"ok": True})
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := vm.Call("on_get", Request{Method: "GET"})
			if err != nil {
				t.Errorf("Call: %v", err)
				return
			}
			if resp.Status != 200 {
				t.Errorf("Status = %d, want 200", resp.Status)
			}
		}()
	}
	wg.Wait()
}

// --- I1: globals are frozen, so a handler modifying a global gets an error,
// not a crash ---

func TestFrozenGlobalError(t *testing.T) {
	src := `
counter = 0
def on_get(req):
    counter = counter + 1
    return respond(200, {"count": counter})
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	_, err = vm.Call("on_get", Request{Method: "GET"})
	if err == nil {
		t.Fatal("expected error when modifying frozen global, got nil")
	}
}

// TestFrozenGlobalConcurrent proves that even with frozen globals, concurrent
// calls are race-free (run with -race).
func TestFrozenGlobalConcurrent(t *testing.T) {
	src := `
counter = 0
def on_get(req):
    return respond(200, {"count": counter})
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := vm.Call("on_get", Request{Method: "GET"})
			if err != nil {
				t.Errorf("Call: %v", err)
			}
		}()
	}
	wg.Wait()
}

// --- I5: infinite loop is bounded by max execution steps ---

func TestInfiniteLoopBounded(t *testing.T) {
	src := `
def loop():
    return loop()

def on_get(req):
    return loop()
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := vm.Call("on_get", Request{Method: "GET"})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from infinite loop, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return within 5 seconds; step limit not enforced")
	}
}

// --- M8: non-int status returns an error ---

// --- stunt-sec: top-level while-True must be bounded at load time ---

func TestLoadWhileTrueBoundedByStepLimit(t *testing.T) {
	// A malicious adapter with a top-level while True: pass must be caught
	// by the step limit during Load (not during a handler call).
	src := `
while True:
    pass

def on_get(req):
    return respond(200, {})
`
	done := make(chan error, 1)
	go func() {
		_, err := Load(src, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from top-level while True, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load did not return within 5 seconds; load-phase step limit not enforced")
	}
}

// TestLoadWhileTrueWithLargeLoop also proves the step limit catches a
// more realistic slow-burn loop that increments a counter forever.
func TestLoadWhileTrueWithLargeLoop(t *testing.T) {
	src := `
x = 0
while x < 1000000000:
    x = x + 1

def on_get(req):
    return respond(200, {"x": x})
`
	done := make(chan error, 1)
	go func() {
		_, err := Load(src, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from infinite load-time loop, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load did not return within 5 seconds; load-phase step limit not enforced")
	}
}

func TestNonIntStatusError(t *testing.T) {
	src := `
def on_get(req):
    return {"status": "not-an-int", "body": {}}
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	_, err = vm.Call("on_get", Request{Method: "GET"})
	if err == nil {
		t.Fatal("expected error for non-int status, got nil")
	}
}
