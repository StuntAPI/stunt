# First-party adapters

`stunt` ships a set of **reference adapters** so the tool is useful out of the box and so the
adapter model has proven examples. Each lives in its own directory under `adapters/`.

## Neutral naming & disclaimers (mandatory for branded adapters)

Adapters that mimic a specific company's API **MUST** follow these conventions, which keep the
project on the safe side of trademark/ToS concerns and make the "local testing simulator"
framing unambiguous:

1. **Neutral, "-style" naming.** The adapter id, directory name, and human description use the
   `<Provider>-style` form — e.g. `stripe-style`, `drive-style`, `twitter-style`, `dropbox-style`.
   Never claim to *be* the provider.
2. **`DISCLAIMER` file.** Every branded adapter directory ships a `DISCLAIMER` (see
   `DISCLAIMER.template`) stating: not affiliated with or endorsed by the provider; for **local
   development and testing only**; uses **synthetic data only** (no real provider data, no recorded
   responses); does not call the real provider.
3. **Synthetic data only.** All fixtures/templates are synthetic (faker-generated). `stunt adapter
   lint` must pass clean. This is the core safety property.
4. **Structure, not documentation.** Adapters reproduce the API *structure* (routes, shapes) needed
   for local testing; they do not reproduce the provider's proprietary documentation verbatim.
5. **No provider logos/trademarks** in the adapter.

The unbranded, generic adapter scaffold (`stunt adapter new`) is unaffected by these rules.

## Adapters in this repo

| Directory | Simulates | Backing |
|---|---|---|
| `adapters/stripe-style` | a Stripe-style payments API (charges, customers, balance, events) | Collection + Starlark |
| `adapters/drive-style` | a Google-Drive-style files API (files, folders, about/quota) | Blob + Collection |
| `adapters/twitter-style` | an X.com / Twitter-style API (auth, tweets, users, timeline) | Identity + Collection (pure-mock) |
| `adapters/echo-style` | a generic gRPC service (Say, Add, ListEchoes) — gRPC reference example | Collection + KV + Starlark |
| `adapters/dropbox-style` | a Dropbox-style files API (upload/download, list_folder, metadata) | Blob + Collection |
| `adapters/blog-style` | a generic GraphQL blog API (users, posts, comments, nested relations) — GraphQL reference example | Collection + Starlark |

Each adapter is **broader than a minimal demo** but remains an MVP: enough endpoints to be a useful
local stand-in and to exercise the stunt primitives end to end. See each adapter's own README.

## Using an adapter

Point a `stunt.yaml` service at the adapter directory (local path for now; git refs via the catalog
later):

```yaml
services:
  stripe:
    adapter: ./adapters/stripe-style
```

Then `stunt up` serves it (port mode by default; `mode: subdomain` for `https://stripe.localhost`).

## Starlark Builtins Reference

Starlark handlers (REST, gRPC, WebSocket) have access to the following builtins. Each is documented
with its signature, argument types, and return type.

### `respond(status?, body?, headers?)` → dict

Returns a response dict suitable as a handler return value. All arguments are optional.

| Argument | Type | Default | Description |
|----------|------|---------|-------------|
| `status` | int | `200` | HTTP status code |
| `body` | dict or str | `None` | Response body — a dict is rendered as JSON; a str is sent as raw text |
| `headers` | dict | `None` | Response headers (string → string) |

```python
def on_get(req):
    return respond(200, {"message": "hello"}, {"Content-Type": "application/json"})
```

### `store_collection(name)` → collection object

Returns a collection object backed by SQLite. Collections must be declared in `adapter.yaml`
under `resources:` with `kind: collection`.

The returned object has these methods:

| Method | Signature | Returns | Notes |
|--------|-----------|---------|-------|
| `insert(doc)` | `insert(doc: dict)` | `str` (generated id) | Inserts a new document; auto-generates an id field |
| `get(id)` | `get(id: str)` | `dict` or `None` | Returns the document, or `None` if not found |
| `list()` | `list()` | `list[dict]` | Returns all documents (no filtering — filter in Starlark) |
| `update(id, doc)` | `update(id: str, doc: dict)` | `None` | **Replaces the entire document** (PUT semantics, not PATCH/merge). To merge, `get`-then-update the full doc |
| `delete(id)` | `delete(id: str)` | `None` | Deletes the document |

```python
def on_post(req):
    items = store_collection("items")
    id = items.insert({"name": "Widget", "price": 999})
    return respond(201, {"id": id})
```

### `store_blob(name)` → blob object

Returns a blob store object backed by the filesystem. Blob namespaces do not need to be declared
in `resources:`.

| Method | Signature | Returns | Notes |
|--------|-----------|---------|-------|
| `put(name, content, content_type?)` | `put(name: str, content: str, content_type: str="")` | `str` (name) | Writes content as a blob; `content_type` is optional |
| `get(id)` | `get(id: str)` | `str` or `None` | Returns blob content as a string, or `None` if not found |
| `stat(id)` | `stat(id: str)` | `dict` or `None` | Returns `{name, size, content_type, modified}`, or `None` |
| `delete(id)` | `delete(id: str)` | `None` | Deletes the blob (idempotent) |
| `list()` | `list()` | `list[dict]` | Returns all blobs as `[{name, size, content_type, modified}, ...]` |

```python
def on_upload(req):
    blobs = store_blob("files")
    id = blobs.put("report.txt", "file content", "text/plain")
    return respond(201, {"id": id})
```

### KV store builtins

The KV store is a simple string→string key-value store backed by SQLite. **All values are stored
and returned as strings** — `set` accepts any type and stringifies it; `get` always returns a string;
`incr` returns an `int` (the new counter value) for convenience — use `str(store_kv_incr(...))` if you need to store it back via `set`.

> **Why does `incr` return `int` while `get` returns `str`?** The KV store stores everything as strings.
`incr` atomically reads, parses, increments, and writes the value, returning the computed `int` so you
can use it directly for IDs and counters without an extra `int()` conversion. `get` returns the raw
stored string. If you `get` a counter after `incr`, you'll get the string representation (e.g. `"3"`).

| Builtin | Signature | Returns | Notes |
|---------|-----------|---------|-------|
| `store_kv_set(ns, key, value)` | `(ns: str, key: str, value: any)` | `None` | Stores value as its string representation (int, bool, str all accepted) |
| `store_kv_get(ns, key)` | `(ns: str, key: str)` | `str` or `None` | Returns the stored string, or `None` if key doesn't exist |
| `store_kv_delete(ns, key)` | `(ns: str, key: str)` | `None` | Deletes the key |
| `store_kv_incr(ns, key)` | `(ns: str, key: str)` | `int` | Atomically increments and returns the new value (starts at 1). Useful for monotonic ID generation |

```python
def on_create(req):
    next_id = store_kv_incr("svc", "counter")  # returns int: 1, 2, 3, ...
    store_kv_set("svc", "status", "active")     # strings
    store_kv_set("svc", "count", 42)             # ints auto-stringified to "42"
    val = store_kv_get("svc", "count")           # returns string "42"
    return respond(201, {"id": next_id, "val": val})
```

### Identity builtins

| Builtin | Signature | Returns | Notes |
|---------|-----------|---------|-------|
| `identity_mint(subject, scopes?)` | `(subject: str, scopes: list[str] = [])` | `str` (token) | Mints a signed token (HMAC) valid for 1 hour |
| `identity_validate(token)` | `(token: str)` | `dict` or `None` | Returns `{subject, scopes, expires_at}`, or `None` if invalid/expired |
| `identity_has_scope(token, scope)` | `(token: str, scope: str)` | `bool` | Returns `True` if the token is valid and has the given scope |

```python
def on_login(req):
    token = identity_mint("user-1", ["read", "write"])
    return respond(200, {"token": token})

def on_protected(req):
    token = req["headers"].get("Authorization", "").replace("Bearer ", "")
    claims = identity_validate(token)
    if claims == None:
        return respond(401, {"error": "invalid token"})
    return respond(200, {"user": claims["subject"]})
```

### Events builtins

Events are fire-and-forget: delivery failures (including no registered webhook) never break the
handler. Webhooks are delivered as HTTP POST with a JSON body `{type, payload}`.

| Builtin | Signature | Returns | Notes |
|---------|-----------|---------|-------|
| `events_register(url)` | `(url: str)` | `None` | Registers a webhook URL for the current service |
| `events_target()` | `()` | `str` or `None` | The currently-registered webhook URL for this service (or `None`). Use it when a provider's signature MACs the destination URL (Twilio, Square) |
| `events_emit(event_type, payload?, headers?)` | `(event_type: str, payload: dict = {}, headers: dict = {})` | `None` | Emits an event to registered webhook(s). `headers` are set on the POST on top of `Content-Type: application/json` (a caller `Content-Type` overrides it); `Host`/`Content-Length` and any CR/LF are rejected |
| `events_body(event_type, payload?)` | `(event_type: str, payload: dict = {})` | `str` | The exact JSON body `events_emit(event_type, payload)` will POST. Use it to compute a signature over the real wire bytes (see Signing webhooks below) |

### Pagination builtin (`paginate`)

`paginate` slices a list into pages so list handlers can honor `limit`/cursor query params. It is a
pure builtin — no backing store — so it works in every handler. Returns a 2-tuple `(page, next_cursor)`.

```python
page, next_cursor = paginate(docs, limit, cursor)
```

| Argument | Type | Default | Notes |
|----------|------|---------|-------|
| `items` | iterable | required | The full result list (typically `store_collection(...).list()`), filtered first |
| `limit` | int | `None` | Page size. `None` or `<= 0` **disables paging** (returns the whole list, `next_cursor = None`) — so unmodified handlers keep their prior behavior |
| `cursor` | str | `None` | Opaque offset token returned by a prior call; `None`/`""` for the first page |

`next_cursor` is the opaque token for the next page, or `None` when no items remain.

The builtin does the universal slicing; **the adapter owns the provider envelope and cursor mapping**.
Stripe, for example, has no cursor field — clients set `starting_after` to the last returned id — so a
thin wrapper translates the id to an offset and reports `has_more`:

```python
def _list_page(req, docs):
    limit = _to_int(req.get("query", {}).get("limit", "")) or 10
    if limit > 100:
        limit = 100
    offset = ""
    sa = req.get("query", {}).get("starting_after", "")
    for i in range(len(docs)):
        if docs[i].get("id") == sa:
            offset = str(i + 1)
            break
    page, nxt = paginate(docs, limit, offset)
    return page, nxt != None  # has_more
```

For opaque-cursor providers (HubSpot `after`, OData `@odata.nextLink`), pass `next_cursor` straight
through as the next page token.

### Signing webhooks (`crypto`, `clock`)

A receiver that verifies a webhook signature (Stripe `Stripe-Signature`, GitHub `X-Hub-Signature-256`, Shopify, Slack, Twilio, Adyen) can now be exercised against stunt. Three primitives — all string-in/string-out (Starlark has no bytes):

| Module | Functions | Notes |
|--------|-----------|-------|
| `crypto` | `hmac_sha256(key, data, encoding="hex")`, `hmac_sha1(...)`, `sha256(data, encoding="hex")`, `base64_encode(data)`, `base64_decode(s)`, `base64url_encode(data)`, `ecdsa_sign_p256(private_key_pem, data, encoding="hex")`, `ecdsa_verify_p256(public_key_pem, data, signature, encoding="hex")`, `rsa_sign(private_key_pem, data, encoding="hex")`, `rsa_verify(public_key_pem, data, signature, encoding="hex")`, `rsa_public_jwk(public_key_pem)→{kty,n,e}` | `encoding` is `"hex"` (default), `"base64"`, or `"base64url"`. MAC, hash, and asymmetric signature (ECDSA P-256 raw r‖s; RSA-SHA256 PKCS#1 v1.5). Keys arrive as PEM strings the adapter supplies (ship a fixed keypair for determinism). `rsa_public_jwk` returns the RSA public key's JWK params (base64url) for serving JWKS. No encryption/KDF/key-gen |
| `clock` | `now_unix()`, `now_rfc3339()` | Wall clock from the engine's injectable `clock.Clock` — real today; the virtual mode is the seam for future record/replay |

**Rule:** MAC the `events_body(...)` bytes verbatim — never a re-marshalled copy — so the signer and verifier agree on the exact bytes.

```python
# Stripe
body = events_body("charge.succeeded", payload)
t = clock.now_unix()
sig = crypto.hmac_sha256(secret, str(t) + "." + body)
events_emit("charge.succeeded", payload, {"Stripe-Signature": "t=" + str(t) + ",v1=" + sig})

# GitHub
sig = crypto.hmac_sha256(secret, events_body("push", payload))
events_emit("push", payload, {"X-Hub-Signature-256": "sha256=" + sig, "X-GitHub-Event": "push"})
```

**Signed-delivery roster** — adapters that compute (not just document) a provider signature on every delivery. Mock secrets are public/low-entropy (local stunt only); configure your receiver with the exact string:

| Adapter | Signed? | Header | Mock secret | Enc |
|---------|:-------:|--------|-------------|:---:|
| stripe-style | yes | `Stripe-Signature` (`t=,v1=`) | `whsec_stunt_mock_0123456789abcdef0123456789abcdef` | hex |
| github-style | yes | `X-Hub-Signature-256` | `stunt_mock_github_webhook_secret_2026` | hex |
| shopify-style | yes | `X-Shopify-Hmac-SHA256` | `shpss_stunt_mock_api_client_secret` | base64 |
| whatsapp-style | yes | `X-Hub-Signature-256` (Meta) | `whatsapp_stunt_mock_app_secret_2026` | hex |
| square-style | yes | `X-Square-HmacSha256-Signature` (URL+body) | `sq0sip_stunt_mock_signature_key_2026` | base64 |
| twilio-style | yes | `X-Twilio-Signature` (SHA-1, URL+body) | `twilio_auth_token` | base64 |
| adyen-style | deferred | — | — | — |
| braintree-style | deferred | — | — | — |

Deferred providers need schemes the current primitives don't cover yet: Adyen signs an in-body `hmacSignature` over a derived field-concatenation; Braintree needs raw-byte HMAC keys + form-encoded delivery. (Twilio was deferred for HMAC-SHA-1 over the sink URL + body — now shipped, via `crypto.hmac_sha1` + the new `events_target()` builtin for the delivery URL.) Square likewise MACs the notification URL + body.

To receive events, set `config.webhook_url` in your `stunt.yaml`:

```yaml
services:
  myapi:
    adapter: ./myapi
    config:
      webhook_url: http://localhost:9090/webhook
```

Then `events_emit("order.created", {"id": "123"})` will POST to that URL.

### `request` object (handler argument)

Every handler receives a `req` argument with:

| Field | Type | Description |
|-------|------|-------------|
| `req.method` | `str` | HTTP method (e.g. `"GET"`, `"POST"`) |
| `req.path` | `str` | Request path (e.g. `/v1/charges/ch_123`) |
| `req.headers` | `dict[str, str]` | Request headers (case-insensitive lookups; `req.headers.get("authorization")` finds `Authorization`). `req` also supports dict access (`req["method"]`, `req.get("query")`) |
| `req.body` | `dict` | Parsed JSON body (empty dict if no body) |
| `req.raw_body` | `str` | The verbatim request body bytes (as a string). Use it for non-JSON / binary content (e.g. an S3 object upload) where the parsed `body` map is meaningless — store it via `store_blob` so it round-trips byte-exact |
| `req.params` | `dict[str, str]` | Path parameters extracted from route (e.g. `{id}` → `{"id": "..."}`) |
| `req.query` | `dict[str, str]` | Query parameters (first value of each key) |

## Serializing concurrent handler calls (`concurrency_key`)

When a handler does a read-modify-write across stores (e.g. a resumable-upload chunk that reads a partial blob, appends, and writes it back), concurrent calls that share a path param can race — both read stale state, both write, last write wins. An endpoint opts into per-key serialization:

```yaml
endpoints:
  - route: /v1.0/_upload/{session}
    method: PUT
    handler: scripts/drive_upload.star#on_upload_chunk
    concurrency_key: session   # calls with the same {session} run one at a time
```

The route must contain `{concurrency_key}` (validated at adapter load). Calls with different keys still run in parallel; the lock is released when the handler returns, across every early-return path. This is the recommended fix whenever a handler's correctness depends on more than one store operation being atomic.

## State persistence
Adapter state (collections, KV stores, blobs) persists on disk in `.stunt/state/` next to your
`stunt.yaml`. Data survives across `stunt up` restarts — seeds are loaded once, and mutations
(inserts, updates, deletes) persist between sessions. Run `stunt clean` to reset all state back
to the seed fixtures.

## gRPC adapters

An adapter can serve a **gRPC service** in addition to (or instead of) REST endpoints. The service is
served dynamically from a compiled protobuf `FileDescriptorSet` — no generated Go stubs needed. Each
RPC method is routed to a Starlark handler, just like REST endpoints.

### Writing a gRPC adapter

1. **Write a `.proto` file** describing your service:

   ```proto
   syntax = "proto3";
   package stunt.example;

   service Echo {
     rpc Say(EchoRequest) returns (EchoReply);
   }

   message EchoRequest  { string message = 1; }
   message EchoReply    { string message = 1; int32 count = 2; }
   ```

2. **Compile it to a descriptor set** with `protoc`. Check the `.desc` file into the adapter so it is
   self-contained (no `protoc` needed at serve time):

   ```bash
   protoc --proto_path=adapters/my-adapter/schemas \
     --descriptor_set_out=adapters/my-adapter/schemas/my.desc \
     adapters/my-adapter/schemas/my.proto
   ```

3. **Declare a `grpc:` section** in `adapter.yaml`, mapping each method to a Starlark handler:

   ```yaml
   grpc:
     service: stunt.example.Echo          # fully-qualified protobuf service name
     descriptor: schemas/my.desc          # path to the compiled .desc (relative to adapter dir)
     methods:
       - name: Say                        # bare method name (must match the proto)
         handler: scripts/echo.star#on_say # Starlark handler: scripts/<file>.star#<function>
   ```

4. **Write the Starlark handlers.** Each handler receives `req` with `body` (the decoded request map,
   keyed by protobuf field names) and returns `respond(status, body)` where `body` is the response
   map. HTTP status codes are mapped to gRPC codes (200 → OK, 404 → NotFound, 400 → InvalidArgument,
   401 → Unauthenticated, 500 → Internal). All the usual Starlark builtins (`store_collection`,
   `store_kv_get/set/incr`, `store_blob`, `identity_*`, `events_emit`) are available:

   ```python
   def on_say(req):
       message = req["body"]["message"]
       count = store_kv_incr("echo", "say_count")
       return respond(200, {"message": message, "count": count})
   ```

   > **Note:** protobuf `int32`/`int64` fields arrive as JSON numbers (floats in Starlark). Use
   > `int(value)` to convert before arithmetic. Protojson marshals response fields in camelCase by
   > default (e.g. `echo_count` → `echoCount` on the wire).

### Notes

- gRPC handlers use the same Starlark sandbox as REST handlers — no host I/O, no network.
- A service can declare both `endpoints:` (REST) and `grpc:` simultaneously.

### gRPC streaming

Streaming RPCs (server-streaming, client-streaming, and bidi-streaming) are fully supported. A
streaming method is routed to a Starlark handler `on_<method>(stream)` where `stream` is a special
value exposing two methods:

- **`stream.recv()`** — returns the next inbound message as a dict, or `None` when the client has
  half-closed (no more messages).
- **`stream.send(msg)`** — emits an outbound message. `msg` must be a dict.

The three streaming modes work as follows:

| Mode | Signature | Handler pattern |
|---|---|---|
| **Server-streaming** | `(EchoRequest) returns (stream EchoReply)` | `req = stream.recv()` once, then `stream.send(...)` for each reply |
| **Client-streaming** | `(stream EchoRequest) returns (EchoReply)` | loop `stream.recv()` until `None`, then `return respond(status, msg)` |
| **Bidi-streaming** | `(stream EchoRequest) returns (stream EchoReply)` | loop `stream.recv()` until `None`, calling `stream.send(...)` per message |

Example server-streaming handler:

```python
def on_stream_echo(stream):
    req = stream.recv()
    message = req["message"] if req != None else ""
    for i in range(3):
        stream.send({"message": message, "echo_count": i + 1})
```

For client-streaming, the handler's return value is the single response:

```python
def on_accumulate(stream):
    total = 0
    while True:
        m = stream.recv()
        if m == None:
            break
        total += int(m["value"])
    return respond(200, {"total": total})
```

- `return respond(status, msg)` sets the **trailing** gRPC status. A 2xx status sends `msg` as a final
  message (useful for client-streaming); a 4xx/5xx status maps to the corresponding gRPC error code.
- `while` loops are allowed — the Starlark handler runs to completion within a single gRPC stream
  invocation.
- Integer fields in streamed messages arrive as `float64` in Starlark (protojson round-trips numbers
  via JSON), so use `int(x)` before arithmetic — e.g. `total += int(m["value"])`.
- Unary methods on the same service continue to use the `on_<method>(req)` API unchanged.

See `adapters/echo-style/` for a complete, working example.

## WebSocket adapters

An adapter can serve **WebSocket** routes alongside REST and gRPC. Each WS route is backed by a
connection-lifetime Starlark handler `on_connect(ws)` that is invoked once per WebSocket connection.
The handler receives a `ws` object with three methods: `recv()`, `send()`, and `close()`.

### Writing a WebSocket adapter

1. **Declare a `ws:` section** in `adapter.yaml`, mapping each route to a Starlark handler:

   ```yaml
   ws:
     - route: "/ws/echo"                         # path pattern (supports {param} segments)
       handler: "scripts/ws.star#on_connect"       # Starlark handler: scripts/<file>.star#<function>
       subprotocols: ["echo.v1"]                    # optional subprotocol negotiation
   ```

2. **Write the `on_connect(ws)` handler.** The handler loops on `ws.recv()` to process inbound
   messages, sends responses via `ws.send()`, and returns when the client disconnects:

   ```python
   def on_connect(ws):
       while True:
           m = ws.recv()
           if m == None:
               break          # client disconnected — exit cleanly
           ws.send(m)         # echo the message back
   ```

### The `ws` object API

| Method | Description |
|---|---|
| **`ws.recv()`** | Returns the next inbound message. A JSON object arrives as a **dict**; other text/binary arrives as a **str**. Returns `None` when the client has disconnected (clean EOF). This is a **blocking** call — no Starlark steps accrue while waiting. On engine shutdown the connection is closed with a `StatusGoingAway` frame; `recv()` then returns `None` as with any client-side close. |
| **`ws.send(msg)`** | Sends a text frame. If `msg` is a **dict**, it is marshalled to a JSON text frame. If `msg` is a **list**, it is marshalled to a JSON array. If `msg` is an **int**, **float**, or **bool**, its JSON representation is sent. If `msg` is a **str**, it is sent as a raw text frame. |
| **`ws.close(code=1000, reason="")`** | Performs a graceful WebSocket close. `code` defaults to 1000 (normal closure). Invalid status codes produce a Starlark error. Valid ranges are 1000–1014 and 3000–4999 (per RFC 6455 §7.4). |

### Step budget & idle connections

The Starlark step budget for WS handlers is elevated (10M steps, 10× the default) so legitimately
chatty connections are not prematurely killed. The key property is that `recv()` is a blocking
builtin: while it waits for the next message, **zero steps accrue**. This means:

- **Idle / slow long-lived connections** live indefinitely (`recv` blocks, 0 steps).
- A handler that tight-loops calling `ws.send(...)` without ever `recv()`-ing is still killed by the
  step limit (correct DoS guard).

### Worked example: stateful echo

The `echo-style` adapter includes a WebSocket route that echoes each message and increments a global
KV counter, demonstrating the builtin store integration:

```yaml
# adapter.yaml
ws:
  - route: "/ws/echo"
    handler: "scripts/ws.star#on_connect"
    subprotocols: ["echo.v1"]
```

```python
# scripts/ws.star
# Echoes messages back and increments a global kv counter on each message.

def on_connect(ws):
    while True:
        m = ws.recv()
        if m == None:
            break
        store_kv_incr("echo", "ws_echo_count")
        ws.send(m)
```

### Notes

- WS handlers use the same Starlark sandbox as REST and gRPC handlers — no host I/O, no network.
- All builtins (`store_collection`, `store_kv_get/set/incr`, `store_blob`, `identity_*`,
  `events_emit`) are available inside `on_connect`.
- `stunt adapter lint` validates the `ws:` section and scans handler scripts for real-looking data,
  just like fixtures and templates.
- A service can declare `endpoints:` (REST), `grpc:` (gRPC), `ws:` (WebSocket), and `graphql:` (GraphQL)
  simultaneously.

## GraphQL adapters

An adapter can serve a **GraphQL endpoint** in addition to (or instead of) REST, gRPC, or WebSocket.
The endpoint is served from an SDL schema file plus convention-named Starlark resolver functions.
No generated code is needed — the executor parses the SDL, validates query documents, and dispatches
field resolution to Starlark.

### Writing a GraphQL adapter

1. **Write an SDL schema** (`schemas/schema.graphql`) describing your types, queries, and mutations:

   ```graphql
   type User {
     id: ID!
     name: String!
     posts: [Post!]!
   }

   type Post {
     id: ID!
     title: String!
     author: User!
   }

   type Query {
     user(id: ID!): User
     users: [User!]!
   }

   type Mutation {
     createUser(name: String!): User!
   }
   ```

2. **Declare a `graphql:` section** in `adapter.yaml`, pointing to the schema and a resolver script:

   ```yaml
   graphql:
     schema: schemas/schema.graphql      # SDL file (relative to adapter dir)
     resolvers: scripts/resolvers.star    # Starlark script with on_*/resolve_* functions
     # path: /graphql                     # optional; defaults to /graphql
   ```

3. **Write Starlark resolvers.** The resolver model has two conventions:

   - **Root fields** (Query / Mutation) — routed to `on_<field>(callArg)` functions.
   - **Object fields** — routed to `resolve_<Type>_<field>(callArg)` functions. If a resolver is
     absent, the **default resolver** returns `parent[fieldName]` (the property of the parent object
     with the same name as the field).

   `callArg` is a single Starlark dict with two keys:

   | Key | Description |
   |-----|-------------|
   | `parent` | The parent object (dict). For root fields this is `None`; for object fields it is the resolved value of the parent type. |
   | `args` | The field arguments (dict), with variables and defaults already resolved by the executor. |

   Resolvers return `respond(status, body)` where `body` is the resolved value (a dict, list, string,
   or `None`). All the usual Starlark builtins (`store_collection`, `store_kv_get/set/incr`,
   `store_blob`, `identity_*`, `events_emit`) are available.

   ```python
   # Root resolver: Query.user(id)
   def on_user(args):
       uid = args["args"]["id"]
       for u in store_collection("users").list():
           if u.get("id") == uid:
               return respond(200, u)
       return respond(200, None)

   # Object resolver: User.posts (relational)
   def resolve_User_posts(args):
       uid = args["parent"]["id"]
       return respond(200, [p for p in store_collection("posts").list() if p.get("user_id") == uid])
   ```

   > **Scalar fields need no resolver.** Fields like `id`, `name`, `title` are resolved automatically
   > from the parent object via the default resolver. Only relational fields (those that require a
   > lookup or computation) need a `resolve_*` function.

### Args & variables

GraphQL arguments (literals, variables, and default values) are resolved by the executor and
passed to the resolver as `args["args"]`. Optional arguments that are omitted appear as `None` —
use `args["args"].get("field")` to safely access them.

```graphql
query($status: PostStatus) {
  posts(status: $status) { id title }
}
```

```python
def on_posts(args):
    status = args["args"].get("status")  # None if not provided
    posts = store_collection("posts").list()
    if status != None:
        posts = [p for p in posts if p.get("status") == status]
    return respond(200, posts)
```

### Enums & custom scalars

- **Enums** (e.g. `enum PostStatus { PUBLISHED; DRAFT }`) are passed and returned as plain strings.
- **Custom scalars** (e.g. `scalar DateTime`) pass through as-is — the executor does not validate or
  coerce them.

### Introspection

Full introspection is supported: `__schema`, `__type(name:)`, and `__typename` work out of the box.
This means GraphQL clients and tools (GraphiQL, code generators) can query the schema at runtime.

### DoS limits

The executor enforces the following limits to prevent abuse:

| Limit | Default | Description |
|-------|---------|-------------|
| Max query depth | 10 | Maximum nesting depth of a query. |
| Max field count | 1000 | Maximum number of fields in a query. |
| Execution timeout | context deadline | Request context timeout (cancels long-running resolvers). |

Queries that exceed these limits are rejected with a 400 error.

### Worked example

```yaml
# adapter.yaml
graphql:
  schema: schemas/schema.graphql
  resolvers: scripts/resolvers.star
```

```graphql
# schemas/schema.graphql
enum PostStatus { PUBLISHED; DRAFT }
type User { id: ID! name: String! posts: [Post!]! }
type Post { id: ID! title: String! status: PostStatus! author: User! }
type Query { users: [User!]! posts(status: PostStatus): [Post!]! }
```

```python
# scripts/resolvers.star

def on_users(args):
    return respond(200, store_collection("users").list())

def on_posts(args):
    status = args["args"].get("status")
    posts = store_collection("posts").list()
    if status != None:
        posts = [p for p in posts if p.get("status") == status]
    return respond(200, posts)

# Relational: User.posts
def resolve_User_posts(args):
    uid = args["parent"]["id"]
    return respond(200, [p for p in store_collection("posts").list() if p.get("user_id") == uid])

# Relational: Post.author
def resolve_Post_author(args):
    author_id = args["parent"]["user_id"]
    for u in store_collection("users").list():
        if u.get("id") == author_id:
            return respond(200, u)
    return respond(200, None)
```

### Notes

- GraphQL resolvers use the same Starlark sandbox as REST, gRPC, and WS handlers — no host I/O,
  no network.
- `stunt adapter lint` validates the `graphql:` section (schema + resolvers must be present with a
  valid handler spec) and scans resolver scripts for real-looking data, just like fixtures and
  templates.
- See `adapters/blog-style/` for a complete, working example with seeded collections, nested
  relations, enums, mutations, and a custom scalar.
