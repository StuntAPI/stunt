package starlark

import (
	"strings"

	sk "go.starlark.net/starlark"
)

// reqValue is the `req` value passed to Starlark handlers. It embeds a
// *starlark.Dict whose entries are the request fields (method, path, host,
// headers, body, raw_body, params, query), so it behaves exactly like a dict
// (req["method"], req.get("query"), len(req), for k in req, "headers" in req,
// req.items(), …) — preserving the API existing adapters rely on — AND it also
// supports attribute access (req.method, req.headers, …) as documented in
// adapters/README.md. The only override is Attr, which resolves a field name to
// its dict entry before falling back to the dict's method attributes.
type reqValue struct {
	*sk.Dict
}

// Attr implements req.method / req.headers (field access) on top of the dict.
// A name that matches a field entry resolves to that entry; otherwise we fall
// through to the dict's own attributes (get, items, keys, values, …).
func (r *reqValue) Attr(name string) (sk.Value, error) {
	if v, found, _ := r.Dict.Get(sk.String(name)); found {
		return v, nil
	}
	return r.Dict.Attr(name)
}

// headerValue is the value of req.headers: a dict (entries keyed by lowercase
// header name) whose lookups are case-insensitive. It embeds *starlark.Dict so
// subscript, iteration, len, items(), keys(), values() and `in` all work; it
// overrides Get (subscript + `in`) and the get() method to lowercase the lookup
// key, so req.headers["Authorization"], .get("authorization"), and
// .get("AUTHORIZATION") all resolve to the same value. Header NAMES are
// case-insensitive per RFC 9110; VALUES are preserved verbatim.
type headerValue struct {
	*sk.Dict
}

// Get implements case-insensitive subscript (headers["X"]) and `in`.
func (h *headerValue) Get(key sk.Value) (sk.Value, bool, error) {
	s, ok := key.(sk.String)
	if !ok {
		return nil, false, nil
	}
	return h.Dict.Get(sk.String(strings.ToLower(string(s))))
}

// Attr returns a case-insensitive get() method; other dict methods (items,
// keys, values, …) delegate to the embedded dict unchanged.
func (h *headerValue) Attr(name string) (sk.Value, error) {
	if name == "get" {
		return sk.NewBuiltin("get", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var key string
			def := sk.Value(sk.None)
			if err := sk.UnpackPositionalArgs("get", args, kwargs, 1, &key, &def); err != nil {
				return nil, err
			}
			v, found, err := h.Get(sk.String(key))
			if err != nil {
				return nil, err
			}
			if !found {
				return def, nil
			}
			return v, nil
		}), nil
	}
	return h.Dict.Attr(name)
}

// newHeaderValue builds a case-insensitive header dict from a Go string map.
// Keys are lowercased (idempotent with engine.headerMap, which already
// lowercases) so iteration/items expose a single canonical form.
func newHeaderValue(h map[string]string) *headerValue {
	d := sk.NewDict(len(h))
	for k, v := range h {
		d.SetKey(sk.String(strings.ToLower(k)), sk.String(v))
	}
	return &headerValue{Dict: d}
}
