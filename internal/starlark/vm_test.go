package starlark

import (
	"strings"
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

// --- LoadWithLib: shared-library preload mechanism ---

// TestLoadWithLibHelperWorks verifies that a handler can use a helper defined
// in lib.star when libSrc is passed to LoadWithLib.
func TestLoadWithLibHelperWorks(t *testing.T) {
	libSrc := `
def _greet(name):
    return "hello " + name
`
	src := `
def on_get(req):
    return respond(200, {"msg": _greet(req["body"]["name"])})
`
	vm, err := LoadWithLib(src, libSrc, nil)
	if err != nil {
		t.Fatalf("LoadWithLib: %v", err)
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

// TestLoadWithLibEmptyDelegatesToLoad verifies that empty libSrc behaves
// exactly like Load — no error, handler works normally.
func TestLoadWithLibEmptyDelegatesToLoad(t *testing.T) {
	src := `
def on_get(req):
    return respond(200, {"ok": True})
`
	vm, err := LoadWithLib(src, "", nil)
	if err != nil {
		t.Fatalf("LoadWithLib: %v", err)
	}

	resp, err := vm.Call("on_get", Request{Method: "GET"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("Status = %d, want 200", resp.Status)
	}
}

// TestLoadWithLibErrorSurfaces verifies that a syntax/runtime error in
// lib.star produces a clear error rather than silently succeeding.
func TestLoadWithLibErrorSurfaces(t *testing.T) {
	libSrc := `
def broken(
` // missing closing paren
	src := `
def on_get(req):
    return respond(200, {})
`
	_, err := LoadWithLib(src, libSrc, nil)
	if err == nil {
		t.Fatal("expected error from broken lib.star, got nil")
	}
	// The error should mention the lib so it's distinguishable.
	if !strings.Contains(err.Error(), "lib") {
		t.Fatalf("error should mention lib, got: %v", err)
	}
}

// TestLoadWithLibStepLimitApplies verifies that the step limit applies to
// lib.star too — an infinite loop in the lib cannot hang the load.
func TestLoadWithLibStepLimitApplies(t *testing.T) {
	libSrc := `
while True:
    pass
`
	src := `
def on_get(req):
    return respond(200, {})
`
	done := make(chan error, 1)
	go func() {
		_, err := LoadWithLib(src, libSrc, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from infinite loop in lib, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadWithLib did not return within 5s; step limit not enforced on lib")
	}
}

// TestLoadWithLibLibGlobalsFrozen verifies that the handler cannot mutate
// a global defined in lib.star (the lib globals are frozen before injection).
func TestLoadWithLibLibGlobalsFrozen(t *testing.T) {
	libSrc := `
_counter = 0
`
	src := `
def on_get(req):
    _counter = _counter + 1
    return respond(200, {"count": _counter})
`
	vm, err := LoadWithLib(src, libSrc, nil)
	if err != nil {
		t.Fatalf("LoadWithLib: %v", err)
	}

	_, err = vm.Call("on_get", Request{Method: "GET"})
	if err == nil {
		t.Fatal("expected frozen-global error when handler modifies lib global, got nil")
	}
}

// TestLoadWithLibUsesBuiltins verifies that lib functions can call builtins
// passed via the builtins dict.
func TestLoadWithLibUsesBuiltins(t *testing.T) {
	libSrc := `
def _double(name):
    return greet(name) + " " + greet(name)
`
	src := `
def on_get(req):
    return respond(200, {"msg": _double(req["body"]["name"])})
`
	builtins := sk.StringDict{
		"greet": sk.NewBuiltin("greet", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, _ []sk.Tuple) (sk.Value, error) {
			var name string
			if err := sk.UnpackArgs("greet", args, nil, "name", &name); err != nil {
				return nil, err
			}
			return sk.String("hi " + name), nil
		}),
	}

	vm, err := LoadWithLib(src, libSrc, builtins)
	if err != nil {
		t.Fatalf("LoadWithLib: %v", err)
	}

	resp, err := vm.Call("on_get", Request{
		Method: "GET",
		Body:   map[string]any{"name": "World"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["msg"] != "hi World hi World" {
		t.Fatalf("Body[msg] = %v, want 'hi World hi World'", resp.Body["msg"])
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

// --- CallRaw / CallWithMaxSteps / ValueToGo / StarlarkListToGo unit tests ---

// TestCallRawReturnsRawValue verifies that CallRaw returns the raw Starlark
// value without converting it to a Response. A handler returning a list
// should give us a *sk.List, not a Response.
func TestCallRawReturnsRawValue(t *testing.T) {
	src := `
def resolver(args):
    return [1, 2, 3]
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	result, err := vm.CallRaw("resolver", sk.None)
	if err != nil {
		t.Fatalf("CallRaw: %v", err)
	}

	list, ok := result.(*sk.List)
	if !ok {
		t.Fatalf("expected *sk.List, got %T", result)
	}
	if list.Len() != 3 {
		t.Fatalf("list len = %d, want 3", list.Len())
	}
}

// TestCallRawReturnsNone verifies that CallRaw returns sk.None when the
// handler has no explicit return.
func TestCallRawReturnsNone(t *testing.T) {
	src := `
def handler(args):
    x = 1 + 1
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	result, err := vm.CallRaw("handler", sk.None)
	if err != nil {
		t.Fatalf("CallRaw: %v", err)
	}
	if _, ok := result.(sk.NoneType); !ok {
		t.Fatalf("expected sk.NoneType, got %T (%v)", result, result)
	}
}

// TestCallRawUndefinedHandler verifies that CallRaw errors on an undefined
// handler name.
func TestCallRawUndefinedHandler(t *testing.T) {
	src := `
def on_get(req):
    return respond(200, {})
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = vm.CallRaw("nonexistent", sk.None)
	if err == nil {
		t.Fatal("expected error for undefined handler in CallRaw")
	}
}

// TestCallRawNotCallable verifies that CallRaw errors when the named global
// is not callable.
func TestCallRawNotCallable(t *testing.T) {
	src := `
my_var = 42
def on_get(req):
    return respond(200, {})
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = vm.CallRaw("my_var", sk.None)
	if err == nil {
		t.Fatal("expected error for non-callable in CallRaw")
	}
}

// TestCallWithMaxSteps verifies that CallWithMaxSteps works correctly with a
// custom step budget, and that the None-return convention (200 OK) is
// respected.
func TestCallWithMaxSteps(t *testing.T) {
	src := `
def handler(stream):
    x = 0
    for i in range(100):
        x = x + i
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.CallWithMaxSteps("handler", sk.None, 50000)
	if err != nil {
		t.Fatalf("CallWithMaxSteps: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200 (None return convention)", resp.Status)
	}
}

// TestCallWithMaxStepsExceeded verifies that exceeding the step budget
// produces an error rather than hanging.
func TestCallWithMaxStepsExceeded(t *testing.T) {
	src := `
def handler(stream):
    x = 0
    while True:
        x = x + 1
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := vm.CallWithMaxSteps("handler", sk.None, 1000)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected step-limit error, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CallWithMaxSteps did not return; step limit not enforced")
	}
}

// TestCallWithMaxStepsUndefinedHandler verifies that CallWithMaxSteps errors
// on an undefined handler name.
func TestCallWithMaxStepsUndefinedHandler(t *testing.T) {
	src := `
def on_get(req):
    return respond(200, {})
`
	vm, err := Load(src, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = vm.CallWithMaxSteps("nonexistent", sk.None, 1000)
	if err == nil {
		t.Fatal("expected error for undefined handler in CallWithMaxSteps")
	}
}

// TestValueToGoScalar verifies ValueToGo on basic scalar types.
func TestValueToGoScalar(t *testing.T) {
	tests := []struct {
		name string
		val  sk.Value
		want any
	}{
		{"string", sk.String("hello"), "hello"},
		{"int", sk.MakeInt(42), int64(42)},
		{"bool_true", sk.Bool(true), true},
		{"bool_false", sk.Bool(false), false},
		{"float", sk.Float(3.14), 3.14},
		{"none", sk.None, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValueToGo(tc.val)
			if err != nil {
				t.Fatalf("ValueToGo: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v (%T), want %v (%T)", got, got, tc.want, tc.want)
			}
		})
	}
}

// TestStarlarkListToGo verifies StarlarkListToGo converts a Starlark list to
// a Go []any with recursive element conversion.
func TestStarlarkListToGo(t *testing.T) {
	elems := []sk.Value{
		sk.MakeInt(1),
		sk.String("two"),
		sk.Bool(true),
		sk.None,
	}
	list := sk.NewList(elems)

	got, err := StarlarkListToGo(list)
	if err != nil {
		t.Fatalf("StarlarkListToGo: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	if got[0] != int64(1) {
		t.Errorf("elem 0 = %v (%T), want int64(1)", got[0], got[0])
	}
	if got[1] != "two" {
		t.Errorf("elem 1 = %v, want 'two'", got[1])
	}
	if got[2] != true {
		t.Errorf("elem 2 = %v, want true", got[2])
	}
	if got[3] != nil {
		t.Errorf("elem 3 = %v, want nil", got[3])
	}
}

// TestStarlarkToGo verifies StarlarkToGo converts a dict correctly,
// including nested structures.
func TestStarlarkToGo(t *testing.T) {
	d := sk.NewDict(2)
	d.SetKey(sk.String("name"), sk.String("Alice"))
	d.SetKey(sk.String("age"), sk.MakeInt(30))

	got, err := StarlarkToGo(d)
	if err != nil {
		t.Fatalf("StarlarkToGo: %v", err)
	}
	if got["name"] != "Alice" {
		t.Errorf("name = %v, want Alice", got["name"])
	}
	if got["age"] != int64(30) {
		t.Errorf("age = %v (%T), want int64(30)", got["age"], got["age"])
	}
}
