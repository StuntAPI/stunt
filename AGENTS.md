# AGENTS.md — Operating stunt (for LLMs and agents)

> This file is a **complete, accurate reference** for LLMs, coding agents, and AI-assisted tools
> that need to operate `stunt` successfully. It mirrors the in-binary reference (`stunt llm`).
> If you are an agent asked to *use* or *build for* stunt, read this first.

## What stunt is

`stunt` creates **local API simulators** (mock servers) so you can develop and test code that calls
real public APIs — **without creating remote accounts, provisioning cloud resources, or hitting the
network**. You describe services in a YAML manifest; stunt serves them on local ports (optionally
over real TLS with subdomain routing). Stateful behavior (databases, tokens, webhooks) comes from
adapters written in a **sandboxed Starlark** scripting layer.

**Install:** `brew install stuntapi/tap/stunt` · **Run from source:** `go run ./cmd/stunt` ·
**One-shot demo:** `stunt demo`

> **Adapters are embedded.** All reference adapters ship INSIDE the binary
> (3.4 MB). `stunt catalog search` lists all 91 offline, and
> `stunt adapter add <name>` resolves a bundled adapter to an `embedded:<name>`
> source that `stunt up` extracts from the binary — no git clone, no network.
> Use `git:` / local-path sources for community or custom adapters.

## The 5-minute mental model

1. **`stunt.yaml`** (the manifest) declares **services**. Each service is either *rules-only*
   (inline declarative responses) or *adapter-backed* (a directory of Starlark handlers + state).
2. **`stunt up`** serves everything in the manifest on local ports (`127.0.0.1:<port>`).
3. Point your client at the served address via an env var (`YOURAPI_BASE_URL=http://127.0.0.1:8000`).
4. State persists in SQLite across requests within one `stunt up` session; `stunt clean` resets it.

## CLI command reference

| Command | What it does |
|---|---|
| `stunt init` | Write a sample `stunt.yaml` (rules-only service). |
| `stunt plan` | Validate the manifest; show what `up` would serve. **Use before `up`.** |
| `stunt up` | Start all services in the foreground (Ctrl-C to stop). **Main command.** |
| `stunt down` | Stop a background/service-managed server. |
| `stunt demo` | Boot the bundled stripe-style sim + print copy-pasteable curl commands. |
| `stunt doctor` | Health check (CA, manifest, adapters, ports). **Run when something fails.** |
| `stunt clean` | Wipe state, CA, and hosts block (keeps manifest + installed adapters). |
| `stunt catalog search [q]` | Search the adapter catalog (`--json` for machine output). |
| `stunt catalog show <name>` | Show one adapter's details. |
| `stunt adapter new <name>` | Scaffold a new adapter (`-style` suffix → adds DISCLAIMER). |
| `stunt adapter add <src>` | Add an adapter source to the manifest. |
| `stunt adapter lint <dir>` | Check an adapter for real (non-synthetic) data. **Must pass.** |
| `stunt adapter test <dir>` | Replay traces against an adapter. |
| `stunt adapter list` | List manifest adapter services. |
| `stunt hosts sync\|clean` | Manage the `/etc/hosts` block (subdomain TLS mode). |
| `stunt proxy start` | Start the TLS reverse proxy (subdomain mode). |
| `stunt trust` | Install stunt's CA into the system trust store (needs privilege). |
| `stunt service install\|status\|uninstall` | Manage the system service unit. |
| `stunt version` \| `stunt --version` | Print the version. |
| `stunt llm` | Print the in-binary reference (this doc, compact). |

**Global flag:** `--manifest <path>` (default `stunt.yaml`). Adapter cache: `--cache-dir` /
`$STUNT_ADAPTER_CACHE` (default `~/.stunt/adapters`). Catalog: `--catalog-url` / `$STUNT_CATALOG_URL`.

## Manifest schema (`stunt.yaml`)

```yaml
version: 1
rng_seed: 42                       # deterministic synthetic data (same seed → same fakes)
network:
  mode: port                       # "port" (one port per service) or "subdomain" (TLS + SNI)
  base_port: 8000                  # port mode: sequential ports (8000, 8001, ...; alphabetical by service name)
  # subdomain mode extras:
  # tld: localhost
  # tls: true                      # real TLS via a local CA + reverse proxy
  # sync_hosts: true               # manage /etc/hosts *.tld entries
  # spoof_real_hosts: false        # redirect real hostnames (api.stripe.com) to localhost

services:
  myapi:                           # service name (becomes the subdomain in subdomain mode)
    adapter: ./adapters/myapi-style   # adapter source: local path OR git:github.com/org/repo@ref
                                    #   OR embedded:stripe-style (a bundled adapter shipped
                                    #     in the binary — no clone needed)
    # rules-only alternative (no adapter):
    # rules:
    #   - name: ok
    #     match: { method: GET, path: /hello }
    #     respond: { status: 200, body: { inline: { message: hi } } }
    config:                         # optional per-service config passed to adapter scripts
      webhook_url: http://127.0.0.1:9999/hooks
```

### Rule fields (inline declarative responses)

```yaml
rules:
  - name: occasional-500
    match: { method: GET, path: /flaky, headers: { X-Debug: "1" } }
    when: { chance: 20 }            # percent probability 0..100
    # when: { expr: "request.body.amount > 1000" }   # boolean expr over request.*
    respond:
      status: 503
      headers: { Content-Type: application/json }
      body: { inline: { error: simulated } }   # OR { file: body.json } OR { template: tmpl.json }
      latency_ms: 100              # simulate latency
      behavior: timeout            # force a hang (drop the connection)
```

Rules in a service are checked in **declaration order**; the first match wins. A catch-all
`match: { path: "/**" }` is the usual 404 backstop. Templates use Go `text/template` with
`{{ faker.Email }}`, `{{ uuid }}`, `{{ now.Format "2006-01-02T15:04:05Z07:00" }}`.

## Adapter manifest schema (`adapters/<name>/adapter.yaml`)

```yaml
id: myapi-style                    # unique adapter id
name: "MyAPI-style API simulator (unofficial)"
version: "0.1.0"                   # the simulator's own version
api:                               # the REAL upstream API + version this reproduces (REQUIRED)
  name: "MyAPI"
  version: "v1"
real_hosts: [api.example.com]

endpoints:                         # route + method → Starlark handler
  - route: /v1/items               # literal routes MUST come before parameterized ones
    method: GET
    handler: scripts/items.star#on_list
  - route: /v1/items/{id}          # {param} captures a path segment
    method: POST
    handler: scripts/items.star#on_create

resources:                         # backing stores (stateful)
  - name: items
    kind: collection               # "collection" (documents) or "kv" (key-value)
    seed: fixtures/items.jsonl     # optional JSONL seed (one JSON object per line)

identity:                          # auth primitive metadata
  token_scheme: bearer             # informational; enforcement is up to your handlers

rules:                             # top-level rules (unmatched routes)
  - name: catchall-404
    match: { path: "/**" }
    respond: { status: 404, body: { inline: { error: not_found } } }

# Optional transports:
# grpc:
#   service: my.api.Service        # fully-qualified gRPC service
#   descriptor: schemas/api.desc   # FileDescriptorSet (length-prefixed protobuf)
#   methods: [{ name: GetUser, handler: scripts/users.star#on_get_user }]
# graphql:
#   schema: schemas/schema.graphql
#   resolvers: scripts/resolvers.star
#   path: /graphql
# ws:                               # WebSocket endpoints
#   - route: /stream
#     handler: scripts/stream.star#on_connect
```

## Starlark handler API (the complete contract)

Handlers are `def on_xxx(req):` functions in `scripts/*.star`. Each is invoked for its route and
must **return a response** via `respond(...)` (or a dict shaped `{status, body, headers}`).

### The `req` object (a dict)

| Key | Type | Description |
|---|---|---|
| `req["method"]` | string | HTTP method (`GET`, `POST`, ...) |
| `req["path"]` | string | request path |
| `req["headers"]` | dict | request headers (case-insensitive keys); e.g. `req["headers"]["Authorization"]` |
| `req["body"]` | dict \| list \| None | parsed JSON body (None if no/invalid JSON) |
| `req["raw_body"]` | string | raw body bytes as a string (for non-JSON/binary uploads) |
| `req["query"]` | dict | query-string parameters |
| `req["params"]` | dict | path parameters (from `{id}` route captures) |

### Builtins

```python
# Return a response (status, body, headers). Required at the end of every handler.
respond(201, {"id": "x"}, {"Content-Type": "application/json"})
respond(200, body={"ok": True})          # status defaults; kwargs are all optional

# --- collections (documents) ---
items = store_collection("items")        # returns a collection handle (declared in resources:)
items.insert({"id": "a1", "v": 1})        # add a document
doc  = items.get("a1")                    # fetch by id → dict or None
all  = items.list()                       # all documents → list of dicts
items.update("a1", {"v": 2})              # replace/merge a document
items.delete("a1")                        # remove by id

# --- key-value store ---
store_kv_set("seq", "next", 42)           # set ns/key = value
v = store_kv_get("seq", "next")           # → value or None
n = store_kv_incr("seq", "counter")       # atomic increment → new int (great for id generation)
store_kv_delete("seq", "next")

# --- blobs (binary / large content) ---
b = store_blob("uploads")                 # blob store handle
b.put("file.bin", raw_bytes, "application/octet-stream")
b.get("file.bin")                         # → bytes
b.stat("file.bin")                        # → metadata
b.list()                                  # → all blob names
b.delete("file.bin")

# --- identity (tokens) ---
tok = identity_mint("user-42", scopes=["read","write"])   # mint a bearer token bound to a subject
ok  = identity_validate(token)            # → subject string if valid, else None
has = identity_has_scope(token, "write")  # → bool

# --- events (webhooks) ---
events_register("http://127.0.0.1:9999/hooks")            # register a webhook sink
events_emit("charge.created", {"id": "ch_1", "amount": 500})  # POST the event to all sinks

# --- standard library ---
json.loads(s) / json.dumps(obj)          # json module is predeclared
```

### `lib.star` (shared helpers)
A `scripts/lib.star` is **preloaded** before any handler script; its top-level `def`s become
available to every handler as if built-in. Use it to share helpers (`_bearer(req)`, `_now()`, ...).
**There is no `load()`** — `lib.star` is the only sharing mechanism.

### Handler return contract
A handler **must** return one of:
- `respond(status, body, headers)` — preferred
- a dict: `{"status": 201, "body": {...}, "headers": {...}}`

### Sandbox limits & gotchas
- **No `load()`**; no filesystem or network access from Starlark; no `import`.
- **Step limit**: handlers are capped (very high) — avoid unbounded loops.
- **Route ordering**: declare literal routes *before* parameterized ones (e.g. `/media_publish`
  before `/{id}/media`); the engine matches in declaration order.
- **String iteration**: iterate via `range(len(s))` + indexing, not per-character.
- **ID generation**: use `store_kv_incr("ns", "seq")` for monotonic ids — do not hand-roll randoms.
- **All data must be synthetic**: `stunt adapter lint` flags real-looking emails, UUIDs, provider
  IDs (`cus_`, `ch_`), credit cards, tokens, PII field literals. Use `{{ faker.Email }}` /
  `{{ uuid }}` in templates, and synthetic literals in fixtures.

## Common patterns

### Bearer auth (mock)
```python
def _require_bearer(req):
    h = req["headers"]
    auth = h.get("Authorization", "") if h else ""
    if not auth.startswith("Bearer "):
        return respond(401, {"error": "unauthorized"})
    return None  # ok

def on_create(req):
    if e := _require_bearer(req): return e
    # ... handle
```

### Stateful create-then-list
```python
def on_create(req):
    items = store_collection("items")
    n = store_kv_incr("meta", "seq")
    doc = {"id": "item_%d" % n}
    doc.update(req["body"] or {})
    items.insert(doc)
    return respond(201, doc)

def on_list(req):
    return respond(200, {"data": store_collection("items").list()})
```

### OAuth2 authorize → token → resource
- `GET /oauth/authorize`: return a `respond(302, None, {"Location": redirect_uri + "?code=X&state=S"})`.
- `POST /oauth/access_token`: validate the single-use code (stored in a collection), mint a token
  with `identity_mint`, return `{access_token, ...}`.
- Protected routes: `identity_validate(token)` to check; bind tokens to subjects via KV.

## Workflows an agent should follow

### "Set up a local mock for API X"
1. Check `stunt catalog search X` for an existing adapter.
2. If found: `stunt adapter add <name>` (or point `adapter:` at it), then `stunt plan`, `stunt up`.
3. If not: `stunt adapter new <name>-style`, edit `adapter.yaml` + `scripts/*.star`, `stunt adapter
   lint <dir>`, then wire it into `stunt.yaml`.
4. Point the client at the served address; assert behavior.

### "Build a new adapter"
1. `stunt adapter new <api>-style` (scaffolds adapter.yaml, a sample handler, README, DISCLAIMER).
2. Fill in `api.name`/`api.version` (the real API + version), `endpoints`, `resources`.
3. Write `scripts/*.star` handlers using the builtins above; put shared helpers in `scripts/lib.star`.
4. `stunt adapter lint <dir>` — must be clean (synthetic data).
5. Add a Go test that boots the adapter and drives the flow (see
   `internal/engine/*_style_test.go` for patterns).
6. `just ci` green.

### "Something isn't working"
1. `stunt doctor` — CA, manifest validity, adapter load errors, port conflicts.
2. `stunt plan` — validate the manifest without starting servers.
3. Check stderr from `stunt up` for per-service load errors (best-effort: one broken service does
   not stop the others).
4. `stunt clean` to reset state, then `stunt up` again.

## Environment & development notes (for agents building stunt itself)
- **Go:** prefix every `go` command with `env -u GOROOT` (or rely on `just`, which sets
  `export GOROOT := ""`). The canonical gate is **`just ci`** = build + `test -race` + vet +
  gofmt + mod-tidy + lint-adapters.
- **Module path:** `stuntapi.com/stunt`. Binary main: `./cmd/stunt` (use `go run ./cmd/stunt`,
  not `go run .`).
- **Adapters ship synthetic data only**, enforced by `stunt adapter lint`. Branded adapters use
  `-style` naming + a `DISCLAIMER`. See `TRADEMARKS.md`.
- **Host-safe tests:** temp dirs, free high ports, `file://` git fixtures, `httptest` sinks. Never
  touch `~/.stunt`, `/etc/hosts`, trust stores, or real network in tests.
