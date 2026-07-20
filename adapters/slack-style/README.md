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
- **List channels:** `GET /api/conversations.list` → `{ok:true, channels:[...]}`.
- **Channel history:** `GET /api/conversations.history?channel=C...` → `{ok:true, messages:[...]}`.
- **Add reaction:** `POST /api/reactions.add` (`{channel, timestamp, name}`).

Messages are **stateful**: a message posted via `chat.postMessage` appears in
`conversations.history` for the same channel, enabling round-trip testing locally.

## Auth — Bearer token

Slack uses `Authorization: Bearer xoxb-...` for all Web API requests. This
adapter **validates** the Bearer token:

- Checks for `Authorization: Bearer <token>` header.
- Returns `401` with `{ok:false, error:"not_authed"}` if the header is missing.

### Dev bypass (`xoxb-`)

For frictionless local testing, **any token starting with `xoxb-`** is
accepted **without** identity validation. This lets you use a well-known dev
token like `xoxb-test-token` in scripts, curl commands, and tests:

```bash
curl -H "Authorization: Bearer xoxb-test-token" \
  -X POST http://localhost:PORT/api/chat.postMessage \
  -H "Content-Type: application/json" \
  -d '{"channel":"C00000001","text":"Hello from stunt!"}'
```

### 401 without auth

```bash
curl -X POST http://localhost:PORT/api/auth.test
# → 401 {"ok":false,"error":"not_authed"}
```

## Webhooks

When a message is posted, the adapter emits a `message` webhook event
(fire-and-forget) to any registered webhook sink.

### Real Slack webhook signing (documented for reference)

The real Slack Events API signs webhook requests with two headers:

- **`X-Slack-Signature`**: `v0=<hex HMAC-SHA256>`, where the HMAC is computed
  over the string `v0:<timestamp>:<raw_body>` using the Signing Secret as the
  key. The output is hex-encoded (not base64).
- **`X-Slack-Request-Timestamp`**: Unix epoch seconds (used to prevent replay
  attacks).

Verification steps (on the receiver):
1. Concatenate `v0:<X-Slack-Request-Timestamp>:<raw request body>`.
2. Compute `HMAC-SHA256(key=signing_secret, msg=concatenated_string)`.
3. Compare the hex digest against the value in `X-Slack-Signature` (without
   the `v0=` prefix).

This stunt adapter validates **inbound** Bearer auth (above) and emits webhook
events via the standard stunt events envelope. The outbound signing scheme is
documented here for reference; it is **not** applied to the synthetic webhook
payload because the events primitive sends a fixed JSON envelope without
custom headers.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/api/auth.test` | `auth.star#on_auth_test` | Authenticate and get workspace info |
| POST | `/api/chat.postMessage` | `chat.star#on_post_message` | Post a message (stateful) |
| POST | `/api/conversations.create` | `conversations.star#on_create_conversation` | Create a channel |
| GET | `/api/conversations.list` | `conversations.star#on_list_conversations` | List all channels |
| GET | `/api/conversations.history` | `conversations.star#on_conversation_history` | Channel message history (stateful) |
| POST | `/api/reactions.add` | `reactions.star#on_add_reaction` | Add a reaction to a message |

Any unmatched route returns `404 {"ok":false,"error":"not_found"}`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `channels` | Channel records (seeded with `#general`) |
| `messages` | Stateful message records (per channel) |

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
  lib.star                      Shared helpers (Bearer auth, timestamps, seed)
  auth.star                     auth.test endpoint
  chat.star                     chat.postMessage (stateful)
  conversations.star            conversations: create, list, history
  reactions.star                reactions.add
```

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  slack:
    adapter: ./adapters/slack-style
```

Then `stunt up` and make requests to the served address.
