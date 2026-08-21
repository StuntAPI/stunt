// Package runtime wires the primitives (Collection + KV + Blob) stores into
// Starlark handler scripts as builtins, enabling stateful adapters that
// persist data across requests.
//
// # Script API
//
// Collections — store_collection(name) returns a collection object with methods:
//
//	c = store_collection("charges")
//	id = c.insert({"amount": 100, "status": "pending"})  # → str
//	doc = c.get(id)                                       # → dict or None
//	docs = c.list()                                       # → list[dict]
//	c.update(id, {"status": "paid"})                       # replaces doc
//	c.delete(id)                                          # removes doc
//
// KV store — standalone builtins:
//
//	store_kv_set("svc", "key", "value")
//	v = store_kv_get("svc", "key")    # → str or None if missing
//	store_kv_delete("svc", "key")
//	n = store_kv_incr("svc", "counter") # → int (atomic; for monotonic ids)
//	# Note: incr returns int for convenience; get returns the stored string.
//	# KV stores values as strings internally.
//
// Blob store — store_blob(name) returns a blob object with methods:
//
//	b = store_blob("drive")
//	id = b.put("report.txt", "file content")             # → str
//	content = b.get(id)                                   # → str or None
//	info = b.stat(id)                                     # → dict or None
//	b.delete(id)
//	infos = b.list()                                      # → list[dict]
//
// Identity — standalone builtins:
//
//	token = identity_mint("user-1", ["read", "write"])     # → str
//	claims = identity_validate(token)                      # → dict or None
//	has = identity_has_scope(token, "read")                # → bool
//
// Events — standalone builtins:
//
//	events_register("http://localhost:9090/webhook")      # → None
//	events_emit("order.created", {"id": "ord-123"})        # → None (fire-and-forget)
//
// List pagination — standalone builtin:
//
//	page, next = paginate(docs, limit, cursor)   # cursor is an opaque offset
//	# limit None/<=0 disables paging; next is None when no items remain
package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"strconv"
	"strings"
	"time"

	sk "go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/identity"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// BuiltinOptions bundles all the primitives and services that
// BuildAllBuiltins wires into Starlark handler builtins. Any field may be
// nil; the corresponding builtins will still be registered but will return a
// clear error if called without a backing primitive.
type BuiltinOptions struct {
	Store       *primitives.Store
	KV          *kv.KV
	Blob        *blob.Store
	Issuer      *identity.Issuer
	Emitter     *events.Emitter
	Clock       *clock.Clock
	ServiceName string
	// ActiveProfile reports THIS service's active profile name ("" when
	// none). Handlers branch on it to codify adapter-authored behavior
	// modes. Nil registers a builtin that always returns None.
	ActiveProfile func() string
}

// eventsEmitTimeout is the maximum time allowed for a single events_emit
// call (including retries) inside a handler.
const eventsEmitTimeout = 10 * time.Second

// BuildAllBuiltins returns a Starlark StringDict exposing store, identity, and
// events primitives as builtins ready to pass to starlark.Load.
func BuildAllBuiltins(opts BuiltinOptions) sk.StringDict {
	dict := buildStoreBuiltins(opts.Store, opts.KV, opts.Blob)
	for k, v := range buildIdentityBuiltins(opts.Issuer) {
		dict[k] = v
	}
	for k, v := range buildEventsBuiltins(opts.Emitter, opts.ServiceName) {
		dict[k] = v
	}
	for k, v := range buildClockBuiltins(opts.Clock) {
		dict[k] = v
	}
	for k, v := range buildListBuiltins() {
		dict[k] = v
	}
	for k, v := range buildMultipartBuiltins() {
		dict[k] = v
	}
	for k, v := range buildQueryBuiltins() {
		dict[k] = v
	}
	for k, v := range buildSafeDecodeBuiltins() {
		dict[k] = v
	}
	for k, v := range buildProfileBuiltins(opts.ActiveProfile) {
		dict[k] = v
	}
	return dict
}

// buildProfileBuiltins exposes profile_active(): the name of THIS service's
// active profile, or None when none is active. Total — never errors, so a
// handler can branch on it unconditionally.
func buildProfileBuiltins(active func() string) sk.StringDict {
	return sk.StringDict{
		"profile_active": sk.NewBuiltin("profile_active", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			if active == nil {
				return sk.None, nil
			}
			if name := active(); name != "" {
				return sk.String(name), nil
			}
			return sk.None, nil
		}),
	}
}

// BuildBuiltins returns a Starlark StringDict exposing the given stores as
// builtins ready to pass to starlark.Load. Any store may be nil; the
// corresponding builtins will still be registered but will return an error if
// called without a backing store.
//
// This is a convenience wrapper around BuildAllBuiltins that omits the
// identity and events primitives.
func BuildBuiltins(store *primitives.Store, kvStore *kv.KV, blobStore *blob.Store) sk.StringDict {
	return BuildAllBuiltins(BuiltinOptions{
		Store: store,
		KV:    kvStore,
		Blob:  blobStore,
	})
}

// buildListBuiltins registers the list-pagination builtin. It is pure: it
// holds no backing service, so it is available in every handler VM regardless
// of configuration.
//
//	paginate(items, limit?, cursor?) -> (page, next_cursor)
//
// limit is an int; None or <= 0 disables paging (returns the whole list with a
// None next_cursor) so unmodified handlers keep their current behavior. cursor
// is an opaque offset token (string) returned by a prior call, or None/"" for
// the first page. next_cursor is None when no items remain. The adapter owns
// the provider-specific envelope (has_more / nextPageToken / @odata.nextLink)
// and maps its cursor query param to this token.
func buildListBuiltins() sk.StringDict {
	return sk.StringDict{
		"paginate": sk.NewBuiltin("paginate", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var itemsVal sk.Value
			var limitVal sk.Value = sk.None
			var cursorVal sk.Value = sk.None
			if err := sk.UnpackArgs("paginate", args, kwargs, "items", &itemsVal, "limit?", &limitVal, "cursor?", &cursorVal); err != nil {
				return nil, err
			}

			// Materialize the iterable so we can slice it.
			iter := sk.Iterate(itemsVal)
			if iter == nil {
				return nil, fmt.Errorf("paginate: items must be iterable, got %s", itemsVal.Type())
			}
			var all []sk.Value
			var item sk.Value
			for iter.Next(&item) {
				all = append(all, item)
			}
			iter.Done()
			total := len(all)

			// limit: None or <= 0 disables paging. An out-of-int64
			// value (client-sent 25-digit limit) is clamped to the
			// maximum — semantically "no limit", which is what a huge
			// limit asks for — rather than raising.
			limit := -1
			if limitVal != sk.None {
				li, ok := limitVal.(sk.Int)
				if !ok {
					return nil, fmt.Errorf("paginate: limit must be an int, got %s", limitVal.Type())
				}
				n, ok := li.Int64()
				if !ok {
					n = math.MaxInt64
				}
				limit = int(n)
			}

			// cursor: opaque offset token (string) or None/"" for the start.
			// A syntactically invalid token is TOTAL — (None, None) — the
			// same contract as json_safe_decode: cursors are client
			// input, handlers cannot try/except a raise, and the right
			// answer is the adapter's own 400, not a 500. Type errors
			// (programmer mistakes) still raise.
			start := 0
			if cursorVal != sk.None {
				s, ok := cursorVal.(sk.String)
				if !ok {
					return nil, fmt.Errorf("paginate: cursor must be a string or None, got %s", cursorVal.Type())
				}
				if string(s) != "" {
					// Tokens are produced by strconv.Itoa — plain
					// digits. Reject anything else (ParseInt would
					// accept "+5"/"0x1f"-adjacent forms).
					digits := true
					for i := 0; i < len(s); i++ {
						if s[i] < '0' || s[i] > '9' {
							digits = false
							break
						}
					}
					off, err := strconv.ParseInt(string(s), 10, 64)
					if !digits || err != nil || off < 0 {
						return sk.Tuple{sk.None, sk.None}, nil
					}
					start = int(off)
				}
			}
			if start > total {
				start = total
			}

			if limit <= 0 {
				return sk.Tuple{sk.NewList(all), sk.None}, nil
			}
			// Clamp against REMAINING before adding: start+limit can
			// wrap negative when a huge limit was clamped to MaxInt64
			// (a valid cursor + limit=1e23 used to panic the slice).
			end := total
			if limit <= total-start {
				end = start + limit
			}
			var next sk.Value = sk.None
			if end < total {
				next = sk.String(strconv.Itoa(end))
			}
			return sk.Tuple{sk.NewList(all[start:end]), next}, nil
		}),
	}
}

// buildMultipartBuiltins registers the multipart/form-data parsing builtin.
// It is pure, like paginate.
//
//	parse_multipart(content_type, body) -> (parts, err)
//
// content_type is the full request header value (boundary included). Each part
// is {name, filename, content_type, data}; filename/content_type are None when
// the part omits them, and data carries the raw bytes (Starlark strings are
// byte strings, so binary parts round-trip via store_blob). err is None on
// success or a short description, letting the handler answer 400 instead of
// surfacing a 500.
func buildMultipartBuiltins() sk.StringDict {
	return sk.StringDict{
		"parse_multipart": sk.NewBuiltin("parse_multipart", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var contentType, body string
			if err := sk.UnpackArgs("parse_multipart", args, kwargs, "content_type", &contentType, "body", &body); err != nil {
				return nil, err
			}

			_, params, err := mime.ParseMediaType(contentType)
			if err != nil {
				return sk.Tuple{sk.None, sk.String("parse_multipart: bad content-type: " + err.Error())}, nil
			}
			boundary := params["boundary"]
			if boundary == "" {
				return sk.Tuple{sk.None, sk.String("parse_multipart: no boundary parameter")}, nil
			}

			mr := multipart.NewReader(strings.NewReader(body), boundary)
			var parts []sk.Value
			for {
				// NextRawPart, not NextPart: a Content-Transfer-Encoding
				// header must not silently transform the part bytes.
				p, err := mr.NextRawPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					return sk.Tuple{sk.None, sk.String("parse_multipart: " + err.Error())}, nil
				}
				data, err := io.ReadAll(p)
				if err != nil {
					return sk.Tuple{sk.None, sk.String("parse_multipart: " + err.Error())}, nil
				}

				dict := sk.NewDict(4)
				_ = dict.SetKey(sk.String("name"), sk.String(p.FormName()))
				var filename sk.Value = sk.None
				if p.FileName() != "" {
					filename = sk.String(p.FileName())
				}
				_ = dict.SetKey(sk.String("filename"), filename)
				var partCT sk.Value = sk.None
				if ct := p.Header.Get("Content-Type"); ct != "" {
					partCT = sk.String(ct)
				}
				_ = dict.SetKey(sk.String("content_type"), partCT)
				_ = dict.SetKey(sk.String("data"), sk.String(data))
				parts = append(parts, dict)
			}
			return sk.Tuple{sk.NewList(parts), sk.None}, nil
		}),
	}
}

// buildSafeDecodeBuiltins registers json_safe_decode. Starlark has no
// try/except, and json.decode raises an evaluation error on malformed input
// that surfaces as a 500 — handlers validating untrusted JSON (JWT claims,
// multipart metadata parts) need a total function:
//
//	json_safe_decode(s) -> value or None
func buildSafeDecodeBuiltins() sk.StringDict {
	return sk.StringDict{
		"json_safe_decode": sk.NewBuiltin("json_safe_decode", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var s string
			if err := sk.UnpackArgs("json_safe_decode", args, kwargs, "data", &s); err != nil {
				return nil, err
			}
			// UseNumber + int-when-integral so numeric claims decode as
			// Starlark ints exactly like the stdlib json.decode does.
			dec := json.NewDecoder(strings.NewReader(s))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				return sk.None, nil
			}
			v = _numbersToInts(v)
			out, err := starlark.GoToStarlark(v)
			if err != nil {
				return sk.None, nil
			}
			return out, nil
		}),
	}
}

// buildStoreBuiltins registers the collection / KV / blob builtins.
func buildStoreBuiltins(store *primitives.Store, kvStore *kv.KV, blobStore *blob.Store) sk.StringDict {
	return sk.StringDict{
		"store_collection": sk.NewBuiltin("store_collection", func(thread *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var name string
			if err := sk.UnpackArgs("store_collection", args, kwargs, "name", &name); err != nil {
				return nil, err
			}
			if store == nil {
				return nil, fmt.Errorf("store_collection: no collection store configured")
			}
			col, err := store.Collection(name)
			if err != nil {
				return nil, err
			}
			return &collectionValue{col: col}, nil
		}),
		"store_kv_get": sk.NewBuiltin("store_kv_get", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var ns, key string
			if err := sk.UnpackArgs("store_kv_get", args, kwargs, "ns", &ns, "key", &key); err != nil {
				return nil, err
			}
			if kvStore == nil {
				return nil, fmt.Errorf("store_kv_get: no kv store configured")
			}
			val, err := kvStore.Get(ns, key)
			if err == sql.ErrNoRows {
				return sk.None, nil
			}
			if err != nil {
				return nil, err
			}
			return sk.String(val), nil
		}),
		"store_kv_set": sk.NewBuiltin("store_kv_set", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var ns, key string
			var val sk.Value
			if err := sk.UnpackArgs("store_kv_set", args, kwargs, "ns", &ns, "key", &key, "value", &val); err != nil {
				return nil, err
			}
			valStr := starlarkValueToString(val)
			if kvStore == nil {
				return nil, fmt.Errorf("store_kv_set: no kv store configured")
			}
			if err := kvStore.Set(ns, key, valStr); err != nil {
				return nil, err
			}
			return sk.None, nil
		}),
		"store_kv_delete": sk.NewBuiltin("store_kv_delete", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var ns, key string
			if err := sk.UnpackArgs("store_kv_delete", args, kwargs, "ns", &ns, "key", &key); err != nil {
				return nil, err
			}
			if kvStore == nil {
				return nil, fmt.Errorf("store_kv_delete: no kv store configured")
			}
			if err := kvStore.Delete(ns, key); err != nil {
				return nil, err
			}
			return sk.None, nil
		}),
		"store_kv_incr": sk.NewBuiltin("store_kv_incr", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var ns, key string
			if err := sk.UnpackArgs("store_kv_incr", args, kwargs, "ns", &ns, "key", &key); err != nil {
				return nil, err
			}
			if kvStore == nil {
				return nil, fmt.Errorf("store_kv_incr: no kv store configured")
			}
			next, err := kvStore.Incr(ns, key)
			if err != nil {
				return nil, err
			}
			return sk.MakeInt64(int64(next)), nil
		}),
		"store_blob": sk.NewBuiltin("store_blob", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var name string
			if err := sk.UnpackArgs("store_blob", args, kwargs, "name", &name); err != nil {
				return nil, err
			}
			if blobStore == nil {
				return nil, fmt.Errorf("store_blob: no blob store configured")
			}
			return &blobValue{store: blobStore, ns: name}, nil
		}),
	}
}

// --- identity builtins ---

// buildIdentityBuiltins registers identity_mint, identity_validate, and
// identity_has_scope. If issuer is nil, each builtin returns a clear error.
func buildIdentityBuiltins(issuer *identity.Issuer) sk.StringDict {
	return sk.StringDict{
		"identity_mint": sk.NewBuiltin("identity_mint", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var subject string
			var scopesVal sk.Value = sk.NewList(nil)
			if err := sk.UnpackArgs("identity_mint", args, kwargs, "subject", &subject, "scopes?", &scopesVal); err != nil {
				return nil, err
			}
			if issuer == nil {
				return nil, fmt.Errorf("identity_mint: no identity issuer configured")
			}
			scopes, err := starlarkListToStrings(scopesVal)
			if err != nil {
				return nil, fmt.Errorf("identity_mint: %w", err)
			}
			token, err := issuer.Mint(subject, scopes, defaultTokenTTL)
			if err != nil {
				return nil, fmt.Errorf("identity_mint: %w", err)
			}
			return sk.String(token), nil
		}),
		"identity_validate": sk.NewBuiltin("identity_validate", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var token string
			if err := sk.UnpackArgs("identity_validate", args, kwargs, "token", &token); err != nil {
				return nil, err
			}
			if issuer == nil {
				return nil, fmt.Errorf("identity_validate: no identity issuer configured")
			}
			claims, err := issuer.Validate(token)
			if err != nil {
				return sk.None, nil // invalid or expired → None
			}
			return claimsToDict(claims), nil
		}),
		"identity_has_scope": sk.NewBuiltin("identity_has_scope", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var token, scope string
			if err := sk.UnpackArgs("identity_has_scope", args, kwargs, "token", &token, "scope", &scope); err != nil {
				return nil, err
			}
			if issuer == nil {
				return nil, fmt.Errorf("identity_has_scope: no identity issuer configured")
			}
			claims, err := issuer.Validate(token)
			if err != nil {
				return sk.False, nil // invalid or expired → False
			}
			return sk.Bool(identity.HasScope(claims, scope)), nil
		}),
	}
}

// defaultTokenTTL is the lifetime of tokens minted via identity_mint.
const defaultTokenTTL = time.Hour

// claimsToDict converts identity.Claims into a Starlark dict with keys
// subject, scopes, and expires_at (RFC3339).
func claimsToDict(c *identity.Claims) sk.Value {
	elems := make([]sk.Value, len(c.Scopes))
	for i, s := range c.Scopes {
		elems[i] = sk.String(s)
	}
	d := sk.NewDict(3)
	d.SetKey(sk.String("subject"), sk.String(c.Subject))
	d.SetKey(sk.String("scopes"), sk.NewList(elems))
	d.SetKey(sk.String("expires_at"), sk.String(c.ExpiresAt.Format(time.RFC3339)))
	return d
}

// --- events builtins ---

// buildEventsBuiltins registers events_register and events_emit. If emitter
// is nil, each builtin returns a clear error.
func buildEventsBuiltins(emitter *events.Emitter, serviceName string) sk.StringDict {
	return sk.StringDict{
		"events_register": sk.NewBuiltin("events_register", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var url string
			if err := sk.UnpackArgs("events_register", args, kwargs, "url", &url); err != nil {
				return nil, err
			}
			if emitter == nil {
				return nil, fmt.Errorf("events_register: no events emitter configured")
			}
			emitter.Register(serviceName, url)
			return sk.None, nil
		}),
		"events_target": sk.NewBuiltin("events_target", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			if emitter == nil {
				return sk.None, nil
			}
			if url := emitter.Target(serviceName); url != "" {
				return sk.String(url), nil
			}
			return sk.None, nil
		}),
		"events_emit": sk.NewBuiltin("events_emit", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var eventType string
			var payloadVal sk.Value = sk.None
			var headersVal sk.Value = sk.None
			if err := sk.UnpackArgs("events_emit", args, kwargs, "event_type", &eventType, "payload?", &payloadVal, "headers?", &headersVal); err != nil {
				return nil, err
			}
			if emitter == nil {
				return nil, fmt.Errorf("events_emit: no events emitter configured")
			}
			payload := map[string]any{}
			if d, ok := payloadVal.(*sk.Dict); ok {
				p, err := starlark.StarlarkToGo(d)
				if err != nil {
					return nil, fmt.Errorf("events_emit: %w", err)
				}
				payload = p
			}
			var headers map[string]string
			if headersVal != sk.None {
				hd, ok := headersVal.(*sk.Dict)
				if !ok {
					return nil, fmt.Errorf("events_emit: headers must be a dict, got %s", headersVal.Type())
				}
				headers = starlark.ToStringMap(hd)
			}
			ctx, cancel := context.WithTimeout(context.Background(), eventsEmitTimeout)
			defer cancel()
			// Fire-and-forget: webhook delivery failures (including "not
			// registered" and HTTP errors) must never break the handler.
			_ = emitter.Emit(ctx, serviceName, eventType, payload, headers)
			return sk.None, nil
		}),
		"events_body": sk.NewBuiltin("events_body", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var eventType string
			var payloadVal sk.Value = sk.None
			if err := sk.UnpackArgs("events_body", args, kwargs, "event_type", &eventType, "payload?", &payloadVal); err != nil {
				return nil, err
			}
			payload := map[string]any{}
			if d, ok := payloadVal.(*sk.Dict); ok {
				p, err := starlark.StarlarkToGo(d)
				if err != nil {
					return nil, fmt.Errorf("events_body: %w", err)
				}
				payload = p
			}
			// Same marshal path as Emit, so a signature over this verifies
			// against the bytes the sink receives. Needs no emitter.
			body, err := events.MarshalEnvelope(eventType, payload)
			if err != nil {
				return nil, fmt.Errorf("events_body: %w", err)
			}
			return sk.String(string(body)), nil
		}),
		"events_emit_raw": sk.NewBuiltin("events_emit_raw", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var eventType string
			var body string
			var headersVal sk.Value = sk.None
			if err := sk.UnpackArgs("events_emit_raw", args, kwargs, "event_type", &eventType, "body", &body, "headers?", &headersVal); err != nil {
				return nil, err
			}
			if emitter == nil {
				return nil, fmt.Errorf("events_emit_raw: no events emitter configured")
			}
			var headers map[string]string
			if headersVal != sk.None {
				hd, ok := headersVal.(*sk.Dict)
				if !ok {
					return nil, fmt.Errorf("events_emit_raw: headers must be a dict, got %s", headersVal.Type())
				}
				headers = starlark.ToStringMap(hd)
			}
			ctx, cancel := context.WithTimeout(context.Background(), eventsEmitTimeout)
			defer cancel()
			// Fire-and-forget, like events_emit. The body string is delivered
			// verbatim (no {type, payload} envelope) for providers whose
			// receivers parse the provider's own event-object shape.
			_ = emitter.EmitRaw(ctx, serviceName, eventType, []byte(body), headers)
			return sk.None, nil
		}),
	}
}

// buildClockBuiltins exposes the engine's injectable clock so handlers can mint
// timestamps for signature schemes with a replay window (Stripe, Slack). nil
// clock → no clock builtins (rather than a runtime error on every call).
func buildClockBuiltins(c *clock.Clock) sk.StringDict {
	if c == nil {
		return sk.StringDict{}
	}
	return sk.StringDict{
		"clock": &starlarkstruct.Module{
			Name: "clock",
			Members: sk.StringDict{
				"now_unix": sk.NewBuiltin("clock.now_unix", func(_ *sk.Thread, _ *sk.Builtin, _ sk.Tuple, _ []sk.Tuple) (sk.Value, error) {
					return sk.MakeInt(int(c.Now().Unix())), nil
				}),
				"now_rfc3339": sk.NewBuiltin("clock.now_rfc3339", func(_ *sk.Thread, _ *sk.Builtin, _ sk.Tuple, _ []sk.Tuple) (sk.Value, error) {
					return sk.String(c.Now().UTC().Format(time.RFC3339)), nil
				}),
				"unix_to_rfc3339": sk.NewBuiltin("clock.unix_to_rfc3339", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
					var v sk.Value
					if err := sk.UnpackArgs("clock.unix_to_rfc3339", args, kwargs, "unix_seconds", &v); err != nil {
						return nil, err
					}
					// A timestamp stored in a collection round-trips through JSON
					// as a float, so accept both int and float here.
					secs, ok := unixSeconds(v)
					if !ok {
						return nil, fmt.Errorf("clock.unix_to_rfc3339: unix_seconds must be int or float, got %s", v.Type())
					}
					return sk.String(time.Unix(secs, 0).UTC().Format(time.RFC3339)), nil
				}),
			},
		},
	}
}

// --- conversion helpers ---

// unixSeconds converts a Starlark int or float to a Unix seconds int64.
func unixSeconds(v sk.Value) (int64, bool) {
	switch x := v.(type) {
	case sk.Int:
		secs, ok := x.Int64()
		return secs, ok
	case sk.Float:
		return int64(x), true
	}
	return 0, false
}

// starlarkListToStrings converts a Starlark list (or None) of strings into
// a Go []string.
func starlarkListToStrings(v sk.Value) ([]string, error) {
	if v == sk.None {
		return nil, nil
	}
	lst, ok := v.(*sk.List)
	if !ok {
		return nil, fmt.Errorf("expected list, got %s", v.Type())
	}
	out := make([]string, lst.Len())
	for i := range out {
		s, ok := sk.AsString(lst.Index(i))
		if !ok {
			return nil, fmt.Errorf("element %d is %s, not a string", i, lst.Index(i).Type())
		}
		out[i] = s
	}
	return out, nil
}

// --- collection object (starlark.Value with methods) ---

// collectionValue wraps a *primitives.Collection as a Starlark value with
// methods: insert, get, list, update, delete.
type collectionValue struct {
	col *primitives.Collection
}

func (c *collectionValue) String() string        { return "collection" }
func (c *collectionValue) Type() string          { return "collection" }
func (c *collectionValue) Freeze()               {}
func (c *collectionValue) Hash() (uint32, error) { return 0, nil }
func (c *collectionValue) Truth() sk.Bool        { return sk.True }

func (c *collectionValue) CompareSameType(_ syntax.Token, _ sk.Value, _ int) (bool, error) {
	return false, fmt.Errorf("collection does not support comparison")
}

// AttrNames returns the method names exposed to Starlark's dir().
func (c *collectionValue) AttrNames() []string {
	return []string{"insert", "get", "list", "update", "delete"}
}

// Attr returns the named method as a Starlark callable, or nil if not found.
func (c *collectionValue) Attr(name string) (sk.Value, error) {
	switch name {
	case "insert":
		return sk.NewBuiltin("collection.insert", c.insert), nil
	case "get":
		return sk.NewBuiltin("collection.get", c.get), nil
	case "list":
		return sk.NewBuiltin("collection.list", c.list), nil
	case "update":
		return sk.NewBuiltin("collection.update", c.update), nil
	case "delete":
		return sk.NewBuiltin("collection.delete", c.delete), nil
	default:
		return nil, nil // no such attribute
	}
}

func (c *collectionValue) insert(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var docVal sk.Value = sk.None
	if err := sk.UnpackArgs("insert", args, kwargs, "doc", &docVal); err != nil {
		return nil, err
	}
	doc, err := dictToGoMap(docVal)
	if err != nil {
		return nil, err
	}
	id, err := c.col.Insert(doc)
	if err != nil {
		return nil, err
	}
	return sk.String(id), nil
}

func (c *collectionValue) get(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var id string
	if err := sk.UnpackArgs("get", args, kwargs, "id", &id); err != nil {
		return nil, err
	}
	doc, err := c.col.Get(id)
	if err == sql.ErrNoRows {
		return sk.None, nil
	}
	if err != nil {
		return nil, err
	}
	return starlark.GoToStarlark(doc)
}

func (c *collectionValue) list(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	if err := sk.UnpackArgs("list", args, kwargs); err != nil {
		return nil, err
	}
	docs, err := c.col.List()
	if err != nil {
		return nil, err
	}
	elems := make([]sk.Value, len(docs))
	for i, doc := range docs {
		sv, err := starlark.GoToStarlark(doc)
		if err != nil {
			return nil, err
		}
		elems[i] = sv
	}
	return sk.NewList(elems), nil
}

func (c *collectionValue) update(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var id string
	var docVal sk.Value = sk.None
	if err := sk.UnpackArgs("update", args, kwargs, "id", &id, "doc", &docVal); err != nil {
		return nil, err
	}
	doc, err := dictToGoMap(docVal)
	if err != nil {
		return nil, err
	}
	if err := c.col.Update(id, doc); err != nil {
		return nil, err
	}
	return sk.None, nil
}

func (c *collectionValue) delete(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var id string
	if err := sk.UnpackArgs("delete", args, kwargs, "id", &id); err != nil {
		return nil, err
	}
	if err := c.col.Delete(id); err != nil {
		return nil, err
	}
	return sk.None, nil
}

// --- blob object (starlark.Value with methods) ---

// blobValue wraps a *blob.Store bound to a namespace as a Starlark value
// with methods: put, get, stat, delete, list.
type blobValue struct {
	store *blob.Store
	ns    string
}

func (b *blobValue) String() string        { return "blob:" + b.ns }
func (b *blobValue) Type() string          { return "blob" }
func (b *blobValue) Freeze()               {}
func (b *blobValue) Hash() (uint32, error) { return 0, nil }
func (b *blobValue) Truth() sk.Bool        { return sk.True }

func (b *blobValue) CompareSameType(_ syntax.Token, _ sk.Value, _ int) (bool, error) {
	return false, fmt.Errorf("blob does not support comparison")
}

// AttrNames returns the method names exposed to Starlark's dir().
func (b *blobValue) AttrNames() []string {
	return []string{"put", "append", "get", "stat", "delete", "list"}
}

// Attr returns the named method as a Starlark callable, or nil if not found.
func (b *blobValue) Attr(name string) (sk.Value, error) {
	switch name {
	case "put":
		return sk.NewBuiltin("blob.put", b.put), nil
	case "append":
		return sk.NewBuiltin("blob.append", b.append), nil
	case "get":
		return sk.NewBuiltin("blob.get", b.get), nil
	case "stat":
		return sk.NewBuiltin("blob.stat", b.stat), nil
	case "delete":
		return sk.NewBuiltin("blob.delete", b.delete), nil
	case "list":
		return sk.NewBuiltin("blob.list", b.list), nil
	default:
		return nil, nil // no such attribute
	}
}

// put(name, content, content_type="") writes content as a blob and returns
// the generated id (which equals name).
func (b *blobValue) put(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var name, content string
	var contentType string // optional, defaults to ""
	if err := sk.UnpackArgs("put", args, kwargs, "name", &name, "content", &content, "content_type?", &contentType); err != nil {
		return nil, err
	}
	id, err := b.store.PutWith(b.ns, name, contentType, strings.NewReader(content))
	if err != nil {
		return nil, err
	}
	return sk.String(id), nil
}

// append(id, content, content_type="") appends content to a blob, creating it
// if absent, and returns the new total size. The O(chunk) path for resumable
// uploads — avoids re-reading and re-writing the accumulated content per chunk.
func (b *blobValue) append(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var id, content string
	var contentType string
	if err := sk.UnpackArgs("append", args, kwargs, "id", &id, "content", &content, "content_type?", &contentType); err != nil {
		return nil, err
	}
	total, err := b.store.Append(b.ns, id, contentType, strings.NewReader(content))
	if err != nil {
		return nil, err
	}
	return sk.MakeInt64(total), nil
}

// get(id) reads and returns the blob content as a string, or None if missing.
func (b *blobValue) get(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var id string
	if err := sk.UnpackArgs("get", args, kwargs, "id", &id); err != nil {
		return nil, err
	}
	rc, err := b.store.Get(b.ns, id)
	if err == blob.ErrNotFound {
		return sk.None, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return sk.String(string(data)), nil
}

// stat(id) returns a dict with name, size, content_type, modified, or None.
func (b *blobValue) stat(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var id string
	if err := sk.UnpackArgs("stat", args, kwargs, "id", &id); err != nil {
		return nil, err
	}
	info, err := b.store.Stat(b.ns, id)
	if err == blob.ErrNotFound {
		return sk.None, nil
	}
	if err != nil {
		return nil, err
	}
	return blobInfoToDict(info), nil
}

// delete(id) removes a blob. Idempotent — returns None whether or not the
// blob existed.
func (b *blobValue) delete(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var id string
	if err := sk.UnpackArgs("delete", args, kwargs, "id", &id); err != nil {
		return nil, err
	}
	if err := b.store.Delete(b.ns, id); err != nil {
		return nil, err
	}
	return sk.None, nil
}

// list() returns all blobs in the namespace as a list of dicts.
func (b *blobValue) list(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	if err := sk.UnpackArgs("list", args, kwargs); err != nil {
		return nil, err
	}
	infos, err := b.store.List(b.ns)
	if err != nil {
		return nil, err
	}
	elems := make([]sk.Value, len(infos))
	for i, info := range infos {
		elems[i] = blobInfoToDict(info)
	}
	return sk.NewList(elems), nil
}

// blobInfoToDict converts a blob.Info into a Starlark dict.
func blobInfoToDict(info blob.Info) sk.Value {
	d := sk.NewDict(4)
	d.SetKey(sk.String("name"), sk.String(info.Name))
	d.SetKey(sk.String("size"), sk.MakeInt64(info.Size))
	d.SetKey(sk.String("content_type"), sk.String(info.ContentType))
	d.SetKey(sk.String("modified"), sk.String(info.Modified.Format("2006-01-02T15:04:05Z07:00")))
	return d
}

// dictToGoMap converts a Starlark dict (or None) into a Go map[string]any.
func dictToGoMap(v sk.Value) (map[string]any, error) {
	if v == sk.None {
		return map[string]any{}, nil
	}
	d, ok := v.(*sk.Dict)
	if !ok {
		return nil, fmt.Errorf("expected dict, got %s", v.Type())
	}
	return starlark.StarlarkToGo(d)
}

// starlarkValueToString converts any Starlark value into its string
// representation. Strings are returned as-is; booleans become "True"/
// "False" (matching Starlark's str()); integers and floats are stringified
// numerically; None becomes the empty string. This makes store_kv_set
// forgiving — any value type is accepted and stored as a string.
func starlarkValueToString(v sk.Value) string {
	if v == nil || v == sk.None {
		return ""
	}
	if s, ok := sk.AsString(v); ok {
		return s
	}
	switch x := v.(type) {
	case sk.Bool:
		if x {
			return "True"
		}
		return "False"
	default:
		return v.String()
	}
}

// _numbersToInts converts json.Number leaves to int64 when integral, else
// float64, recursing through maps and slices.
func _numbersToInts(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case map[string]any:
		for k, val := range x {
			x[k] = _numbersToInts(val)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = _numbersToInts(val)
		}
		return x
	}
	return v
}
