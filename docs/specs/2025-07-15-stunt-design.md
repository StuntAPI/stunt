# Design Spec: `stunt` — local API simulators

- **Status:** Approved design (pending implementation plan)
- **Date:** 2025-07-15
- **Working name:** `stunt` ("stunt double for your APIs")
- **Related:** `../../api-simulator-research/FEASIBILITY.md` (feasibility study)

---

## 1. Problem

Developers integrate many remote APIs (Stripe, Drive, Dropbox, X, …). Testing against the real services requires creating accounts, handling credentials, costs money/rate-limits, is non-deterministic, needs network, and is slow. Existing mock tools (WireMock, Prism, MSW, Microcks) cover static/schema mocking well but don't give you a **stateful, realistic, locally-runnable stand-in** for arbitrary services without hand-authoring everything.

## 2. Goals

1. A single, static, cross-platform **Go binary** that spins up local servers mimicking real APIs.
2. **Three entry points** reading the same declarative **YAML manifest** (`stunt.yaml`): an interactive **wizard**, the **CLI**, and the file itself (Hashicorp-flavored lifecycle).
3. **Named `*.localhost` domains** (no port juggling) with **TLS** out of the box.
4. A **rules engine** supporting: always-success, **%-based error injection** by type (domain errors vs. infra errors like timeout/5xx), latency, conditional matching — all **serializable in the manifest**.
5. **Templates/fixtures referenced by relative path** with a convention, so the manifest stays clean.
6. **Contributor-friendly adapters** distributed as independent git repos (pinned by tag/hash), composable from **core primitives** so adapters stay thin.
7. Adapter logic in **sandboxed Starlark** so community adapters can't execute arbitrary code on the host.
8. A **catalog** of popular APIs and a **reverse-engineering workflow** for contributors.

## 3. Non-goals (v1)

- gRPC / GraphQL / WebSocket protocols (REST first).
- Full real-OAuth-flow simulation (a local "yes" issuer only).
- npm adapter distribution (git first).
- A gallery website (catalog = static registry first).
- Shipping recorded real responses — **never** (synthetic only; see §10).

---

## 4. Settled decisions (decision log)

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | Fidelity model | **Hybrid stateful** — core is a stateless rules engine; adapters opt endpoints into statefulness | Keeps core small; pushes hard work to adapters where community contributes |
| D2 | Core vs. adapters | **Core provides reusable primitives**; adapters compose them | Avoids N bespoke reimplementations of storage etc. |
| D3 | Adapter authoring | **Declarative (YAML) + embedded scripting** for the tricky 5% | 95% of adapters are config; escape hatch for the rest |
| D4 | Distribution | **pi-style**: git repos (npm later), pinned by tag/hash/head, convention dirs, catalog keyword, global vs project scope | Proven, contributor-friendly model |
| D5 | Language/stack | **Go + YAML + Starlark** | Static binary; universal YAML; sandboxed Starlark (safe community adapters) |
| C1 | Name | `stunt` | "stunt double for your APIs" |
| C2 | `.localhost` networking | **Follow portless.dev (vercel-labs)**: privileged listener on `:80/:443` (auto-elevates with sudo; installable as an OS startup service — launchd/systemd/Task Scheduler — so it binds at boot, no per-run sudo) that forwards by Host/SNI to the **unprivileged** engine on a high port. HTTP/2 + HTTPS default; local CA auto-trusted; `/etc/hosts` auto-sync; high-port fallback. | True port-less URLs without sudo-every-run, **and** adapter code never runs as root (portless topology already separates privileged proxy from unprivileged apps) |
| C3 | State store | **SQLite** (collections/KV) + **local FS** (blobs) under `./.stunt/state` | Free query/persistence; trivial blobs |
| C4 | Catalog | Static registry/list keyed by `api-sim-adapter` keyword (pi gallery model); website later | Lowest-friction start |

---

## 5. Architecture overview

```
                         ┌──────────────────────────────────────┐
   stunt.yaml ──────────►│              ENGINE (Go)             │
   (manifest)            │  router · rules · primitives ·       │──► https://stripe.localhost
   init wizard ─────────►│  Starlark VM · clock · events        │──► https://drive.localhost
   CLI flags ───────────►│                                      │──► https://twitter.localhost
                         └───────────────┬──────────────────────┘
                                         │ loads
                         ┌───────────────▼──────────────────────┐
                         │   ADAPTERS (cached git repos)        │
                         │   declarative YAML + fixtures +      │
                         │   templates + Starlark scripts       │
                         └──────────────────────────────────────┘
```

`stunt` = an **engine** (HTTP router + primitives + rules + Starlark VM + clock + events) that reads a **manifest**, loads **adapters** from a local cache, and exposes services on **`*.localhost`** with TLS.

---

## 6. Component specs

### 6.1 Manifest — `stunt.yaml`

Self-contained; rules live inline; bulky artifacts referenced by relative path.

```yaml
version: 1

network:
  mode: subdomain            # subdomain (default) | port
  tld: localhost             # -> https://<service>.localhost (repeatable; e.g. also 'test')
  tls: true                  # local CA auto-trusted; HTTP/2 + HTTPS (use false for plain HTTP :80)
  wildcard: false            # true: unregistered sub.<service>.<tld> falls back to the parent service
  sync_hosts: true           # auto-sync /etc/hosts so Safari/custom TLDs resolve
  spoof_real_hosts: false    # true: also map real hostnames (api.stripe.com) -> 127.0.0.1

state:
  dir: ./.stunt/state        # sqlite + blob files
  reset: on-up               # on-up | on-down | manual | never

services:
  stripe:
    adapter: git:github.com/stunt-adapters/stripe@v3.1.0
    rules: []                # project-level overlay; see 6.2
    config: {}               # passed into adapter
  drive:
    adapter: git:github.com/stunt-adapters/google-drive@v1.2.0
    config: { quota_bytes: 16106127360 }
```

**Path resolution convention:**
- Refs in the *project manifest* resolve relative to `stunt.yaml`.
- Refs *inside an adapter* resolve relative to the adapter root using convention dirs: `templates/`, `fixtures/`, `schemas/`, `scripts/`, `endpoints/`.

**Field semantics:**
- `network.mode`: `subdomain` → `<service>.<tld>` (no ports; needs the privileged listener or OS service); `port` → `<host>:<base_port+offset>` (fallback, no sudo).
- `network.tld`: default `localhost` (auto-resolves to 127.0.0.1 in Chrome/Firefox/Edge); repeatable to also serve `.test` etc. Avoid `.local` (mDNS) and `.dev` (HSTS).
- `network.wildcard`: when true, any unregistered `sub.<service>.<tld>` routes to `<service>` (useful for multi-tenant-style paths).
- `network.sync_hosts`: auto-sync `/etc/hosts` for route hostnames so Safari and custom TLDs resolve (Safari uses the system resolver, which may not handle `.localhost` subdomains).
- `network.spoof_real_hosts`: when true, also map the real hostnames each adapter declares (e.g. `api.stripe.com`) to 127.0.0.1, for clients that hardcode hosts.
- `state.reset`: when persisted state is cleared.
- `services.<name>.rules`: a list that **overlays** the adapter's own rules (see 6.2).
- `services.<name>.config`: free-form object passed into the adapter (e.g. quotas, feature toggles).

### 6.2 Rules engine

One rule shape, used both as adapter defaults and as manifest overlays. **First-match-wins, top-to-bottom.**

```yaml
rules:
  - name: charges succeed
    match: { method: POST, path: /v1/charges }
    respond:
      status: 200
      body: { template: templates/charges/created.json }   # static or templated
      # body: { generate: charge.created }                 # faker generator

  - name: 5% server errors
    match: { method: POST, path: /v1/charges }
    when: { chance: 5 }            # percent; if not rolled, rule skipped -> next
    respond: { status: 503, body: { template: templates/errors/503.json } }

  - name: 10% timeouts
    match: { method: GET, path: /v1/balance }
    when: { chance: 10 }
    respond: { behavior: timeout, latency_ms: 30000 }
```

**Match** supports: `method`, `path` (glob with `*` and `**`), `headers`, `query`, and `body` (JSONPath/expression predicates).

**Respond** supports: `status`, `headers`, `body.template` | `body.generate`, `latency_ms`, `behavior: timeout` (hang), and (for stateful adapters) triggering a **state transition** via Starlark.

**`when`** gates whether a rule fires:
- `when.chance: N` (percent): the rule fires only when a 0–100 roll is ≤ N; otherwise it's skipped and evaluation continues. This is the primitive for "N% of replies should error" and composes a baseline + injected faults cleanly. Deterministic under a seeded RNG (`state.rng_seed`).
- `when.expr: "<predicate>"`: a boolean expression over request/store state (e.g. `not store.charges.exists(request.path.id)`) for conditional rules such as 404-on-unknown-resource. `chance` and `expr` may be combined (both must pass).

**Overlay semantics:** for a service, the engine evaluates **project rules first**, then the **adapter's own rules** as the fallback chain. This lets a project inject faults or pin specific responses without forking the adapter.

**Templating:** templates are JSON/YAML files supporting a small expression language: `{{ request.body.amount }}`, `{{ request.headers["Idempotency-Key"] }}`, `{{ faker.id("ch") }}`, `{{ now() }}`, `{{ uuid() }}`, and access to store state for stateful adapters (`{{ store.charges.last.id }}`).

### 6.3 Core primitives (the toolbox adapters compose)

| Primitive | Purpose | Backed by |
|---|---|---|
| **Collection** | typed CRUD over named collections; relationships, filtering, pagination, seeding | SQLite |
| **Blob store** | file content (Drive/Dropbox/S3-style) | local FS |
| **KV store** | settings/config/session state | SQLite |
| **Clock + scheduler** | deterministic virtual time, delays, TTLs, scheduled transitions | in-process + SQLite durability |
| **Identity** | mint/validate tokens, fake scopes/OAuth handshake | in-process |
| **Events** | webhook emission to registered URLs with retries | background workers |
| **Generator** | faker + templating engine for bodies & IDs | embedded |
| **Validator** | validate req/resp vs OpenAPI/JSON-Schema | embedded |

Adapters *declare* wiring declaratively (e.g. "charges is a Collection with schema X, seed from `fixtures/charges.jsonl`") and reach into primitives from Starlark for custom logic. Common needs (CRUD, file storage) never get reimplemented per adapter.

### 6.4 Adapter model

**Repo layout (convention dirs):**
```
stripe/
  adapter.yaml          # metadata, endpoint index, resource models, default rules
  endpoints/
    charges.yaml        # per-endpoint rules + primitive wiring
    customers.yaml
  templates/            # response bodies (SYNTHETIC only)
    charges/created.json
    errors/503.json
  fixtures/             # static SEED data (SYNTHETIC only)
    charges.jsonl
  scripts/              # Starlark for the tricky 5%
    charges.star
  schemas/              # OpenAPI/JSON-Schema for validation + generation
    openapi.yaml
```

**`adapter.yaml`** declares:
- `id`, `name`, `real_hosts` (for `spoof_real_hosts`), `version`
- `endpoints`: index mapping routes → `endpoints/*.yaml`
- `resources`: resource models — which Collections/Blobs each resource uses, schemas, seed fixtures
- `rules`: the adapter's default rule chain
- `identity`: token scheme to simulate (optional)

### 6.5 Starlark scripting SDK

Adapters are ~95% declarative. The 5% needing logic uses sandboxed Starlark over the primitive SDK:

```python
# scripts/charges.star
def on_post(request):
    charge = store.collection("charges").insert({
        "id": gen.id("ch"),
        "amount": request.body.amount,
        "status": "pending",
        "created": clock.now(),
    })
    clock.after("2s", lambda: _settle(charge["id"]))   # deterministic settle
    events.emit("charge.updated", {"id": charge["id"]})
    return respond.json(200, charge)

def _settle(charge_id):
    store.collection("charges").update(charge_id, {"status": "succeeded"})
```

**Sandbox guarantees:** Starlark has no I/O, no `os`, no network by design — only the explicitly injected primitive API (`store`, `gen`, `clock`, `events`, `respond`, `request`). This is the trust property that lets users install community adapters safely, and is the deliberate advantage over a TS/JS adapter model.

### 6.6 Networking — named `*.localhost`, no ports (portless.dev model)

Following the **portless.dev** (vercel-labs) approach, with one stunt-specific security addition.

**Topology — privileged listener + unprivileged engine.** This is portless's own topology (privileged proxy `:443` → unprivileged app processes), which maps cleanly onto stunt:

```
client (https://stripe.localhost) ──► [privileged listener :80/:443] ──► [unprivileged stunt engine :4xxx]
                                            (TLS, HTTP/2, Host/SNI)            (adapters / Starlark / state)
```

- **Listener** binds `:80`/`:443`. It terminates TLS, speaks **HTTP/2**, routes by Host/SNI, and forwards to the engine on a high port. It does **no** untrusted work.
- **Engine** runs as the normal user, holds the manifest/state, and runs adapters (Starlark). Because the listener forwards to it, **adapter code never runs as root** — preserving the sandbox safety story (§8). This is why stunt adopts portless's topology rather than running a single root process.

**Binding low ports (no sudo-every-run):**
- `stunt up` auto-elevates with sudo to start the listener (mirrors `portless proxy start`).
- `stunt service install` installs the listener as a root-owned **OS startup service** (launchd on macOS, systemd on Linux, Task Scheduler as SYSTEM on Windows) so `:443` is bound at boot and no sudo is needed per run — survives reboots. Config (port, TLS, TLDs) is persisted and reused (won't silently revert).
- **Fallback:** `mode: port` (or `--port <n>`) binds a high port with no sudo for locked-down machines.

**TLS:** a local CA is generated on first run and auto-trusted into the OS/browser store, cross-platform: macOS Keychain, Linux `update-ca-certificates`/`update-ca-trust`, Windows `certutil`, and **both** stores under WSL. HTTP/2 + HTTPS by default (multiplexing avoids the HTTP/1.1 6-connection-per-host bottleneck); `--no-tls` for plain HTTP on :80. Bring-your-own certs supported (`--cert`/`--key`, e.g. mkcert).

**DNS:** `.localhost` auto-resolves to 127.0.0.1 in Chrome/Firefox/Edge. For Safari / custom TLDs (`.test`), `/etc/hosts` is auto-synced (`sync_hosts: true`) so hostnames resolve on the system resolver.

**Subdomains & wildcards:** `<service>.<tld>` by default; `<sub>.<service>.<tld>` for nested resources; `wildcard: true` lets unregistered subdomains fall back to the parent service. `spoof_real_hosts: true` additionally maps real hostnames (declared per-adapter) to 127.0.0.1 for clients that hardcode hosts.

### 6.7 CLI (Cobra, Hashicorp-style)

```
stunt init                       # wizard -> generates/edits stunt.yaml
stunt plan                       # validate manifest, resolve adapters, show what'll run
stunt up / down                  # start / stop engine + services (auto-starts listener if needed)
stunt status                     # running services, URLs, state summary
stunt logs [service]             # tail logs
stunt reset [service]            # wipe state
stunt adapter add|remove|list|update   # pi-style, pinned git refs
stunt catalog search|show              # browse the registry
stunt exec '<request>'                 # fire one request against a service

# networking lifecycle (portless-style)
stunt proxy start|stop           # privileged listener on :443 (auto-elevates; HTTP/2 + HTTPS)
stunt service install|status|uninstall  # install listener as OS startup service (no per-run sudo)
stunt trust                      # (re)add local CA to the system trust store
stunt hosts sync|clean           # manage the /etc/hosts block (fixes Safari/custom TLDs)
stunt doctor                     # health check: listener, routes, DNS, CA trust
stunt clean                      # remove state, CA trust entry, service, and hosts block
stunt version
```

- `stunt init` is the **wizard** entry point (choose services → write manifest). All wizard choices map to manifest fields, so the file is the single source of truth and is fully scriptable via CLI flags.

### 6.8 State management

- `./.stunt/state/` holds: `stunt.db` (SQLite: collections, KV, scheduled jobs, rng seed) and `blobs/<service>/...` (file content).
- `state.reset` controls lifecycle (`on-up` is the default for clean test runs; `never` for persistence across restarts).
- The RNG is seeded (`state.rng_seed`) so probabilistic rules are **deterministic and reproducible** in tests.

### 6.9 Adapter distribution & catalog (pi-style)

- **Sources:** `git:host/user/repo@<tag|sha>` (and head-at-install), local paths. npm later.
- **Install:** `stunt adapter add git:github.com/stunt-adapters/stripe@v3.1.0` → writes to project manifest; cached under `~/.stunt/adapters/git/<host>/<path>`.
- **Pinning:** refs are pinned; `stunt adapter update` reconciles to the pinned ref but does **not** auto-bump. Moving a ref requires an explicit `add ...@new-ref`.
- **Identity/dedup:** by repository URL (without ref); global vs project scope; project wins.
- **Catalog:** a registry of adapters keyed by the `api-sim-adapter` keyword (npm) or a curated index (git); browsable via `stunt catalog`. Website later.

---

## 7. Contributor reverse-engineering workflow

```
1. Gather   official OpenAPI / docs (HTML, md) / a HAR from YOUR OWN legit session
2. scaffold stunt adapter new <service>              # convention-dir skeleton
3. import   stunt adapter import openapi.yaml        # -> endpoints/*.yaml + templates
           stunt adapter import har session.har      # -> infer endpoints + seed fixtures
4. author   declare Collections/Blobs, tweak templates, write Starlark
5. lint     stunt adapter lint       # ENFORCES synthetic fixtures (see §8)
6. test     stunt adapter test       # conformance vs your LOCAL real traces
7. ship     git repo, tag v1, keyword api-sim-adapter -> catalog
```

---

## 8. Safety & legal model

This operationalizes the rules from the feasibility study:

- **Synthetic data only.** `stunt adapter lint` flags any shipped fixture/template that looks like raw recorded data (real-looking IDs, PII patterns, provider-copyrighted blobs). HAR import may *seed local dev state* but adapters must ship synthetic fixtures.
- **Sandboxed adapters.** Starlark has no host I/O; community adapters cannot execute arbitrary code. (Contrast: pi extensions run with full system access — `stunt` improves on this.) The adapter engine also **runs unprivileged** behind the privileged listener (§6.6), so even a sandbox escape can't do root-level damage.
- **No scraping/bypass positioning.** Framed as local dev/testing/interoperability; the user still needs a real account for production. The simulator spares the *remote* API during development — it does not enable evasion.
- **Neutral naming + non-affiliation** for any branded adapters in the first-party set.
- **Structure, not documentation.** Adapters extract API *structure*; they do not reproduce proprietary docs verbatim.

See `api-simulator-research/FEASIBILITY.md` §3 for the full legal landscape (ToS, Oracle v. Google, hiQ, DMCA §1201(f), trademark).

---

## 9. v1 scope

**In:**
- Go binary; YAML manifest; Starlark engine.
- Primitives: Collection, Blob, KV, Clock+scheduler, Identity, Events, Generator, Validator.
- Networking: `*.localhost` (portless.dev model) — privileged listener + unprivileged engine, HTTP/2+HTTPS, auto-trusted local CA, `/etc/hosts` sync, OS service install, high-port fallback.
- Full CLI (setup/init/plan/up/down/status/logs/reset/adapter/catalog/exec).
- Adapter distribution: git-pinned refs + cache + convention dirs.
- Catalog: static registry keyed by `api-sim-adapter`.
- **3 first-party adapters:** Stripe (stateful-lite: charges/customers), Google Drive (blob + metadata), X.com (pure-mock auth + a couple endpoints) — chosen to exercise Collection, Blob, and pure-mock paths. **Dropbox** (the fourth requested service) is the immediate next adapter; it reuses the Blob path proven by Drive, so it is deferred from the initial cut without loss of coverage.
- Synthetic-fixture enforcement (`adapter lint`).

**Out (later):**
- npm adapter distribution.
- gRPC / GraphQL / WebSocket.
- Full real-OAuth-flow simulation.
- Gallery website.
- Advanced async scheduling beyond Clock+scheduler.

---

## 10. Risks & open questions

- **Fidelity ceiling:** how realistic must a first-party adapter be to be useful? Define a "conformance score" target per adapter.
- **Networking cross-OS:** the portless model gives a concrete plan per OS (launchd / systemd / Task Scheduler; Keychain / update-ca-certificates / certutil). Windows/WSL support is well-defined but more work; v1 may ship macOS+Linux first, Windows after.
- **Starlark ergonomics:** contributors must learn a small Starlark SDK; mitigate with great docs + the import scaffolding doing the heavy lifting.
- **Catalog trust:** how to signal reviewed vs unreviewed adapters (signed adapters later).
- **`.localhost` on Windows:** resolution is less reliable; document/automate the `/etc/hosts`-equivalent.

---

## 11. Appendix A — full example manifest

```yaml
version: 1

network:
  mode: subdomain
  tld: localhost
  tls: true                 # HTTP/2 + HTTPS, local CA auto-trusted
  wildcard: false
  sync_hosts: true          # /etc/hosts sync for Safari/custom TLDs
  spoof_real_hosts: false

state:
  dir: ./.stunt/state
  reset: on-up
  rng_seed: 42

services:
  stripe:
    adapter: git:github.com/stunt-adapters/stripe@v3.1.0
    rules:
      - { match: { method: POST, path: /v1/charges }, when: { chance: 5 },
          respond: { status: 503, body: { template: templates/errors/503.json } } }
      - { match: { method: GET, path: /v1/balance }, when: { chance: 10 },
          respond: { behavior: timeout, latency_ms: 30000 } }
  drive:
    adapter: git:github.com/stunt-adapters/google-drive@v1.2.0
    config: { quota_bytes: 16106127360 }
  twitter:
    adapter: git:github.com/stunt-adapters/twitter@v0.4.0
```

## 12. Appendix B — adapter `endpoints/charges.yaml` (illustrative)

```yaml
route: /v1/charges
resource: charges            # declared in adapter.yaml as a Collection
rules:
  - name: created
    match: { method: POST }
    handler: scripts/charges.star#on_post    # Starlark for stateful create
  - name: list
    match: { method: GET }
    respond:
      status: 200
      body: { generate: charge.list }        # generator reads the Collection
  - name: 404 unknown
    match: { method: GET, path: /v1/charges/* }
    when: { expr: "not store.charges.exists(request.path.id)" }
    respond: { status: 404, body: { template: templates/errors/404.json } }
```
