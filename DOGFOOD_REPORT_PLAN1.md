# Dogfood Report — stunt "Plan 1" Observability (dashboard + request capture)

**Date:** 2026-07-23
**Tester:** pi (subagent)
**Binary:** `env -u GOROOT go build -o /tmp/df2 ./cmd/stunt` (clean build, exit 0)
**Manifest:** 3 embedded adapters (`embedded:stripe-style`, `embedded:github-style`, `embedded:slack-style`), port mode, `base_port: 9000`.
**Scenario:** `stunt up` → sent 8 requests (3 with `Authorization: Bearer sk_df_SECRET_xyz`) → exercised dashboard API, HTML page, and CLI → killed server.

---

## Bugs

**None found that block core functionality.** A few minor observations (not strictly bugs):

1. **`duration_ms` is almost always `0` for fast local handlers.** Sub-millisecond timings floor to 0 ms, so the latency column is uninformative for a local sim (only the deliberately-slow 100 KB POST showed `1`). Not wrong, just low-resolution. *Repro:* any fast GET shows `duration_ms: 0`. Suggest microsecond resolution or sub-ms rounding in Plan 2.
2. **`seq` resets per-service but `id` is global** — this is fine/intentional, but the HTML page shows the per-service `seq` (e.g. two rows both labelled `#2`), which reads slightly oddly next to the global ordering. Cosmetic only. *Repro:* `GET /` shows duplicate `#` values across services.
3. **HTML page is server-rendered, no live refresh.** Expected for Plan 1 (Plan 2 = live feed), noting it so it's not seen as a regression later.

---

## Security ✅

- **Secret did NOT leak.** Grepped the full `/api/requests` capture for the literal `sk_df_SECRET_xyz` → **0 occurrences**. The `Authorization: Bearer ...` header is consistently stored as `"Authorization":["[REDACTED]"]`. Other headers (`Content-Type`, `User-Agent`, `Content-Length`) are preserved verbatim, so redaction is correctly scoped to sensitive headers only.
- **Token guard holds:** `/api/requests` and `/` return **401 `unauthorized`** with no token and with a wrong token.
- **Host (loopback) guard holds:** both `/api/requests` and `/` return **403 `forbidden host`** when a foreign `Host: evil.example.com` / `Host: attacker.io` header is sent (even with a valid token). Confirmed on both the JSON endpoint and the HTML page.
- Token is printed once on stdout at boot (`token: <32-hex>`), not to stderr, not logged per-request. Good.

---

## Capture accuracy ✅

- **All 8 requests captured** (methods, paths, services, statuses all correct).
- **Service attribution correct:** each request tagged with the right service name (`stripe` / `github` / `slack`) — not just the port.
- **Statuses accurate:** 401s, a 404, all faithfully recorded.
- **Request & response bodies captured** (e.g. `amount=1500&currency=usd`, `{"title":"bug here"}`, and the JSON error bodies).
- **Redaction sane** (see Security).
- **Truncation sane:** a 100 KB POST body was captured at **65,549 bytes** with a clear trailing marker `…[truncated]`. 64 KB cap confirmed working. The marker is human/agent legible and unambiguous.
- Minor: `req_headers` / `resp_headers` are stored as a **JSON string-of-JSON** (e.g. `"{\"Accept\":[\"*/*\"]}"`) rather than a nested object. This is stable but means consumers must double-decode. See UX(LLM).

---

## UX (human)

- **`stunt up` banner is excellent** — dashboard URL + token printed in one clean line: `dashboard:  http://127.0.0.1:58549   (token: b5a8...)`. Copy-paste friendly.
- **The `/` HTML page reads very well.** Single server-rendered `<table>`, dark theme, monospace, columns `# / method / path / status / svc / ms`. No JS, no build step, loads instantly. Slightly bare (no row click → detail, no filtering in-page) but perfectly serviceable as a Plan 1 "glance at history" view.
- **`--url` / `--token` friction is real and annoying.** ⚠️ This is the biggest human (and LLM) UX wart. There is **no instance auto-discovery** — `stunt requests` with no flags errors with:
  > `no dashboard URL: run `stunt up` first, or pass --url and --token`
  
  So to use the CLI I had to manually copy the URL+token out of the `up` banner into two flags. The default `--url` ("read from the running server" per `--help`) does **not** actually work in practice — every real invocation needs both flags hand-filled. This is the #1 thing to fix for usability.
- The CLI table output is clean and aligned (seq, method, status, path).
- `stunt down` only works for service-managed instances; a foreground `stunt up` must be Ctrl-C'd. Fine, but worth a one-line hint in the banner.

---

## UX (LLM) ✅ (with one caveat)

- **`stunt requests --json` is clean and stable.** Output is a JSON array; **keys are alphabetically sorted** (deterministic), so a consuming agent can rely on shape. Fields: `duration_ms, id, method, path, req_body, req_headers, resp_body, resp_headers, seq, service, status, transport, ts`.
- `--limit` is respected. Filtering by `service`/`method`/`path`/`q` is available via the HTTP API (curl is trivial for an agent).
- **Caveat for agents:** `req_headers`/`resp_headers` are **JSON-encoded strings**, not objects — an agent parsing with `json.loads` gets a `str`, not a `dict`, and must `json.loads` *again*. Stable, but a small footgun for naive parsers. Consider emitting them as nested objects in the JSON form (or document the double-decode).
- Timestamps include timezone offset (`+03:00`) — good for locality, slightly unusual vs UTC; fine.

---

## What delighted

- **Zero-friction boot.** `embedded:<adapter>` just works — no clone, no network, three real adapters mounted in <2 s. The `embedded:` prefix is a great DX touch.
- **Redaction "just worked"** with no config — sent a real-looking bearer secret, it never appeared anywhere. Trustworthy by default.
- **The Host guard + token guard layering** is exactly right defense-in-depth: token stops unauth'd reads, Host stops DNS-rebinding/exfil via a foreign origin. Both enforced on JSON *and* HTML.
- **Truncation marker `…[truncated]`** is the right call — visible, not silent data loss.
- **Single static binary, instant HTML dashboard, no JS deps** — the whole observability story is self-contained.

---

## Top 3 priorities

### Plan 2 (live feed + replay)
1. **Instance auto-discovery for `stunt requests`.** Write the running dashboard URL+token to a well-known file (e.g. `~/.stunt/run/<pid>.json` or a port-file next to the manifest) so `stunt requests` works with **zero flags**, reading from the most recent local `up`. This kills the biggest UX wart above and benefits humans *and* agents.
2. **Live/SSE feed on the dashboard** (`GET /api/requests/stream` or WebSocket) so the `/` page and CLI `--watch` update in real time instead of requiring manual refresh. Pair with a `--follow` flag on `stunt requests`.
3. **Replay primitive** — `stunt requests replay <id>` (and a "replay" button on the HTML row) that re-issues a captured request against the same service. Massive for debugging flaky/stateful flows. (Also: resolve the `duration_ms: 0` resolution issue and add sub-ms / μs timing while touching capture.)

### Plan 3 (data browsers + reset/snapshot)
- **Service-aware data browsers** (`GET /api/data/<service>`) that expose the adapter's key-value store contents (charges, issues, channels) as inspectable/resettable tables — turns the dashboard from "request log" into "state inspector."
- **`stunt snapshot` / `stunt reset`** to save/restore the full sim state (all stores + capture) to a file, enabling reproducible bug reports and CI fixtures.
- **Per-request detail view** on the HTML page (click a row → expand req/resp headers+bodies with the redaction/truncation markers visible), replacing the current flat table.

---

## Repro artifacts

- Manifest: `/tmp/df2-run/m.yaml` (cleaned up after run)
- Binary & run dir: removed (`/tmp/df2*` deleted)
- Server: **confirmed killed** (no `df2` processes, dashboard port closed)

*Budget note: single efficient pass completed within budget; server killed before report writing. No exhaustive negative-path testing performed (e.g. didn't probe concurrency/race on capture, didn't test gRPC/WebSocket capture).*
