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
- **Streaming RPCs are not yet supported** — only unary methods.

See `adapters/echo-style/` for a complete, working example.
