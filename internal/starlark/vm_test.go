package starlark

import (
	"testing"

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
