# stunt

**Local API simulators — test against real APIs, locally, without remote accounts.**

`stunt` reads a `stunt.yaml` manifest and serves local, runnable stand-ins for real APIs.
Stateful behavior comes from sandboxed Starlark adapters backed by SQLite/blob primitives;
declarative behavior comes from a rules engine (templated responses, probabilistic faults,
conditional expressions). REST, gRPC (unary + streaming), and WebSocket transports are supported.
Optionally front everything with a portless.dev-style TLS proxy on `*.localhost`. Everything is
deterministic via `rng_seed`.

> **Status:** pre-1.0 MVP. The core is built and self-tested; see **Known limitations** below.
> Unofficial, not affiliated with any provider whose API style an adapter mimics.

---

## Install

```bash
go install github.com/stunt-adapters/stunt/cmd/stunt@latest
```

> This machine note (dev only): if `go` commands fail with a stdlib version mismatch, prefix
> them with `env -u GOROOT` (a local `GOROOT`/toolchain misconfiguration).

## Quickstart

```bash
stunt init            # writes a sample stunt.yaml
stunt plan            # validate + show what will run
stunt up              # serve all services (Ctrl-C to stop)
```

Inline, declarative service (no adapter needed):

```yaml
version: 1
rng_seed: 42
network:
  mode: port
  base_port: 8000
services:
  example:
    rules:
      - match: { method: GET, path: /hello }
        respond: { status: 200, body: { template: '{"message":"hi","id":"{{ faker.ID "k" }}"}' } }
      - match: { method: GET, path: /hello }
        when: { chance: 20 }                       # 20% of replies error
        respond: { status: 503, body: { inline: { error: boom } } }
```

Stateful adapter service (e.g. the bundled Stripe-style adapter):

```yaml
version: 1
rng_seed: 42
network:
  mode: port
  base_port: 8000
services:
  stripe:
    adapter: ./adapters/stripe-style
```

Then `curl http://127.0.0.1:8000/v1/charges -d '{"amount":1000,"currency":"usd"}'`.

## Rules engine

First-match-wins, top-to-bottom.

- **match**: `method`, `path` (globs `*` one segment, `**` zero+), `headers`.
- **when** (both must pass to fire):
  - `when.chance`: percent probability; otherwise fall through.
  - `when.expr`: boolean expression over `request.*` (`method`, `path`, `headers`, `body`), e.g. `request.body.amount > 1000`.
- **respond**: `status`, `headers`, `latency_ms`, `behavior: timeout`, and `body`:
  - `body.inline` — literal (rendered as JSON).
  - `body.file` — static file (relative to the manifest).
  - `body.template` — `text/template` with `{{ .Request.* }}`, `{{ faker.ID/Email/Name }}`, `{{ uuid }}`, `{{ now }}`.

## Adapters

An adapter is a directory (later: a git repo) describing how to simulate one API:
`adapter.yaml` + `endpoints/` + `templates/` + `fixtures/` + `scripts/*.star` + `schemas/`.
Endpoints are either rules-based or backed by **sandboxed Starlark** handlers with access to
stateful primitives. Build your own with the contributor workflow:

```bash
stunt adapter new myapi               # scaffold a synthetic adapter
stunt adapter import openapi spec.yaml # generate endpoints/templates from an OpenAPI doc
stunt adapter import har session.har   # infer endpoints + synthetic fixtures from a HAR
stunt adapter import proto api.proto    # scaffold a gRPC adapter from a .proto (descriptor + handlers)
stunt adapter lint ./adapters/myapi     # enforce SYNTHETIC data only (the safety guard)
stunt adapter test ./adapters/myapi     # conformance vs your local real traces
stunt catalog search stripe            # browse the adapter registry
```

Starlark handlers are sandboxed (no host I/O, no network) and can use the primitive builtins:
`store_collection(name)` (insert/get/list/update/delete), `store_kv_get/set/delete/incr`,
`store_blob(name)` (put/get/stat/delete/list).

### Reference adapters (in this repo)

All unofficial, synthetic-data-only, with a DISCLAIMER. See `adapters/README.md`.

| Adapter | Simulates | Backing |
|---|---|---|
| `adapters/stripe-style` | payments API — charges (create/retrieve/list/capture/refund), customers (CRUD), balance | Collection + Starlark |
| `adapters/drive-style` | files API — upload/get/download/list/patch/delete, folders, about/quota | Blob + Collection |
| `adapters/twitter-style` | X.com/Twitter-style — mock OAuth, tweets (CRUD), users, timeline | Collection (pure-mock reads) |
| `adapters/echo-style` | generic gRPC service (Say, Add, ListEchoes) — gRPC reference example | Collection + KV + Starlark |
| `adapters/dropbox-style` | files API (RPC-style) — upload/download/list_folder/get_metadata/create_folder/delete | Blob + Collection |

## Networking (`*.localhost`, TLS)

`network.mode: subdomain` runs the engine on a high port behind a portless.dev-style TLS proxy:

```yaml
network:
  mode: subdomain
  tld: localhost
  tls: true
```

Then services are reachable at `https://<service>.localhost` (HTTP/2, local CA auto-trusted via
`stunt setup`). The privileged listener forwards to an **unprivileged** engine (so adapter code
never runs as root). CLI: `stunt proxy start|stop`, `stunt service install|status|uninstall`,
`stunt trust`, `stunt hosts sync|clean`, `stunt doctor`, `stunt clean`.

## Primitives

`Collection` (SQLite), `KV`, `Blob` (FS), `Clock+scheduler` (deterministic), `Identity` (HMAC tokens),
`Events` (webhooks), `Generator`, `Validator` (JSON-Schema).

## Known limitations (MVP)

- Adapters load from **local paths or git refs** (`stunt adapter add git:host/user/repo@ref`, cached under `~/.stunt/adapters`, pinned tags/sha/head).
  The remote catalog index URL is a placeholder (`stunt catalog` works offline via a bundled fallback).
- Adapters can `identity_mint`/`identity_validate` tokens and `events_emit` webhooks from Starlark
  (the stripe-style adapter demonstrates real bearer-token validation + `charge.*` webhook emission).
- The privileged `:443` bind requires `stunt setup`/`stunt service install` (one-time); without it,
  subdomain mode uses an OS-assigned high port (the URL includes the port).
- **gRPC** unary and streaming RPCs are supported via the `grpc:` adapter section (served dynamically
  from a compiled protobuf descriptor set). Streaming handlers use `stream.recv()`/`stream.send()`.
  **WebSocket** is supported via the `ws:` adapter section with connection-lifetime Starlark handlers
  (`on_connect(ws)` using `ws.recv()`/`ws.send()`). GraphQL is not yet supported.
- Concurrency is tested with `-race`; the design is single-process per `stunt up`.

## Project layout

`internal/{rules,manifest,engine,adapter,adapter/runtime,starlark}`,
`internal/primitives{,/blob,/clock,/events,/gen,/identity,/kv,/validator}`,
`internal/netutil{,/proxy}`, `internal/contrib{,/openapi,/har,/lint,/conform}`, `internal/catalog`,
`internal/cli`, `cmd/stunt`, `adapters/`.

## Design & roadmap

- Spec: `docs/specs/2025-07-15-stunt-design.md`
- Plans: `docs/plans/`
- Issue tracking: `bd` (beads) — `bd ready`, `bd list`, `bd epic status`
