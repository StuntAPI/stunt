# Changelog

All notable changes to **stunt** are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.7.0] — 2026-08-13

### Starlark handler API

- **Deterministic identity-token timing.** `identity.Issuer` now mints and
  validates against the engine's injectable clock (`NewIssuerWithClock`),
  instead of `time.Now()`. Token expiry no longer leaks wall-clock time, so
  auth flows are deterministic for record/replay. `NewIssuer` (real clock) is
  unchanged, so existing callers behave identically.

## [0.6.0] — 2026-08-13

### Starlark handler API

- **`blob.append` + `blob.Store.Append`.** The blob store gains an append path
  (`O_APPEND`, returns the new total size, preserves `content_type` from
  creation) so resumable uploads append each chunk in place — O(chunk)
  regardless of how large the partial has grown — instead of reading,
  concatenating, and re-writing the whole blob per chunk (the O(k·N²)
  read-concat-rewrite). A failed append rolls back to its pre-append size, so a
  caller retry re-appends exactly once. Append is not linearizable across
  concurrent appends to the same id; the caller must serialize (the Graph
  upload endpoint already does, via `concurrency_key`).
- **`clock.unix_to_rfc3339(unix_seconds)`.** Renders a Unix timestamp as
  RFC3339. Accepts int or float — timestamps stored in a collection round-trip
  through JSON as floats, so a strict int unpack rejected them.
- **`engine.WithClock` option.** `New` accepts an optional clock so time-based
  adapter behavior is testable against a virtual clock (production defaults to
  real time).

### Adapters

- **microsoft-graph-style: real resumable-upload lifecycle.** Upload sessions
  now expire at a real ~48h TTL (was a `2030-…` placeholder) and abandoned
  sessions plus their partial blobs are reclaimed — sweep-on-create and
  expire-on-access, both pure Starlark on the injectable clock. The chunk
  handler uses `blob.append`; the final range reads the assembled bytes once.
  HTTP contract of `/v1.0/_upload/{session}` (202/201/404/416/409) is
  unchanged. Closes #17.

## [0.5.1] — 2026-08-12

### Engine

- **Capped response-body capture.** The request-log recorder no longer buffers
  the entire response body in memory for the request lifetime — it tees the
  response stream the way the request side does, retaining at most a 64 KB
  sample while writing every byte through to the client. Fixes the multi-MB
  media-response duplication (#5 part 1). Abandoned resumable-upload sessions
  and quadratic blob rewrites (#5 part 2) remain a low-priority follow-up.

## [0.5.0] — 2026-08-12

### Starlark handler API

- **Webhook-delivery signing.** Adapters can now compute (not just set) a
  webhook signature, so a real receiver's signature-verification code path can
  be exercised against stunt (Stripe, GitHub, Shopify, Slack, Twilio, Adyen).
  Three additions:
  - **`crypto` module** — `hmac_sha256`, `hmac_sha1`, `sha256` (with an
    `encoding="hex"|"base64"` kwarg), plus `base64_encode`/`base64_decode`.
    Stateless, registered next to `json`. Scope: MAC + hash only.
  - **`events_body(event_type, payload?)`** — returns the **exact** on-wire
    JSON body `events_emit` will POST, via a single shared `events.MarshalEnvelope`
    so a signature computed over it verifies against the bytes the sink receives
    (the load-bearing invariant, pinned by a byte-equality test).
  - **`clock` module** — `now_unix()` / `now_rfc3339()`, backed by the engine's
    injectable `clock.Clock` (real today; the virtual mode is the seam for
    future record/replay) — no new determinism leak.
  - Tests: crypto KAT (RFC/HMAC vectors incl. a corrected SHA256("") vector),
    `clock` determinism, the `events_body`↔`Emit` byte-equality pin, and
    end-to-end Stripe-style + GitHub-style deliveries verified with the real
    SDK formulas on an httptest sink.
  - Out of scope (follow-up): converting the in-repo adapters' documented-but-
    uncomputed schemes into working code, and retrofitting `identity` onto the
    injectable clock.

## [0.4.2] — 2026-08-12

> v0.4.1 was tagged but the release Action failed at the `just ci` gofmt gate
> (an unformatted `engine.go`); it never published. v0.4.2 is the first
> published 0.4.x — same #4 change, plus the formatting fix.

### Adapters

- **Per-endpoint request serialization (`concurrency_key`).** An endpoint can
  declare `concurrency_key: <param>` in `adapter.yaml`; concurrent calls sharing
  that path-param value (e.g. a Graph upload session id) then run under a
  per-key lock, so a handler's read-modify-write across stores is atomic per
  key. Closes the microsoft-graph-style resumable-upload TOCTOU (#4): two
  concurrent chunks on the same session now yield one `202` and one `416`
  instead of a corrupted/duplicated write. Opt-in; handlers are unchanged, and
  calls with different keys still run in parallel.

## [0.4.0] — 2026-08-12

### Webhooks

- **Custom headers on `events_emit`.** `events_emit(event_type, payload?, headers?)`
  now accepts an optional `headers` dict, applied to the outgoing webhook POST
  on top of the default `Content-Type: application/json` (a caller `Content-Type`
  overrides it). Adapters can now set `Stripe-Signature`, `X-Hub-Signature-256`,
  `X-GitHub-Event`, and any other delivery header — closing the asymmetry with
  `respond`, which already accepted headers. Reserved headers (`Host`,
  `Content-Length`) and any CR/LF are rejected to prevent header/request
  smuggling. (Computing a signature in Starlark — HMAC/clock/stable-body
  primitives — is tracked separately as the webhook-signing follow-up.)

## [0.3.0] — 2026-08-12

### Server lifecycle (`stunt stop` / `stunt down`)

- **Cross-platform server shutdown.** `stunt stop`/`down` now work on Windows.
  Previously they failed — Go's `os.Process.Signal` only honors `os.Kill` on
  Windows, so the Unix signal path errored (`pid not running`) and left the
  server holding its port. Process control is now build-tag-split
  (`proc_unix.go` / `proc_windows.go`); the Windows liveness probe uses
  `OpenProcess` + `GetExitCodeProcess`, and registry pruning of dead entries
  now works on Windows too.
- **Graceful shutdown via the dashboard.** A new authenticated `POST /api/shutdown`
  endpoint cancels the server's shutdown context (the same path Ctrl-C/SIGTERM
  takes), so `stunt stop`/`down` — and the dashboard's instance-stop button —
  shut the server down **gracefully** (draining in-flight requests, freeing the
  port cleanly) on every platform, falling back to a platform signal then a hard
  kill only if the dashboard is unreachable. Windows stops are no longer
  hard-kills.

### Starlark handler API

- **`req` supports attribute access.** `req.method`, `req.headers`, `req.body`,
  … now work alongside the existing dict style (`req["method"]`, `req.get("query")`).
  Previously `req` was a plain dict, so `req.headers` failed at runtime. Both
  styles work; no adapter changes required.
- **Case-insensitive request headers.** `req.headers` lookups are now
  case-insensitive — `req.headers.get("authorization")`, `.get("Authorization")`,
  and `req.headers["AUTHORIZATION"]` all resolve to the same value — and rule
  `match.headers` matching is case-insensitive too. The per-adapter lowercase
  workarounds in `lib.star` are now harmless redundancy.

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
