# Changelog

All notable changes to **stunt** are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.0] — 2026-07-24

### The observability dashboard

Every running stunt server now serves its **own localhost dashboard** — a real-time
request inspector, a state browser, snapshot/restore for deterministic runs, and a
multi-instance manager. No daemon, no IPC: it reads in-process state directly. Every
feature has a matching CLI command with `--json` output for LLMs & CI. Loopback-only,
token-authed, DNS-rebinding-guarded; sensitive headers redacted; logging is async.

#### Request inspector
- **Live feed** of every request hitting your sims — REST, gRPC (unary + streaming),
  and WebSocket — with method, path, status, transport, and **sub-microsecond** timing.
- **Gap-free** WebSocket push (sequence-numbered; reconnect backfills any gap).
- **Search & filter** the feed by service, method, status, or free-text on the path.
- Row detail: full **headers** (redacted) + **bodies**, **copy-as-curl**, **replay**
  (re-issues a request in-process against current state).
- CLI: `stunt ui`, `stunt requests [--json] [--limit N] [--follow]`,
  `stunt request <id>`, `stunt replay <id>`.

#### State browser
- Browse, read-only, what your tests created: **collections** (SQLite docs, with
  counts), **kv** (namespaced pairs), and **blobs** (uploaded files). Closes the
  "did my POST create the right state?" loop that a wire-only inspector can't.
- CLI: `stunt state collections|collection|kv|blobs <svc> [--json]`.

#### Snapshot, restore & reset — deterministic runs
- **`stunt reset [<svc>] [--all]`** — wipe state to a clean slate.
- **`stunt snapshot save|load`** — capture a known-good state to a portable `.tar.gz`
  (a logical dump, not a raw DB copy) and restore the exact pre-test state, enabling
  reproducible test runs.
- Dashboard ⤓/⤒ controls mirror the CLI.

#### Instance manager
- **Global registry** (`~/.stunt/instances.json`) tracks every running server across
  all manifests; **`stunt ps`** lists them, **`stunt stop [<pid>]`** stops them.
- **Self-healing**: dead entries prune on the next read via a PID-liveness check.
- Dashboard **instances** tab lists siblings, jumps to another instance's dashboard,
  and stops any of them.

#### Reliability & internals
- Async SQLite request log (off the request hot path); bounded ring (last 1000/server);
  `busy_timeout` + writer drain fixes a `SQLITE_BUSY` race on reopen.
- `--json` on every dashboard command; documented in `stunt llm` (regrouped under a
  dedicated section) + the new exhaustive `docs/dashboard.md` guide.

### Documentation
- New `docs/dashboard.md` (9-section exhaustive reference) + 6 curated screenshots.
- README gains an "Observability dashboard" section + status bump.

---

## [0.1.0] — initial public release

The foundation: 91-adapter catalog (100% versioned, all SYNTHETIC-data-only, enforced
by `stunt adapter lint`), sandboxed Starlark adapters, SQLite state, REST/gRPC/WebSocket/
GraphQL transports, optional local-TLS subdomain proxy, OpenAPI/HAR/proto import,
`stunt plan`/`stunt doctor`/`stunt catalog`, and the LLM operability layer
(`AGENTS.md` + `stunt llm` + `--json`). Homebrew cask + winget distribution.
