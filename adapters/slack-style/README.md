# Slack-style adapter

A stunt adapter for simulating a **Slack Web API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Slack. "Slack" is a trademark of its respective owner.
> See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

A faithful behavioral mock of Slack's Web API messaging surface, designed for
local integration testing without a real Slack workspace:

- **auth.test:** `POST /api/auth.test` → `{ok:true, url, team, user, team_id, user_id}`.
- **Post message:** `POST /api/chat.postMessage` (`{channel, text}`) → `{ok:true, channel, ts, message:{...}}`.
- **Create channel:** `POST /api/conversations.create` (`{name}`).
- **List channels:** `POST /api/conversations.list` (form-encoded; `GET` with
  query params also accepted) → `{ok:true, channels:[...]}`.
- **Channel history:** `POST /api/conversations.history` (form field `channel`;
  `GET ?channel=C...` also accepted) → `{ok:true, messages:[...]}`.
- **Add reaction:** `POST /api/reactions.add` (`{channel, timestamp, name}`).
- **Events API config:** `POST /api/apps.events.url` (`{url, signing_secret?, events?}`) —
  sets the Request URL and immediately delivers the `url_verification`
  handshake (see [Webhooks](#webhooks)).

Messages are **stateful**: a message posted via `chat.postMessage` appears in
`conversations.history` for the same channel, enabling round-trip testing locally.

## Auth — Bearer token

Slack uses `Authorization: Bearer xoxb-...` for all Web API requests. This
adapter **validates** the Bearer token:

- Checks for `Authorization: Bearer <token>` header.
- Returns `401` with `{ok:false, error:"not_authed"}` if the header is missing.
- Accepts a token only when it is present in the adapter's token store (and
  not expired) **or** when it validates against the identity issuer.

### Seeded test token

The well-known test token `xoxb-test-token` is seeded automatically into the
token store on first request (with a far-future expiry — Slack bot tokens do
not expire until revoked), so existing scripts, curl commands, and tests
keep working:

```bash
curl -H "Authorization: Bearer xoxb-test-token" \
  -X POST http://localhost:PORT/api/chat.postMessage \
  -H "Content-Type: application/json" \
  -d '{"channel":"C00000001","text":"Hello from stunt!"}'
```

### 401 without auth / with an invalid token

```bash
curl -X POST http://localhost:PORT/api/auth.test
# → 401 {"ok":false,"error":"not_authed"}

curl -X POST http://localhost:PORT/api/auth.test \
  -H "Authorization: Bearer xoxb-unknown-bogus-token"
# → 401 {"ok":false,"error":"invalid_auth"}
```

Any random `xoxb-...` token that is neither the seeded test token nor a
valid issuer-minted token is rejected with `invalid_auth`.

## Webhooks

Real Slack has no Web API endpoint for configuring the Events API — the
Request URL and Signing Secret are set in the app dashboard. This adapter
exposes a local analog:

```bash
curl -X POST "http://localhost:PORT/api/apps.events.url" \
  -H "Authorization: Bearer xoxb-test-token" \
  -H "Content-Type: application/json" \
  -d '{"url": "http://localhost:9090/slack/events", "events": ["message"]}'
# → {ok:true, url: ..., events: [...]}
```

On registration the adapter immediately delivers Slack's
`url_verification` handshake to the URL (a POST with
`{type:"url_verification", token, challenge}` — echo the challenge with a 200,
or at least return 2xx). If a `signing_secret` is supplied it becomes the
per-app signing secret for all deliveries; otherwise the fixed synthetic
default `stunt_mock_slack_signing_secret_2026` is used.

### Events emitted

Every delivery is a signed Events API `event_callback` envelope (wrapped in
stunt's standard `{"type": ..., "payload": {...}}` delivery envelope):

| Inner event | Emitted when |
|-------------|--------------|
| `message` | `POST /api/chat.postMessage` |
| `channel_created` | `POST /api/conversations.create` |
| `reaction_added` | `POST /api/reactions.add` |
| `url_verification` | `POST /api/apps.events.url` (handshake, not an `event_callback`) |

The `events` list at registration subscribes to inner event types (an empty
list subscribes to everything).

### Signature scheme

Slack signs every Events API delivery with HMAC-SHA256 over
`"v0:" + timestamp + ":" + raw_body` and carries two headers:

- **`X-Slack-Signature`**: `v0=<hex(HMAC-SHA256(signing_secret, signed))>`
  (hex, not base64)
- **`X-Slack-Request-Timestamp`**: Unix epoch seconds (replay window)

This adapter **signs every delivery** with exactly this scheme. Verification
in Go:

```go
ts := r.Header.Get("X-Slack-Request-Timestamp")
mac := hmac.New(sha256.New, []byte(signingSecret))
mac.Write([]byte("v0:" + ts + ":" + string(rawBody)))
expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
if expected != r.Header.Get("X-Slack-Signature") { return 401 }
```

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/api/auth.test` | `auth.star#on_auth_test` | Authenticate and get workspace info |
| POST | `/api/chat.postMessage` | `chat.star#on_post_message` | Post a message (stateful) |
| POST | `/api/conversations.create` | `conversations.star#on_create_conversation` | Create a channel |
| any | `/api/conversations.list` | `conversations.star#on_list_conversations` | List all channels (SDK POST form or GET query) |
| any | `/api/conversations.history` | `conversations.star#on_conversation_history` | Channel message history (stateful; SDK POST form or GET query) |
| POST | `/api/reactions.add` | `reactions.star#on_add_reaction` | Add a reaction to a message |
| POST | `/api/apps.events.url` | `events.star#on_set_events_url` | Set the Events API Request URL (local analog of the dashboard setting) |

Any unmatched route returns `404 {"ok":false,"error":"not_found"}`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `channels` | Channel records (seeded with `#general`) |
| `messages` | Stateful message records (per channel) |
| `webhooks` | Registered Events API Request URLs (+ per-app signing secret) |

KV is used for monotonic sequence counters (`ts_seq`, `channel_seq`) and a
`seeded` flag.

## Shared library

Shared helpers (`_bearer`, `_require_auth`, `_ok`, `_err`, `_next_ts`, `_seed`)
are defined in `scripts/lib.star` and preloaded into every handler script via
stunt's `LoadWithLib` mechanism.

## Layout

```
adapter.yaml                    Manifest: endpoints, resources, rules, identity
DISCLAIMER                      Not affiliated / synthetic-only notice
README.md                       This file
scripts/
  lib.star                      Shared helpers (Bearer auth, timestamps, seed, webhook signing)
  auth.star                     auth.test endpoint
  chat.star                     chat.postMessage (stateful)
  conversations.star            conversations: create, list, history
  reactions.star                reactions.add
  events.star                   apps.events.url (Events API Request URL config)
```

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  slack:
    adapter: ./adapters/slack-style
```

Then `stunt up` and make requests to the served address.
