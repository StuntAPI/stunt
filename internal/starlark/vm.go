package starlark

import (
	"fmt"

	sk "go.starlark.net/starlark"
)

// Request is the Go-friendly representation of an incoming HTTP request
// passed into a Starlark handler.
type Request struct {
	Method  string
	Path    string
	Headers map[string]string
	Body    map[string]any
	Params  map[string]string // path params extracted from route match
}

// Response is the Go-friendly representation of the HTTP response produced
// by a Starlark handler.
type Response struct {
	Status  int
	Headers map[string]string
	Body    map[string]any
}

// VM wraps a Starlark thread plus the globals defined by a loaded script.
// Each Load call produces a fresh VM; handlers are invoked via Call.
type VM struct {
	globals sk.StringDict
	thread  *sk.Thread
}

// Load parses and executes src at top-level, capturing the global bindings
// (including handler functions like on_get, on_post). A default respond
// builtin is injected unless the caller overrides it via builtins.
//
// respond(status=200, body=None, headers=None) returns a dict suitable as
// a handler return value. If status is omitted it defaults to 200.
func Load(src string, builtins sk.StringDict) (*VM, error) {
	predeclared := sk.StringDict{
		"respond": sk.NewBuiltin("respond", respondBuiltin),
	}
	// Caller-provided builtins override or extend the defaults.
	for k, v := range builtins {
		predeclared[k] = v
	}

	thread := &sk.Thread{
		Name: "stunt",
	}

	globals, err := sk.ExecFile(thread, "handler.star", src, predeclared)
	if err != nil {
		return nil, fmt.Errorf("starlark load: %w", err)
	}

	return &VM{globals: globals, thread: thread}, nil
}

// Call invokes the named handler function with a Request and converts the
// returned Starlark value into a Response. The handler must return either a
// dict (e.g. from respond) or any value convertible to a Response dict.
func (vm *VM) Call(handlerName string, req Request) (Response, error) {
	fn, ok := vm.globals[handlerName]
	if !ok {
		return Response{}, fmt.Errorf("starlark: handler %q is not defined", handlerName)
	}

	if _, ok := fn.(sk.Callable); !ok {
		return Response{}, fmt.Errorf("starlark: %q is not callable", handlerName)
	}

	reqVal := GoToStarlark(map[string]any{
		"method":  req.Method,
		"path":    req.Path,
		"headers": req.Headers,
		"body":    req.Body,
		"params":  req.Params,
	})

	result, err := sk.Call(vm.thread, fn, sk.Tuple{reqVal}, nil)
	if err != nil {
		return Response{}, fmt.Errorf("starlark: call %q: %w", handlerName, err)
	}

	return starlarkToResponse(result)
}

// respondBuiltin implements respond(status=200, body=None, headers=None).
// It returns a Starlark dict with keys "status", "body", and "headers".
func respondBuiltin(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var status sk.Value = sk.MakeInt(200)
	var body sk.Value = sk.None
	var headers sk.Value = sk.None

	if err := sk.UnpackArgs("respond", args, kwargs, "status?", &status, "body?", &body, "headers?", &headers); err != nil {
		return nil, err
	}

	d := sk.NewDict(3)
	d.SetKey(sk.String("status"), status)
	d.SetKey(sk.String("body"), body)
	d.SetKey(sk.String("headers"), headers)
	return d, nil
}

// starlarkToResponse converts the value returned by a handler into a Response.
// Expected shape: a dict with optional keys "status" (int, default 200),
// "body" (dict), and "headers" (dict of string→string).
func starlarkToResponse(v sk.Value) (Response, error) {
	resp := Response{Status: 200}

	d, ok := v.(*sk.Dict)
	if !ok {
		return Response{}, fmt.Errorf("starlark: handler must return a dict, got %s", v.Type())
	}

	for _, item := range d.Items() {
		key, _ := sk.AsString(item[0])
		val := item[1]

		switch key {
		case "status":
			if status, err := sk.AsInt32(val); err == nil {
				resp.Status = status
			}
		case "body":
			if bd, ok := val.(*sk.Dict); ok {
				resp.Body = StarlarkToGo(bd)
			}
		case "headers":
			if hd, ok := val.(*sk.Dict); ok {
				resp.Headers = starlarkToStringMap(hd)
			}
		}
	}

	return resp, nil
}

// --- conversion helpers: Go → Starlark ---

// GoToStarlark converts a Go value into the equivalent Starlark value.
// Supports string, int, int64, float64, bool, nil, map[string]any, and
// slices of any of these.
func GoToStarlark(v any) sk.Value {
	switch x := v.(type) {
	case nil:
		return sk.None
	case string:
		return sk.String(x)
	case bool:
		return sk.Bool(x)
	case int:
		return sk.MakeInt(x)
	case int64:
		return sk.MakeInt64(x)
	case float64:
		return sk.Float(x)
	case map[string]any:
		d := sk.NewDict(len(x))
		for k, val := range x {
			d.SetKey(sk.String(k), GoToStarlark(val))
		}
		return d
	case map[string]string:
		d := sk.NewDict(len(x))
		for k, val := range x {
			d.SetKey(sk.String(k), sk.String(val))
		}
		return d
	case []any:
		elems := make([]sk.Value, len(x))
		for i, val := range x {
			elems[i] = GoToStarlark(val)
		}
		return sk.NewList(elems)
	default:
		return sk.String(fmt.Sprintf("%v", v))
	}
}

// --- conversion helpers: Starlark → Go ---

// StarlarkToGo converts a Starlark dict into a Go map[string]any.
func StarlarkToGo(d *sk.Dict) map[string]any {
	out := make(map[string]any, d.Len())
	for _, item := range d.Items() {
		key, _ := sk.AsString(item[0])
		out[key] = starlarkValueToGo(item[1])
	}
	return out
}

// starlarkValueToGo converts an arbitrary Starlark value into a Go value.
func starlarkValueToGo(v sk.Value) any {
	switch x := v.(type) {
	case sk.NoneType:
		return nil
	case sk.String:
		return string(x)
	case sk.Bool:
		return bool(x)
	case sk.Int:
		n, _ := x.Int64()
		return n
	case sk.Float:
		return float64(x)
	case *sk.Dict:
		return StarlarkToGo(x)
	case *sk.List:
		out := make([]any, x.Len())
		for i := range out {
			out[i] = starlarkValueToGo(x.Index(i))
		}
		return out
	default:
		return v.String()
	}
}

// starlarkToStringMap converts a Starlark dict of string→string into a Go
// map[string]string, stringifying non-string values.
func starlarkToStringMap(d *sk.Dict) map[string]string {
	out := make(map[string]string, d.Len())
	for _, item := range d.Items() {
		key, _ := sk.AsString(item[0])
		val, ok := sk.AsString(item[1])
		if !ok {
			val = item[1].String()
		}
		out[key] = val
	}
	return out
}
