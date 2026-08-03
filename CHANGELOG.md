# Changelog

All notable changes to **stunt** are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Engine

- **Configurable request body limit with honest overflow.** Services can set
  `max_body_bytes` in `stunt.yaml` (default stays 1 MiB). Oversize bodies now
  return **413** instead of being silently truncated; the request-log recorder
  tees the body stream (capture stays capped at 64 KB) so handlers always see
  the full, untruncated bytes.
- **`req["host"]` in Starlark handlers.** The request Host header is injected
  into the request dict, so adapters can mint self-referential URLs (media
  `baseUrl`, upload session `uploadUrl`) that point back at the simulator.

### Adapters

- **photos-style:** real media plane. Uploaded bytes are stored and linked to
  created media items; `baseUrl` is computed at read time from the request
  host; new `GET /v1/media-dl/{id}` with strict `=d`/`=dv` semantics (bare
  baseUrl serves a distinct derivative payload); new `GET /v1/mediaItems/{id}`;
  list/search honor `pageSize`/`pageToken` and emit `nextPageToken`.
- **microsoft-graph-style:** strict OneDrive write plane. Simple upload
  (`PUT root:/{name}:/content` + folder variant) with real conflictBehavior
  semantics, createFolder, per-parent child listing, path resolution with
  `?select=id`, `GET items/{id}/content`, and the full resumable upload
  protocol (`createUploadSession`, self-referential `uploadUrl`, sequential
  Content-Range chunks with 416 on violations, 202 + `nextExpectedRanges`,
  201 + driveItem on the final range, session invalidation).

## [0.2.2] — 2026-07-24

### Housekeeping

- **History scrub:** removed an internal QA/dogfood report
  (`DOGFOOD_REPORT_PLAN1.md`) that had been accidentally committed. It contained
  no secrets (only a synthetic test token, `sk_df_SECRET_xyz`) — purely internal
  process notes. The file is gone from the repo, all history, and `.gitignore`
  now blocks `DOGFOOD_REPORT*.md` / `*-findings.md`. Released as a new version
  only because the Go module proxy is immutable (v0.2.1's cached zip still
  carries the file); v0.2.2 is the first clean version.

## [0.2.1] — 2026-07-24

> v0.2.0 was tagged but never published (a Windows cross-compile bug — Unix-only
> `unix.Flock` compiled under `GOOS=windows` — slipped past the host-only CI gate;
> and the poisoned version is immutable in the Go module proxy). This is the first
> published 0.2.x: same features, with the cross-platform build fix.

### Cross-platform build fix

- Build-tagged the Unix-only `unix.Flock` (registry file lock) and the test's
  `syscall.Kill` (self-SIGTERM) so the **Windows** cross-compile succeeds — they now
  live in `registry_unix.go` / `registry_windows.go` (Windows = no-op; atomic temp-rename
  + PID-pruning heals any race) and `sigterm_{unix,windows}_test.go`. All six
  GoReleaser targets (linux/darwin/windows × amd64/arm64) now build; cross-platform
  `go vet` is clean.

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
