# Echo-style adapter (gRPC example)

A stunt adapter demonstrating a **generic gRPC service** — a small stateful
"Echo" API. This is a reference example for writing gRPC-backed adapters; it
does **not** mimic any specific company's API (no DISCLAIMER needed). All data
is synthetic.

## What it serves

A trivial gRPC service (`stunt.example.Echo`) with three unary RPCs:

| RPC | Request | Reply | Behavior |
|-----|---------|-------|----------|
| `Say` | `{ message: string }` | `{ message: string, echo_count: int32 }` | Echoes the message back and records it; `echo_count` is the total number of `Say` calls. |
| `Add` | `{ value: int32 }` | `{ total: int32, count: int32 }` | Accumulates `value` into a running total; `total` is the sum across all `Add` calls, `count` is the number of calls. |
| `ListEchoes` | `{}` | `{ echoes: [{ message: string }] }` | Returns all messages previously echoed by `Say`. |

State persists in an in-process SQLite-backed store, so data you create in one
call is visible in subsequent calls within the same `stunt up` session.

## Schema

The protobuf definition is in [`schemas/echo.proto`](schemas/echo.proto). The
compiled descriptor set ([`schemas/echo.desc`](schemas/echo.desc)) is checked
in so the adapter is self-contained — no `protoc` needed at serve time.

To regenerate the descriptor after editing the `.proto`:

```bash
protoc --proto_path=adapters/echo-style/schemas \
  --descriptor_set_out=adapters/echo-style/schemas/echo.desc \
  adapters/echo-style/schemas/echo.proto
```

## Usage

Point a `stunt.yaml` service at the adapter directory:

```yaml
services:
  echo:
    adapter: ./adapters/echo-style
```

Then `stunt up` serves it. The gRPC server listens on a free loopback port
(printed at startup). Call it with any gRPC client, e.g.:

```bash
grpcurl -plaintext -d '{"message": "hello"}' \
  127.0.0.1:PORT stunt.example.Echo/Say
```

## How it works

The `grpc:` section in [`adapter.yaml`](adapter.yaml) maps each RPC to a
Starlark handler in [`scripts/echo.star`](scripts/echo.star). The handler
receives the decoded request as `req["body"]` (a JSON-like map keyed by
protobuf field names) and returns `respond(status, body)` where `body` is the
response map. Status codes are mapped to gRPC codes (200 → OK, 404 → NotFound,
etc.). See [`adapters/README.md`](../README.md) for the full gRPC authoring
guide.
