# Changelog

All notable changes to **stunt** are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.26.0] — 2026-08-14

### Added

- **`parse_multipart(content_type, body)` builtin** — decodes
  `multipart/form-data` requests inside handlers, returning
  `(parts, err)` with each part as `{name, filename, content_type, data}`
  (raw bytes; binary parts round-trip through `store_blob` exactly). Unblocks
  document/file-upload endpoints (Onfido/Jumio document scans, WhatsApp media,
  Cloudflare Worker deploys, Drive multipart uploads, Pinata pinning) that
  previously had to fall back to JSON shims. First wired in `whatsapp-style`,
  whose media upload now accepts the real Cloud API shape and whose media
  metadata reports the upload's real `sha256`/`file_size` with a local
  content-download endpoint (`GET /v21.0/{media_id}/content`) for byte-exact
  retrieval.
- **`crypto.ec_public_jwk(pub_pem)`** — the EC P-256 public key's JWK params
  `{kty, crv, x, y}` (base64url, fixed 32-byte coords), the ES256 counterpart
  to `rsa_public_jwk` for serving JWKS the way Sign in with Apple, APNs, and
  Apple Search Ads do.
- **`crypto.base64url_decode(s)`** — decodes (unpadded) base64url, tolerating
  padding; the inverse of `base64url_encode` for reading JWT segments.


## [0.25.0] — 2026-08-14

### Documentation

- **`stunt llm` builtin summary refreshed** — now lists `paginate`,
  `events_target`, the asymmetric crypto set (`ecdsa_sign_p256`,
  `rsa_sign`, `ed25519_sign`, `rsa_public_jwk`, `base64url_encode`), `raw_body`,
  and OData embedded-param routing (previously HMAC-only). Agent harnesses
  capturing `stunt llm` get the current handler API.
- README winget install path bumped to the current release.

## [0.24.0] — 2026-08-14

### Adapters

- **discord-style: WebSocket Gateway + Ed25519-signed interactions.** The
  dominant bot pattern — event-driven over the Gateway plus signed interactions
  — was entirely absent:
  - **WebSocket Gateway** (`ws /gateway`): sends HELLO, awaits IDENTIFY, sends
    READY, then dispatches a synthetic `MESSAGE_CREATE`.
  - **Ed25519-signed outbound**: `MESSAGE_CREATE` deliveries signed with Ed25519
    over timestamp+body (`X-Signature-Ed25519` + `X-Signature-Timestamp`).
  - **Inbound `POST /interactions`**: verifies the Ed25519 signature
    (ts+raw_body); valid → 200 (PONG/deferred), invalid → 401.

## [0.23.0] — 2026-08-14

### Starlark handler API

- **`crypto.ed25519_sign` / `crypto.ed25519_verify`** — Ed25519 signature over
  the raw message (the last asymmetric scheme the crypto module lacked),
  unblocking Discord's signed interactions and the deferred
  discord/plaid/sendgrid signing work. PEM keys in, encoded signature out
  (hex/base64/base64url); verify returns `False` on a bad signature.

## [0.22.0] — 2026-08-14

### Adapters

- **salesforce-style: general SOQL query engine.** Beyond `WHERE Id = '...'`, the
  query endpoint now evaluates a general WHERE clause — comparators
  (`=, !=, <, <=, >, >=`), `IN ('a','b')`, `LIKE 'pattern'` (`%` wildcards),
  and `AND`/`OR` with correct precedence — plus `ORDER BY field [ASC|DESC]` and
  `LIMIT`/`OFFSET` paging. Non-trivial read paths are now testable.
  (SObject Collections bulk DML + JWT bearer grant remain.)

## [0.21.0] — 2026-08-14

### Adapters

- **CRUD teardown (DELETE) across REST adapters.** ~18 adapters that implemented
  Create+Read but not Delete now support `DELETE` for their store-backed
  resources (reusing each adapter's auth + not-found helpers, real-provider
  status codes), so create→delete lifecycle test flows can clean up:
  aws-s3 (buckets), cloudflare (zones/workers/r2/d1), firebase (Firestore
  documents), gmail (messages/drafts/labels), google-admin (groups), jira
  (issues), microsoft-graph (messages/events/driveItems/teams channels),
  photos (albums), qbo (customers/invoices), shopify, square, xero, youtube,
  zendesk, netsuite, apps-script. (gcalendar already had full CRUD.)

## [0.20.0] — 2026-08-14

### Engine

- **OData key-in-parens routing.** `matchRoute` now supports an embedded single
  `{param}` within a literal segment (`prefix{param}suffix`, e.g.
  `/accounts({accountid})`), capturing the middle — so OData single-entity URLs
  that previously 404'd now route. Whole-segment `{name}` and literal segments
  are unchanged.

### Adapters

- **powerplatform-style Dataverse write side.** `POST`/`PATCH`/`DELETE
  /accounts` and `GET /accounts({accountid})`, backed by a collection seeded
  from the fixture; list honors `$select` (projection) and `$count`
  (`@odata.count`), with `@odata.nextLink` paging.

## [0.19.0] — 2026-08-14

### Adapters

- **Byte-exact binary round-trip (aws-s3, azure-storage).** Object content
  moved out of the JSON-backed collection (which stored a stringified body map
  for JSON, `""` for binary, and corrupted invalid-UTF-8 bytes via
  `json.Marshal`) into the byte-exact filesystem-backed blob store via
  `req.raw_body`, keyed by a path-safe per-object id. The collection holds
  metadata only; size/Content-Length come from the real bytes. Binary
  upload-then-GET now round-trips exactly. (`req.raw_body` documented in
  `adapters/README.md`.)

## [0.18.0] — 2026-08-14

### Adapters

- **OAuth2 refresh_token grant.** x-articles-style, threads-style,
  salesforce-style, and bluesky-style now issue a refresh_token they honor
  (and rotate on use), so the 401→refresh→retry loop is testable. (google,
  linkedin, qbo, reddit, aws-cognito already supported refresh.)
  - x-articles: `refresh_token` grant added (was `unsupported_grant_type`).
  - threads: auth-code now returns a `refresh_token`; refresh grant added.
  - salesforce: `_issue_token` mints + returns a `refresh_token`; the refresh
    branch now validates it.
  - bluesky: new `POST /xrpc/com.atproto.server.refreshSession` — the minted
    `refreshJwt` is now usable.

## [0.17.0] — 2026-08-14

### Adapters

- **tenderly-style: revert/failure path + execution artifacts.** The simulator's
  core "will this tx revert, and why?" was unreachable (every result hardcoded
  `status=true`, empty trace). Now reachable two ways — an explicit
  `{"revert": true, "revert_reason"}` body flag, or calldata beginning with the
  `Error(string)` selector `0x08c379a0` — producing `status: false`, a
  `revert_reason`, and an ABI-encoded revert output. Value transfers synthesize
  a Transfer event log + balance overrides. Stored simulations are retrievable
  via `GET .../simulations/{id}` and listable via `GET .../simulations`.

## [0.16.0] — 2026-08-14

### Adapters

- **Idempotency across the payment/ERP adapters.** Completes the idempotency
  gap (`stunt-9of`; stripe shipped in v0.15.0, square already had its own):
  - **adyen-style / braintree-style** — honor the `Idempotency-Key` header on
    creates (replay the original response, scoped by method+path+collection+key;
    2xx only), so retry/idempotency logic is testable.
  - **netsuite-style** — NetSuite's model is `externalId` upsert: a second create
    with the same `externalId` returns the existing record's `Location` instead
    of duplicating.

## [0.15.0] — 2026-08-14

### Adapters

- **stripe-style: Idempotency-Key on payment writes.** A write carrying the
  `Idempotency-Key` header now replays the original response verbatim (scoped by
  method+path+collection+key so a reused key never collides across endpoints;
  only 2xx responses are cached). Retrofitted on payment_intents
  create/confirm/capture, charges create, and refunds create — so retry/
  idempotency logic is testable against the flagship adapter. (square-style
  already had its own; adyen/braintree/netsuite follow the same pattern.)

## [0.14.0] — 2026-08-14

### Adapters

- **stripe-style: PaymentIntents, PaymentMethods, first-class Refunds.** Adds
  Stripe's canonical modern payment objects (Charges-for-direct-use is
  deprecated) so the common integration path is testable:
  - **PaymentIntents** — create/retrieve/list + confirm + capture with the real
    state machine (`requires_payment_method` → `succeeded` [automatic] /
    `requires_capture` → capture → `succeeded`); `confirm: true` at create.
  - **PaymentMethods** — create/retrieve/attach/detach/list (synthetic card).
  - **Refunds** — first-class `/v1/refunds`: full or partial over a
    `payment_intent` or `charge`, `reason`; decrements `amount_received`.
  - Signed webhook events on transitions (`payment_intent.*`, `refund.created`,
    `charge.refunded`); paginated lists with `?customer=` / `?payment_intent=`.

## [0.13.0] — 2026-08-14

### Adapters

- **entra-id-style issues real RS256 JWTs.** The access token is now signed
  with RS256 (`rsa_sign` over the real base64url `header.payload`) instead of
  the literal `"mock-signature-..."`, and the adapter serves
  `/common/discovery/v2.0/keys` JWKS so a server-side token-verification flow
  runs end-to-end. The minted token verifies against the served JWKS in a new
  engine test. Same recipe applies to the remaining JWT adapters.

### Starlark handler API

- **`crypto.rsa_public_jwk(public_key_pem)`** — returns the RSA public key's
  JWK params `{kty, n, e}` (base64url) for serving JWKS endpoints.
- **`crypto.base64url_encode(data)`** — real URL-safe base64 (no padding) for
  JWT header/payload segments.

## [0.12.0] — 2026-08-13

### Starlark handler API

- **Asymmetric signature primitives.** The `crypto` module gains ECDSA P-256
  and RSA sign/verify — the algorithms JWT issuers and signed webhooks use that
  a symmetric MAC cannot satisfy:
  - `ecdsa_sign_p256` / `ecdsa_verify_p256` — ES256, raw r‖s (64 bytes).
  - `rsa_sign` / `rsa_verify` — RS256, PKCS#1 v1.5 + SHA-256 (deterministic).
  - `encoding` now accepts `base64url` (JWT segments); new `decodeDigest` for
    verify.
  - Keys arrive as PEM strings the adapter supplies (ship a fixed keypair for
    determinism); charter expanded to MAC + hash + asymmetric signature (still
    no encryption/KDF/RNG/key-gen). `verify` returns `False` on a bad
    signature, errors only on an unparseable/wrong-type key.
  - Unblocks (per-adapter follow-ups): RS256 JWTs + JWKS for signin-with-apple,
    entra-id, aws-cognito, firebase, google-style, appstoreconnect; ES256 for
    sendgrid; the asymmetric webhook signers deferred in v0.8.0/v0.11.0.

## [0.11.0] — 2026-08-13

### Adapters

- **More signed webhook deliveries.** Extended the v0.8.0 signing sweep to
  three providers whose real webhooks HMAC every delivery, each verified
  against the real formula in a new engine test:
  - **whatsapp-style (Meta)** — `X-Hub-Signature-256: sha256=<hex>`; secret
    `whatsapp_stunt_mock_app_secret_2026`.
  - **square-style** — `X-Square-HmacSha256-Signature: base64(...)` over the
    notification URL + body; key `sq0sip_stunt_mock_signature_key_2026`.
  - **twilio-style** — `X-Twilio-Signature: base64(HMAC-SHA1(AUTH_TOKEN,
    url+body))`; closes the v0.8.0 "twilio deferred" item.

### Starlark handler API

- **`events_target()` builtin** — returns the service's currently-registered
  webhook URL (or `None`). Needed by providers (Twilio, Square) whose
  signature MACs the destination URL; backed by a new `Emitter.Target`
  accessor.

## [0.10.0] — 2026-08-13

### Adapters

- **List pagination across the REST corpus.** Retrofitted cursor paging onto
  every REST list endpoint using the `paginate` builtin, each surfacing the
  next cursor in its provider's own envelope: Google family
  (`pageSize`/`pageToken` → `nextPageToken`), Slack (`response_metadata.next_cursor`),
  Square (top-level `cursor`), Shopify REST (`Link: rel="next"` header),
  OData/Microsoft/Azure (`@odata.nextLink`), HubSpot-style (`after`), and
  Zendesk/Jira/Twitter/Xero/Google-Workspace/drive/dropbox plus the rest.
  `limit` missing/`<=0` disables paging, so unmodified callers keep prior
  behavior. GraphQL connection fields, gRPC unary lists, and non-list
  endpoints were intentionally skipped.

### Tests

- **`TestQCAllAdapterScriptsParse`** — a permanent guard that parses every
  reference adapter's Starlark upfront. `TestQCBootAllReferenceAdapters`
  validates manifests but parses handler scripts lazily at request time, so a
  Starlark syntax error (e.g. a Python-ism like `try/except`) previously
  survived boot and surfaced only as a 500. The new guard catches such errors
  in the test suite instead.

## [0.9.0] — 2026-08-13

### Starlark handler API

- **List pagination builtin.** New pure `paginate(items, limit?, cursor?)`
  builtin, available in every handler VM, returns `(page, next_cursor)`.
  `limit` of `None`/`<=0` disables paging (so unmodified handlers are
  unchanged); `cursor` is an opaque offset token. The adapter owns the
  provider envelope (`has_more` / `nextPageToken` / `@odata.nextLink`) and
  maps its cursor query param to the token — see the new Pagination section
  in `adapters/README.md`. First slice of closing the cross-adapter
  pagination gap.

### Adapters

- **stripe-style** list endpoints (charges, customers, accounts, transfers,
  payouts) now honor `limit` (1–100, default 10) and `starting_after`
  (cursor by object id) and report `has_more`. An unknown `starting_after`
  id returns a Stripe-shaped 400 instead of silently paging from the start.

## [0.8.0] — 2026-08-13

### Adapters

- **Computed webhook signatures.** The stripe-style, github-style, and
  shopify-style adapters now **compute and attach** their provider's real
  webhook signature on every delivery (previously documented only), so a
  receiver's signature-verification code path runs against stunt. Each adapter
  signs the exact `events_body` bytes with a fixed, documented mock secret
  (configure your receiver with it):
  - **stripe-style** — `Stripe-Signature: t=<unix>,v1=<hex>` over
    `"{t}.{body}"`; secret `whsec_stunt_mock_…`.
  - **github-style** — `X-Hub-Signature-256: sha256=<hex>` + `X-GitHub-Event`;
    SHA-256 only (legacy SHA-1 omitted); secret `stunt_mock_github_…`. Routing
    `workflow_dispatch` through the subscription gate also fixes a pre-existing
    inconsistency (it emitted even with no hook registered).
  - **shopify-style** — `X-Shopify-Hmac-SHA256` (**base64**); secret
    `shpss_stunt_mock_…`.
  - Each delivery is verified against the real provider formula in a new
    engine-integration test. Stunt POSTs its `{type, payload}` envelope, so the
    raw-body MAC verifies but this exercises the signature-verification path,
    not the provider's event-schema parser.
  - Deferred (need schemes the current primitives don't cover): adyen-style
    (in-body derived-field signature), braintree-style (SHA-1 + raw-byte keys +
    form delivery), twilio-style (SHA-1 over URL + body hash).

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
