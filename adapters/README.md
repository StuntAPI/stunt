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
| **`ws.recv()`** | Returns the next inbound message. A JSON object arrives as a **dict**; other text/binary arrives as a **str**. Returns `None` when the client has disconnected (clean EOF). This is a **blocking** call — no Starlark steps accrue while waiting. |
| **`ws.send(msg)`** | Sends a frame. If `msg` is a **dict**, it is marshalled to a JSON text frame. If `msg` is a **str**, it is sent as a raw text frame. |
| **`ws.close(code=1000, reason="")`** | Performs a graceful WebSocket close. `code` defaults to 1000 (normal closure). |

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
- A service can declare `endpoints:` (REST), `grpc:` (gRPC), and `ws:` (WebSocket) simultaneously.
