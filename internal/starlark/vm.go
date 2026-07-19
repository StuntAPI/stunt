package starlark

import (
	"fmt"
	"time"

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
	Status   int
	Headers  map[string]string
	Body     map[string]any
	BodyList []any  // JSON array body (for endpoints returning a bare array)
	RawBody  string // raw text body (when handler returns a string body)
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
	return loadFile("handler.star", src, buildPredeclared(builtins))
}

// LoadWithLib is like Load but first executes libSrc (if non-empty),
// capturing its top-level definitions and making them available to the
// handler script as if they were predeclared builtins. This is the
// shared-library mechanism: a lib.star in the adapter's scripts/ directory
// provides shared helpers without Starlark's load() (which stunt does not
// support).
//
// libSrc runs under the same step-limit and sandbox constraints as the
// handler script. Its globals are frozen before being injected, so handlers
// cannot mutate shared library state.
//
// If libSrc is empty, LoadWithLib behaves exactly like Load.
func LoadWithLib(src, libSrc string, builtins sk.StringDict) (*VM, error) {
	if libSrc == "" {
		return Load(src, builtins)
	}

	predeclared := buildPredeclared(builtins)

	opts := &syntax.FileOptions{While: true}

	// Exec the library first, under the same step limit + sandbox.
	libThread := &sk.Thread{Name: "stunt-lib"}
	libThread.SetMaxExecutionSteps(maxExecutionSteps)
	libGlobals, err := sk.ExecFileOptions(opts, libThread, "lib.star", libSrc, predeclared)
	if err != nil {
		return nil, fmt.Errorf("starlark load lib: %w", err)
	}

	// Freeze lib globals so handlers cannot mutate shared library state.
	for _, v := range libGlobals {
		v.Freeze()
	}

	// Inject lib globals into the handler's predeclared dict.
	for k, v := range libGlobals {
		predeclared[k] = v
	}

	return loadFile("handler.star", src, predeclared)
}

// buildPredeclared assembles the predeclared StringDict that every script
// sees: the default respond builtin plus any caller-provided builtins.
func buildPredeclared(builtins sk.StringDict) sk.StringDict {
	predeclared := sk.StringDict{
		"respond": sk.NewBuiltin("respond", respondBuiltin),
	}
	for k, v := range builtins {
		predeclared[k] = v
	}
	return predeclared
}

// loadFile execs src under a step-limited, while-enabled thread, freezes the
// resulting globals, and returns the VM. This is the common path for both
// Load and LoadWithLib.
func loadFile(name, src string, predeclared sk.StringDict) (*VM, error) {
	opts := &syntax.FileOptions{
		While: true,
	}
	loadThread := &sk.Thread{
		Name: "stunt-load",
	}
	loadThread.SetMaxExecutionSteps(maxExecutionSteps)

	globals, err := sk.ExecFileOptions(opts, loadThread, name, src, predeclared)
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

	reqVal, err := GoToStarlark(map[string]any{
		"method":  req.Method,
		"path":    req.Path,
		"headers": req.Headers,
		"body":    req.Body,
		"params":  req.Params,
		"query":   req.Query,
	})
	if err != nil {
		return Response{}, fmt.Errorf("starlark: build request value: %w", err)
	}

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
				m, err := StarlarkToGo(bd)
				if err != nil {
					return Response{}, fmt.Errorf("starlark: convert body: %w", err)
				}
				resp.Body = m
			} else if ll, ok := val.(*sk.List); ok {
				arr, err := StarlarkListToGo(ll)
				if err != nil {
					return Response{}, fmt.Errorf("starlark: convert body list: %w", err)
				}
				resp.BodyList = arr
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
// Supports string, bool, all integer types (int, int8–int64, uint, uint8–uint64),
// float32/float64, time.Time (→ RFC3339 string), []byte (→ string), nil,
// map[string]any, map[string]string, and slices of any of these.
// For unsupported types it returns an error so adapter authors notice the
// problem instead of getting a silently-stringified value.
func GoToStarlark(v any) (sk.Value, error) {
	switch x := v.(type) {
	case nil:
		return sk.None, nil
	case string:
		return sk.String(x), nil
	case bool:
		return sk.Bool(x), nil
	case int:
		return sk.MakeInt(x), nil
	case int64:
		return sk.MakeInt64(x), nil
	case int32:
		return sk.MakeInt64(int64(x)), nil
	case int16:
		return sk.MakeInt64(int64(x)), nil
	case int8:
		return sk.MakeInt64(int64(x)), nil
	case uint:
		return sk.MakeUint64(uint64(x)), nil
	case uint64:
		return sk.MakeUint64(x), nil
	case uint32:
		return sk.MakeUint64(uint64(x)), nil
	case uint16:
		return sk.MakeUint64(uint64(x)), nil
	case uint8:
		return sk.MakeUint64(uint64(x)), nil
	case float64:
		return sk.Float(x), nil
	case float32:
		return sk.Float(float64(x)), nil
	case time.Time:
		return sk.String(x.Format(time.RFC3339)), nil
	case []byte:
		return sk.String(string(x)), nil
	case map[string]any:
		d := sk.NewDict(len(x))
		for k, val := range x {
			sv, err := GoToStarlark(val)
			if err != nil {
				return nil, fmt.Errorf("map key %q: %w", k, err)
			}
			d.SetKey(sk.String(k), sv)
		}
		return d, nil
	case map[string]string:
		d := sk.NewDict(len(x))
		for k, val := range x {
			d.SetKey(sk.String(k), sk.String(val))
		}
		return d, nil
	case []any:
		elems := make([]sk.Value, len(x))
		for i, val := range x {
			sv, err := GoToStarlark(val)
			if err != nil {
				return nil, fmt.Errorf("slice index %d: %w", i, err)
			}
			elems[i] = sv
		}
		return sk.NewList(elems), nil
	default:
		return nil, fmt.Errorf("starlark: unsupported Go type %T", v)
	}
}

// --- conversion helpers: Starlark → Go ---

// StarlarkToGo converts a Starlark dict into a Go map[string]any.
func StarlarkToGo(d *sk.Dict) (map[string]any, error) {
	out := make(map[string]any, d.Len())
	for _, item := range d.Items() {
		key, _ := sk.AsString(item[0])
		val, err := starlarkValueToGo(item[1])
		if err != nil {
			return nil, fmt.Errorf("dict key %q: %w", key, err)
		}
		out[key] = val
	}
	return out, nil
}

// StarlarkListToGo converts a Starlark list into a Go []any, suitable for
// JSON marshalling. Each element is recursively converted via
// starlarkValueToGo.
func StarlarkListToGo(l *sk.List) ([]any, error) {
	out := make([]any, l.Len())
	for i := range out {
		val, err := starlarkValueToGo(l.Index(i))
		if err != nil {
			return nil, fmt.Errorf("list index %d: %w", i, err)
		}
		out[i] = val
	}
	return out, nil
}

// ValueToGo converts an arbitrary Starlark value into a Go value. This is
// the general-purpose converter for resolver return values that may be
// dicts, lists, scalars, or None.
func ValueToGo(v sk.Value) (any, error) {
	return starlarkValueToGo(v)
}

// starlarkValueToGo converts an arbitrary Starlark value into a Go value.
// For sk.Int values that overflow int64, an error is returned rather than
// silently truncating.
func starlarkValueToGo(v sk.Value) (any, error) {
	switch x := v.(type) {
	case sk.NoneType:
		return nil, nil
	case sk.String:
		return string(x), nil
	case sk.Bool:
		return bool(x), nil
	case sk.Int:
		n, ok := x.Int64()
		if !ok {
			return nil, fmt.Errorf("starlark: integer %s overflows int64", x.String())
		}
		return n, nil
	case sk.Float:
		return float64(x), nil
	case *sk.Dict:
		return StarlarkToGo(x)
	case *sk.List:
		out := make([]any, x.Len())
		for i := range out {
			val, err := starlarkValueToGo(x.Index(i))
			if err != nil {
				return nil, fmt.Errorf("list index %d: %w", i, err)
			}
			out[i] = val
		}
		return out, nil
	default:
		return v.String(), nil
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
