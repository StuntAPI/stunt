# Changelog

All notable changes to **stunt** are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.39.0] — 2026-08-16

### Adapters

- **threads-style**: TEXT media containers finish processing immediately.
  Real Threads only requires polling container status for video/image
  uploads — text posts are finished at creation, so real-world clients
  create and publish back-to-back. The simulator was enforcing a 3-second
  processing window on text too, rejecting `threads_publish` with the
  "not finished processing" error. Found by dogfooding (omnipost's
  production threads adapter driven end-to-end against stunt); the
  `simulate_fail` branch and the status-poll endpoint keep the
  derive-on-read machinery.

## [0.38.0] — 2026-08-16

### Adapters

- **stripe-style: full API coverage** — 48 → **158 endpoints**, 15 → 32
  collections. Every core Stripe resource family is now simulated:
  - **Test Clocks** (`/v1/test_clocks` + the real-path
    `/v1/test_helpers/test_clocks` aliases): a KV time offset drives every
    adapter timestamp, so subscription renewals, dispute settlement, payout
    lifecycles, capability activation and session expiry are deterministic
    and assertable without sleeps.
  - **Disputes**: test-card triggered (fraudulent / product_not_received)
    with the `needs_response → under_review → won/lost` derive-on-read
    lifecycle, all 27 evidence fields, submit/close, evidence due-by, funds
    withdrawn and reinstated through the ledger, the full
    `charge.dispute.*` event set.
  - **Balance transactions**: a real ledger over every money movement
    (charge incl. the 2.9%+30¢ processing fee, refund, refund_failure,
    payout, transfer, transfer_reversal, application_fee(+refund), dispute,
    dispute_reversal), account-scoped rows, full filter set.
  - **Billing**: products, prices, subscriptions (clock-driven renewal +
    auto-charge, `past_due` on decline/no-PM, coupon + tax support),
    subscription items, usage records (metered), invoices (draft → open →
    paid/void/uncollectible; finalize/pay/void/send/lines/upcoming),
    invoice items, credit notes (issuing real refunds), coupons, promotion
    codes, tax rates (exclusive + inclusive).
  - **Checkout Sessions**: payment/subscription/setup modes, hosted
    `/c/pay/{id}` completion page with `{CHECKOUT_SESSION_ID}` redirect
    substitution, decline injection via `payment_method`, expire, line
    items. **SetupIntents** with SCA challenge + decline behavior.
  - **Webhook endpoints**: CRUD + registration-gated delivery per
    `enabled_events` (events always recorded in `/v1/events`).
  - **Files + file links**: multipart upload with real purpose enum.
  - **Connect depth**: persons, capabilities (requested → pending → active),
    external bank accounts (last4 only, default resolution), application
    fees (+ partial refunds, both refund routes), login links, transfer
    partial reversals (`trr_*`, list/retrieve), payout full lifecycle
    (pending → in_transit → paid) + cancel with funds return.
  - **Refunds completion**: cancel (pending → canceled with
    `failure_reason` + `refund_failure` ledger row), charge/payment_intent
    list filters, `balance_transaction` linkage, uncaptured-charge refunds
    release the authorization.

### Engine

- Adapter lint: the provider-ID heuristic now requires a digit in the
  suffix, so real API names (`file_links`) are no longer flagged while
  actual ids (`ch_1Mio2eLkdIwHu7ix`) still are; regression tests added.

## [0.37.0] — 2026-08-16

### Adapters

- **Final P3 slices — the 603-gap sweep is complete.**
  - microsoft-graph: Excel table rows persist (add/list/patch/delete);
    mail folder-scoped lists, drafts create/send, isRead/flag updates;
    calendar single-event GET, accept/decline/tentativelyAccept with
    recorded responses, calendarView windows; numeric `$skip`.
  - xero: invoice totals across ALL line items (integer-cent discount
    factoring, SubTotal/TotalTax/Total/TotalDiscount); partial-payment
    ledger (PAID exactly at zero, over-payment and non-AUTHORISED
    rejected with the real validation messages); contacts PUT true upsert
    with active-name uniqueness.
  - salesforce: reusable refresh tokens (real model, 2h access expiry
    enforced), external-ID upsert ({id, success, created}), composite
    `{ref}` substitution, queryLocator queryMore, SObject Collections
    bulk DML with allOrNone.
  - dune: parameterized execution ({{param}} substitution, 400 on
    missing required), results-by-id JSON + CSV, next_uri pagination.

## [0.36.0] — 2026-08-16

### Adapters

- **Real GraphQL execution for 5 adapters** (thegraph, producthunt, shopify,
  github, braintree — previously substring pattern-matchers): provider-real
  SDL schemas + resolvers over the same collections the REST surface uses,
  via the manifest `graphql:` transport. Arguments, variables, aliases,
  fragments, `@skip/@include`, `__typename`, and introspection all work;
  unknown fields/operations return real errors. Highlights: graph-node
  semantics (where-suffix filters, orderBy, BigInt/BigDecimal, joins);
  Relay connections with per-field payload errors (producthunt); Admin
  2024-10 with `gid://` translation, query/sortKey, mutations with
  userErrors and REST-identical signed webhook payloads (shopify); v4
  conventions with base64 global ids, Actor interface, and issue/comment
  mutations (github); search criteria + charge/void/refund mutations
  (braintree).
- **Final P2 slices.** braze (persisted track events/purchases, export/ids,
  alias merging, validated campaign triggers), hubspot (`/search`
  filterGroups with the full operator set, batch breadth, archive + restore),
  apple-music (library CRUD, ratings, catalog/charts), gsearchconsole
  (dimension cross-product analytics with aggregation, real URL inspection,
  sites lifecycle), netsuite (`!transform` chain, +2 record types), chainlink
  (Functions lifecycle, upkeeps, feed rounds), cognito (refresh grant,
  ForgotPassword, GlobalSignOut, NEW_PASSWORD_REQUIRED challenge).
- **Resumable uploads** (S3 multipart uploadId/parts with InvalidPart, Azure
  Put Block/Put Block List, drive + youtube sessions with keyed
  `concurrency_key` chunk serialization and 308/Range semantics).
- **Soft-delete semantics** across stripe (`deleted:true`), qbo
  (`Active=false`), xero (VOIDED), salesforce (`isDeleted` + `queryAll`),
  dropbox/graph (trash cascade + restore).
- Review fixed: producthunt Int-variable coercion + numeric NEWEST ordering,
  shopify webhook payload parity, keyed upload sessions (chunk-retry
  corruption), ms-graph frozen timestamps.

## [Unreleased]

### Added

- **`smartbill-style` adapter** — a SmartBill-style Romanian invoicing
  API simulator on the real version-free surface: invoices
  (create/get/cancel/restore/paymentstatus with per-line VAT totals),
  estimates, purchase invoices, payments (`{payment: {...}}` envelope)
  that drive paid/unpaid transitions, warehouse-grouped stocks,
  document/send, tax/series metadata; Basic auth, JSON-numeric money,
  `errorText` errors, documents read by `cif`+`seriesname`+`number`.
  Includes a conformance test. Adapter count is now **94**.

## [0.34.0] — 2026-08-15

### Adapters

- **Eight P2 slices.** paypal (fixed a 500 on every refund — non-iterable
  string in the cents parser; authorization lifecycle with
  ORDER_NOT_APPROVED / AMOUNT_EXCEEDS_AUTHORIZATION / terminal 422s, refund
  PENDING→COMPLETED with refunded_amount bookkeeping), jira (real field
  storage with per-field 400s, full JQL — IN/NOT IN/`~`/IS EMPTY with
  AND/OR precedence and ORDER BY, workflow-constrained transitions with
  resolution semantics, comment CRUD), github (PR PATCH/merge/reviews,
  issue comments + labels, state validation), shopify (order
  create/cancel/close state machine, financial_status derived from the
  transaction history, line-item fulfillment, customer CRUD, product
  field-merge), psd2 (payment initiation with consentId validation and
  RCVD→ACTC→ACSC/RJCT lifecycle, consent-bound account access, SCA
  intermediate states — plus fixed float-coercion bugs that had made token
  expiry dead code), revenuecat (real RC subscriber schema, entitlement
  expiry math, receipt validation, lifecycle webhooks), plaid (sync
  modify/remove with cursor math, link_token-bound sessions, institutions
  + sandbox endpoints), square (autocomplete semantics, ListPayments/
  ListPaymentRefunds, refund ceilings + currency match, orders/calculate).

### Added

- **`fattureincloud-style` adapter** — a Fatture in Cloud-style bookkeeping API
  v2 simulator: entities (companies), received and issued documents (full CRUD
  + metodata categories), suppliers, clients, products (full CRUD), taxes,
  cashbook, webhooks, archive. Models the v2 conventions client code gets
  wrong against thinner sims: Laravel pagination envelopes (`last_page` must
  be followed), `{data}` wrappers, company-id scoping where a foreign id is an
  indistinguishable 404, genuine 401s for missing bearers, and amounts as
  decimal strings (`"9800.00"`). Conformance test included.
- **`escrow-style` adapter** — an Escrow.com-style transaction API simulator
  (public 2017-09-01 surface): create with parties/items/schedules and a
  caller-controlled or defaulted fee split, per-party agreement via PATCH
  action, secured-state transitions, lookup by id or caller reference, webhook
  registration, and a clearly-namespaced `/sim/transaction/{id}/fund`
  affordance standing in for the hosted payment page no API can drive.
  Conformance test included.

- **`json_safe_decode(s)` builtin** — total JSON decode for handlers
  validating untrusted JSON (JWT claims, multipart metadata): returns the
  value or `None` instead of raising. Numbers decode as ints when integral,
  matching the stdlib `json.decode`.

## [0.33.0] — 2026-08-15

### Adapters

- **Real inbound JWT verification (stunt-nu5).** Six auth adapters now
  verify inbound JWTs cryptographically and serve real JWKS from fixed
  synthetic key material: signin-with-apple (ES256 id_tokens +
  client_secret verification, `/auth/keys` JWKS), apple-apns (ES256
  provider-token signature + expiry, 403 ExpiredProviderToken /
  InvalidProviderToken), aws-cognito (real RS256 tokens with kid and claim
  sets, pool-path JWKS, inbound signature/iss/exp/token_use), entra-id
  (minted tokens carry aud/iat/exp and are signature-verified with kid/iss/
  aud cross-checks), google-style + google-iam (RS256 id_tokens on openid
  scope, `/oauth2/v3/certs` JWKS, JWT-bearer grants cryptographically
  verified). Malformed tokens 4xx cleanly.
- **Eight P2 slices.** stripe: decline/SCA magic test cards (real
  decline_codes, requires_action + 3DS confirmation), refund state machine
  with fleet-wide over-refund guard, `GET /v1/events`. drive: real `q`
  grammar, OAuth2 with validated tokens, live changes feed.
  appstoreconnect: appStoreVersions lifecycle (PREPARE_FOR_SUBMISSION →
  READY_FOR_SALE), app PATCH, bundleId dedupe. anaplan: chunked file
  upload/download, imports that apply uploaded data, exports symmetric.
  azure-servicebus: peek-lock receive with per-delivery rotating LockTokens
  (complete/renew/abandon/defer, 410 lock-lost), topics + subscription
  fan-out. marketo: custom-field upserts with per-record sync status, bulk
  extract with downloadable results, /leads/describe. firebase:
  token-bound getAccountInfo, securetoken refresh, Firestore runQuery,
  documentId + nested paths, FCM topic/condition routing. gdocs: structural
  document model with the real batchUpdate vocabulary (utf16-aware
  insertText, deleteContentRange, paragraph/text styles, bullets, page
  breaks, images; unknown requests 400). jumio verified complete from the
  async slice.

## [0.32.0] — 2026-08-15

### Adapters

- **Seven P1 sev-3 gaps closed** (braintree, apple-searchads, adyen, zuora,
  azure-devops, helius, cloudflare — 32 gaps, 50 endpoints, 56 test cases):
  - **braintree** — full transaction state machine (authorized →
    submitted_for_settlement → settled, derive-on-read) with real error codes
    (void 91506, refund 91507, over-refund 91521, amount 81501), partial
    captures, authorization expiry, advanced_search with the real criteria
    vocabulary, and subscriptions/plans with billing cycles + signed
    subscription webhooks.
  - **apple-searchads** — ES256 client-secret JWT → bearer OAuth2 exchange
    (validated, stored, expiring), campaign PATCH, keyword single/bulk
    update + delete.
  - **adyen** — 3DS `/payments/details` (fingerprint → authorised/refused),
    modification amount ceilings (422s), paymentLinks lifecycle.
  - **zuora** — payments applied to invoices (partial application, unapply
    with balance restoration), subscription-generated invoices, computed
    billing preview, cancellation_policy semantics.
  - **azure-devops** — pipelines + runs (queued → in_progress → completed),
    git pushes that actually store commits/refs, items served from stored
    state, work-item patch-document REST semantics, WIQL subset.
  - **helius** — parsed transactions (TRANSFER/SWAP, filters, cursor
    paging), sendTransaction → getSignatureStatuses linked
    (processed → confirmed → finalized), getTransaction/getBalance.
  - **cloudflare** — DNS record CRUD, a D1 query engine (SELECT subset,
    INSERT/UPDATE, CREATE TABLE, 400 on unknown SQL), store-backed
    firewall/page-rule CRUD, multipart worker deploys.
  Review fixed: zuora config-sink callouts + partial-unapply bookkeeping,
  D1 empty-select `_rid` leak, patch-document validation, crash-path 400s,
  confirmation-timestamp stability, error-code test pins.

## [0.31.0] — 2026-08-14

### Adapters

- **Async state machines across 18 adapters (derive-on-read).** Background
  tasks, checks, messages, and deploys no longer complete instantly or stall
  forever: docs store `_running_at`/`_done_at` at create time, every read
  derives the current state from the (injectable) clock — provider-real
  vocabularies, 1s to in-flight, 3s to terminal — persists the transition,
  and fires the provider's signed webhook exactly once per new state.
  Twilio messages `queued→sent→delivered` (with status-callback webhooks at
  each hop and the real `+15005550001` invalid-number magic trigger);
  SendGrid `processed→delivered/dropped`; Onfido `in_progress→complete`
  (clear/consider) and Jumio/Persona KYC lifecycles; Dune executions
  `PENDING→EXECUTING→COMPLETED/FAILED`; GitHub Actions runs
  `queued→in_progress→completed` with conclusions; ASC builds
  `PROCESSING→VALID/INVALID`; Cloudflare deploys and zones; Anaplan tasks;
  Chainlink Functions; ERC-4337 userOps; eth-jsonrpc receipts; WhatsApp/
  Instagram/Threads delivery states; Resend `queued→sent→delivered/bounced`.
  Failure injection: provider-real sandbox triggers where they exist, else a
  documented `simulate_fail` simulator flag. Poll routes carry
  `concurrency_key` so concurrent reads can't double-emit, and
  `TestTwilioStyleLifecycleEmitsOnce` pins the exactly-once guarantee.

## [0.30.0] — 2026-08-14

### Adapters

- **Real token validation (401 + expiry) across 34 adapters.** Protected
  routes resolve the presented credential against the adapter's token store
  and enforce expiry via the engine clock — the invalid/expired-token 401
  paths that auth-handling code exists to exercise are finally testable.
  Where minting didn't record expiry, tokens now store `expires_at` at the
  provider's real TTL (Google 3599, GitHub installation 3600, LinkedIn 60d,
  Discord 7d, …). Static tokens used by existing tests are seeded insert-once
  (runtime-computed far-future expiry, race-safe get-then-insert), so
  validation enforces without breaking them; OAuth-minted flows keep working
  end-to-end; google and entra-id already validated and gained only the
  expiry half. ~20 adapter tests gained a bogus-token → 401 negative case.
  apple-music registers its developer JWT in a token registry (forged
  ES256-header JWTs now 401); photos/youtube enforce the advertised 3599 TTL;
  instagram/threads 401s carry Meta's `type: OAuthException` + `fbtrace_id`.

## [0.29.0] — 2026-08-14

### Adapters

- **Per-provider signed webhook delivery across 17 adapters.** Every adapter
  now emits its provider's real webhook events at the right state changes,
  registered through the provider's real endpoint, signed with the provider's
  real scheme where one exists: resend (Svix `svix-*` headers), sendgrid
  (ECDSA P-256 over ts+body, public key documented), slack
  (`X-Slack-Signature` `v0=` + `url_verification` handshake), github
  (`X-Hub-Signature-256` with the **per-hook** secret, no longer a global
  constant), braintree (`bt_signature`/`bt_payload` + `bt-hash` HMAC-SHA1,
  signed `check` on registration), adyen (`additionalData.hmacSignature`
  base64 HMAC-SHA256 over the escaped-field signing string), zuora, printify
  (`X-Potify-Signature`), printful (`X-Pful-Signature`), zendesk (base64
  HMAC over ts+body). microsoft-graph implements the validationToken +
  clientState subscription model. Adapters whose providers sign nothing
  (braze, revenuecat, paypal, jira, azure-devops, helius) emit real-shaped
  envelopes unsigned-by-design with the rationale documented — nothing
  invented. Deliveries gate on per-hook event/topic subscription, and secret
  selection matches the hook the emitter is actually targeting (re-registration
  never signs with a stale key).

## [0.28.0] — 2026-08-14

### Adapters

- **Real list-filter/query params across 37 adapters.** The sweep's largest
  gap class — filter params accepted but silently ignored, returning the
  unfiltered superset — is closed: ~210 documented provider params (status
  and date windows, `q`/`$filter`/`sysparm_query`/selector bodies, `fields`
  projections, cursors) now filter, sort, and project via the `query_select`
  builtin before paging, envelope unchanged. Highlights: gmail
  (`q`/`labelIds`/`includeSpamTrash` default-false), gcalendar (overlap-window
  `timeMin`/`timeMax` on end/start respectively, `showDeleted`, `orderBy`
  with the real `singleEvents` 400), ga4 (body-driven
  `dimensionFilter`/`metricFilter` trees, `orderBys`, validation 400s),
  netsuite (the documented operator vocabulary incl. `ANY_OF`/`BETWEEN`/date
  ops and OR groups), servicenow (case-insensitive `sysparm_query` with
  anchored operator parsing and 400 on invalid queries), stripe (10 params
  plus newest-first default ordering and `parameter_invalid_integer` 400s),
  apple-searchads (OR-semantics multi-value selectors, reports conditions),
  psd2 (required `bookingStatus`/`dateFrom` 400s per Berlin Group), zuora
  (case-insensitive `filter[]`), powerplatform (`$filter`/`$orderby` with
  pre-`$skip` `@odata.count`), google-admin (14 params), github (13 params),
  thegraph (`where:` suffix operators incl. corrected `_in`/`_not_in`), and
  22 more. Stripe payouts are now scoped to the `Stripe-Account` header with
  the internal key stripped from responses and webhooks. hn-style unchanged
  (the real API takes no query params).

## [0.27.0] — 2026-08-14

### Added

- **`query_select(items, filter?, order_by?, order_dir?, limit?, offset?, fields?)` builtin** —
  the semantic core behind every provider's list-filter params: filter
  (list of `[field, op, value]` triples, AND'ed; ops `= != > >= < <= contains
  startswith endswith in like`, dotted field paths) → stable sort →
  offset/limit slice → field projection, in one call. Numbers compare
  numerically, ISO-8601 strings chronologically. Each adapter translates its
  provider's query syntax (`$filter`, SOQL `WHERE`, `q=`, `sysparm_query`, …)
  into triples — closing the sweep's largest gap class (filter params silently
  ignored, ~40 adapters). First wired in `shopify-style` (orders:
  status/financial_status/fulfillment_status/since_id/ids/created_at_min/max,
  `fields` projection) and `twilio-style` (messages: `To`/`From`/`DateSent`
  windows excluding queued messages).

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
