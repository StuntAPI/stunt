# Security policy & threat model

`stunt` is a **local** API simulator: you run it on your own machine to stand in
for real APIs during development and testing. This document explains the trust
model, what is and isn't in scope, and how to report issues.

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security problems. Instead,
email **alex@deblasis.net** with details and a repro. We will acknowledge within
72 hours and aim to ship a fix promptly. Responsible disclosure is appreciated
and credited.

## The core trust property

> **A community adapter is safe to install: running an adapter you didn't write
> must never let it read your files, touch the network, or execute commands.**

This is stunt's defining difference from "just run someone's mock code." It is
enforced by a layered design:

1. **The wall — sandboxed Starlark.** All adapter *logic* (HTTP/gRPC/WS/GraphQL
   handlers) runs inside a [go.starlark.net](https://github.com/google/starlark-go)
   VM that has **no host I/O**:
   - `load()` is disabled (no importing other files).
   - There is no `open`/`file`/`socket`/`subprocess`/`exec` builtin.
   - Globals are frozen after a script loads; each call gets a fresh thread.
   - **Execution is bounded**: a step limit (`SetMaxExecutionSteps`) caps *both*
     handler calls and **script load** (so a top-level `while True: pass` can't
     hang the engine at load time; `while` loops are enabled but bounded).
2. **The gate — mediated file reads.** Adapter-declared file references (rules
   `body.file`, collection `seed`, `graphql.schema`, `grpc.descriptor`, handler
   scripts) are read by stunt **after** a containment check
   (`internal/pathutil.ContainedPath`) that resolves the path against the
   adapter directory and **rejects `..` escapes and absolute paths that leave
   it**. An adapter cannot read `~/.ssh/id_rsa`, `.env`, or anything outside its
   own directory. (See the audit matrix in `internal/rules/file_read_audit_test.go`.)
3. **The check — synthetic-data lint.** `stunt adapter lint` scans adapter
   fixtures/templates/handlers for real-looking data (emails, tokens, card
   numbers, PII fields) so installing an adapter never ships someone's real PII
   or secrets. First-party adapters must pass clean.

State a handler *can* touch flows only through controlled **primitives**
(Collection / KV / Blob / Identity / Events / Clock / Generator / Validator),
which live under `.stunt/state/` — adapter-mediated, never raw host access.

## What is NOT protected / out of scope

- **`stunt` itself is trusted.** The model protects you from *adapter* authors,
  not from stunt's own binary or your own manifest. If you write a manifest that
  points a service at an adapter *you* control, you're running your own code.
- **The TLS proxy / CA / `/etc/hosts` / trust store are privileged, opt-in
  surfaces** (`stunt setup`, `stunt trust`, `stunt hosts`, the `:443` listener).
  These modify your system by design and require privilege; they are NOT part of
  the adapter sandbox. `stunt doctor` reports their state. CA private keys are
  written `0600`; `/etc/hosts` entries are newline/host-injection-guarded;
  trust-store commands are shell-quoted.
- **Localhost-bound only.** stunt binds to loopback. It is not a hardened
  internet-facing server; do not expose it remotely.
- **No authentication/authorization on the simulator itself.** Anyone who can
  reach the loopback port can call it. Acceptable for local dev; do not run it
  where untrusted callers can reach it.

## Per-protocol DoS bounds

stunt applies resource limits so a single request/connection can't exhaust it:

| Transport | Bound |
|---|---|
| **Starlark (all)** | execution-step limit on calls **and** load; recursion disabled |
| **gRPC** | explicit `MaxRecvMsgSize` (4 MiB) |
| **WebSocket** | per-service concurrent-connection cap (503 when full); 32 KiB frame limit (library default); blocking `recv` accrues no steps (idle conns don't burn CPU) |
| **GraphQL** | max query depth (10), max field count (1000), per-query timeout; limits cannot be bypassed via fragments or aliases |

## Adapter distribution (`stunt adapter add git:…`)

Fetching an adapter over git is itself a privileged action you initiate.
`internal/adapterdist` runs git with an **explicit argv** (no shell), separates
the URL from options with `--`, **validates refs** (rejects shell-meta and
non-ref strings), and clones into a cache pinned by `@<ref>`. The *content* of a
cloned adapter is still subject to the sandbox/gate/check above once served.

## Hardened surfaces (selected)

- Path containment: `internal/pathutil` (single source of truth, used by rules,
  seeds, schema, descriptor, handler scripts).
- Git: explicit argv + ref validation + `--` (`internal/adapterdist`).
- TLS: ECDSA P-256 CA, per-SNI leaf minting, TLS 1.2+ minimum, key files `0600`.
- Value conversion: the Go↔Starlark boundary **errors** on unsupported types and
  integer overflow instead of silently corrupting (so a buggy/malicious adapter
  can't smuggle surprising values through).
- Panic recovery on every handler call path (HTTP/Starlark/gRPC/WS/GraphQL) — a
  faulty handler yields an error, never a crash.

## Adapter auth/header handling

These are the implicit security-relevant design decisions around how adapter
handlers interact with request headers, OAuth, and outbound network calls:

- **Header pass-through is intentional.** Handlers receive ALL request headers
  (including `Authorization`) via `req["headers"]` so adapters can read Bearer
  tokens, Basic credentials, `User-Agent`, etc. **Implication:** any installed
  adapter can read auth headers from requests it handles. The adapter is
  sandboxed (no host I/O, no network) and cannot exfiltrate them, but it does
  *see* the inbound request's credentials. This is by design — OAuth-aware
  adapters (LinkedIn, Threads, Reddit, X Articles) need to validate tokens.

- **PKCE S256 is relaxed in the x-articles-style adapter** (documented in the
  script's module docstring). The real X server verifies `code_verifier` by
  computing `base64url_no_pad(sha256(code_verifier))` and comparing against the
  stored `code_challenge`. Starlark in stunt has no `sha256` or `base64`
  builtins, so the mock checks `code_verifier` for *presence* (non-empty) but
  does **not** verify the cryptographic match. This is acceptable for a
  localhost pipeline double: a real client that generates a valid S256 pair
  always passes, and a client that omits the verifier fails appropriately. The
  only gap is that a deliberately-wrong-but-present verifier is accepted.

- **`events_register` / `events_emit` is an SSRF surface** adapters *can* use.
  An adapter can call `events_register("http://...")` to register an arbitrary
  webhook URL, then `events_emit(type, payload)` to deliver a POST to it. This
  is fire-and-forget: delivery failures never break the handler. The four new
  adapters (linkedin-style, threads-style, reddit-style, x-articles-style) do
  **not** use events at all. But it is a known capability — any installed
  adapter could register a webhook and emit to localhost services. The events
  emitter is subject to a 10-second timeout per `events_emit` call.

## Disclosure / responsible use

Branded adapters use `<provider>-style` naming and ship a `DISCLAIMER` stating
they are not affiliated with the provider and return synthetic data only (see
`adapters/DISCLAIMER.template`). Reproduce API *structure* for local testing;
never ship real provider data or documentation.
