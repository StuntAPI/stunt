package starlark

import (
	"fmt"

	sk "go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// maxExecutionSteps bounds the number of Starlark VM steps per handler call
// to prevent infinite loops from blocking the server indefinitely (I5).
const maxExecutionSteps = 1_000_000

// Request is the Go-friendly representation of an incoming HTTP request
// passed into a Starlark handler.
type Request struct {
	Method  string
	Path    string
	Headers map[string]string
	Body    map[string]any
	Params  map[string]string // path params extracted from route match
	Query   map[string]string // query parameters (first value of each key)
}

// Response is the Go-friendly representation of the HTTP response produced
// by a Starlark handler.
type Response struct {
	Status  int
	Headers map[string]string
	Body    map[string]any
	RawBody string // raw text body (when handler returns a string body)
}

// VM wraps the globals defined by a loaded script. Each Load call produces
// a fresh VM; handlers are invoked via Call, which creates a fresh thread
// per invocation so the VM is safe for concurrent use. The globals dict is
// frozen after load to prevent handlers from mutating shared state.
type VM struct {
	globals sk.StringDict
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

	loadThread := &sk.Thread{
		Name: "stunt-load",
	}
	loadThread.SetMaxExecutionSteps(maxExecutionSteps)

	// Enable while-loops (a non-standard Starlark extension) so that
	// streaming handlers can drain inbound streams naturally:
	//
	//   while True:
	//       m = stream.recv()
	//       if m == None: break
	//
	// Recursion remains disabled to preserve the compile-time recursion
	// check that prevents unbounded stack growth. While-loops are safe
	// because each iteration is bounded by SetMaxExecutionSteps.
	opts := &syntax.FileOptions{
		While: true,
	}
	globals, err := sk.ExecFileOptions(opts, loadThread, "handler.star", src, predeclared)
	if err != nil {
		return nil, fmt.Errorf("starlark load: %w", err)
	}

	// Freeze all globals so handlers cannot mutate shared state, preventing
	// fatal concurrent-map-write panics under concurrent requests (I1).
	for _, v := range globals {
		v.Freeze()
	}

	return &VM{globals: globals}, nil
}

// Has reports whether the named global is defined in the loaded script.
// This is used by `stunt plan` to verify that a handler function exists
// before starting the server.
func (vm *VM) Has(name string) bool {
	_, ok := vm.globals[name]
	return ok
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
		"query":   req.Query,
	})

	// Create a fresh thread per Call so concurrent requests don't share
	// mutable state (C1). Enforce a step limit to prevent infinite loops (I5).
	thread := &sk.Thread{
		Name: "stunt",
	}
	thread.SetMaxExecutionSteps(maxExecutionSteps)

	result, err := sk.Call(thread, fn, sk.Tuple{reqVal}, nil)
	if err != nil {
		return Response{}, fmt.Errorf("starlark: call %q: %w", handlerName, err)
	}

	return starlarkToResponse(result)
}

// CallWith invokes the named handler with a pre-built Starlark value as its
// sole argument and returns the result as a Response. This is used by
// streaming gRPC handlers that receive a stream object rather than a
// Request. A None return (implicit when the handler has no return statement)
// is treated as a 200 OK with no body — the streaming convention.
func (vm *VM) CallWith(handlerName string, arg sk.Value) (Response, error) {
	return vm.CallWithMaxSteps(handlerName, arg, maxExecutionSteps)
}

// CallRaw invokes the named handler with a pre-built Starlark value and
// returns the raw Starlark result without converting it to a Response.
// This is used by GraphQL resolvers, where the return value can be a list,
// scalar, or dict — not just the respond(...) convention. A None return
// is returned as sk.None; the caller decides how to interpret it.
func (vm *VM) CallRaw(handlerName string, arg sk.Value) (sk.Value, error) {
	fn, ok := vm.globals[handlerName]
	if !ok {
		return nil, fmt.Errorf("starlark: handler %q is not defined", handlerName)
	}

	if _, ok := fn.(sk.Callable); !ok {
		return nil, fmt.Errorf("starlark: %q is not callable", handlerName)
	}

	thread := &sk.Thread{
		Name: "stunt",
	}
	thread.SetMaxExecutionSteps(maxExecutionSteps)

	result, err := sk.Call(thread, fn, sk.Tuple{arg}, nil)
	if err != nil {
		return nil, fmt.Errorf("starlark: call %q: %w", handlerName, err)
	}
	return result, nil
}

// CallWithMaxSteps is like CallWith but allows the caller to specify a
// custom step budget. WebSocket handlers use an elevated limit because they
// run for the lifetime of a connection and may legitimately exchange many
// messages, each with moderate per-message work. The blocking recv() call
// accrues no steps while waiting, so idle connections are unaffected.
func (vm *VM) CallWithMaxSteps(handlerName string, arg sk.Value, maxSteps int) (Response, error) {
	fn, ok := vm.globals[handlerName]
	if !ok {
		return Response{}, fmt.Errorf("starlark: handler %q is not defined", handlerName)
	}

	if _, ok := fn.(sk.Callable); !ok {
		return Response{}, fmt.Errorf("starlark: %q is not callable", handlerName)
	}

	thread := &sk.Thread{
		Name: "stunt",
	}
	thread.SetMaxExecutionSteps(uint64(maxSteps))

	result, err := sk.Call(thread, fn, sk.Tuple{arg}, nil)
	if err != nil {
		return Response{}, fmt.Errorf("starlark: call %q: %w", handlerName, err)
	}

	// A None return (implicit) means the handler completed successfully
	// without a trailing response body — common for server/bidi streaming
	// where all messages were sent via stream.send().
	if _, ok := result.(sk.NoneType); ok {
		return Response{Status: 200}, nil
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
			status, err := sk.AsInt32(val)
			if err != nil {
				return Response{}, fmt.Errorf("starlark: status must be an int, got %s", val.Type())
			}
			resp.Status = status
		case "body":
			if bd, ok := val.(*sk.Dict); ok {
				resp.Body = StarlarkToGo(bd)
			} else if ss, ok := sk.AsString(val); ok && val.Type() == "string" {
				resp.RawBody = ss
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

// StarlarkListToGo converts a Starlark list into a Go []any, suitable for
// JSON marshalling. Each element is recursively converted via
// starlarkValueToGo.
func StarlarkListToGo(l *sk.List) []any {
	out := make([]any, l.Len())
	for i := range out {
		out[i] = starlarkValueToGo(l.Index(i))
	}
	return out
}

// ValueToGo converts an arbitrary Starlark value into a Go value. This is
// the general-purpose converter for resolver return values that may be
// dicts, lists, scalars, or None.
func ValueToGo(v sk.Value) any {
	return starlarkValueToGo(v)
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
