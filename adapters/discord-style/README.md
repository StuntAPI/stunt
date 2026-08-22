# Discord-style adapter

A stunt adapter for simulating a **Discord-style REST + OAuth2 API** (v10)
locally. All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Discord. "Discord" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of Discord's bot REST + OAuth2 surface, designed to
unblock chat-routing integrations (e.g. a bot that mirrors messages across channels)
during local development:

- **OAuth2:** authorize redirect, token exchange (auth code), refresh-token grant
  (issues new access token, no new refresh — matching Discord).
- **Application/user resolution:** `GET /oauth2/@me` (Bearer).
- **Bot user:** `GET /users/@me` → `{id, username, bot:true}`.
- **Guild + channels:** `GET /guilds/{id}`, `GET /guilds/{id}/channels`.
- **Send message:** `POST /channels/{id}/messages` (JSON `{content, embeds?, tts?}`).
- **List messages:** `GET /channels/{id}/messages?limit=N&after=<cursor>` (bare
  array, newest first, cursor-paginated).
- **Reactions:** `PUT /channels/{id}/messages/{msg}/reactions/{emoji}/@me` → 204
  (the real API's method; `POST` also accepted).
- **Interactions webhook:** `POST /interactions` — Ed25519-verified slash-command
  endpoint (ping → type 1 PONG, otherwise a type 5 deferred ack; bad/missing
  signature → 401).
- **WebSocket Gateway:** `ws /gateway` — HELLO (op 10) → IDENTIFY (op 2) →
  READY (op 0) → a synthetic `MESSAGE_CREATE` dispatch.

Messages are **stateful**: a message sent via POST appears in the GET list for the
same channel, enabling customer-chat round-trip testing locally.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/oauth2/authorize` | `oauth.star#on_authorize` | 302 redirect with code + state |
| POST | `/oauth2/token` | `oauth.star#on_token` | Token exchange (auth code + refresh) |
| GET | `/oauth2/@me` | `oauth.star#on_oauth_me` | Current app/user (Bearer) |
| GET | `/users/@me` | `bot.star#on_bot_user` | Bot user (Bot/Bearer) |
| GET | `/guilds/{guild_id}` | `bot.star#on_guild` | Guild object |
| GET | `/guilds/{guild_id}/channels` | `bot.star#on_guild_channels` | Channel list |
| POST | `/channels/{channel_id}/messages` | `messages.star#on_send_message` | Send a message |
| GET | `/channels/{channel_id}/messages` | `messages.star#on_list_messages` | List messages (stateful, paginated) |
| PUT (POST also accepted) | `/channels/.../reactions/{emoji}/@me` | `messages.star#on_react` | Add reaction → 204 |
| POST | `/interactions` | `interactions.star#on_interactions` | Signed slash-command webhook (Ed25519) |
| WS | `/gateway` | `gateway.star#on_connect` | Gateway: HELLO → IDENTIFY → READY → dispatch |

Any unmatched route returns `404`.

## Pagination

Bare-array list endpoints support **cursor pagination** via the `limit` and
`after` query params (`after` is the opaque cursor token from a prior call):

- `GET /channels/{id}/messages` — pages newest-first with a default page size of
  50 (matching Discord's default).
- `GET /guilds/{id}/channels` — paging is opt-in: a missing or zero `limit`
  returns the whole list.

When a further page exists, the response carries a Discord-style
`Link: <https://discord.com/api/v10<path>?after=<cursor>>; rel="next"` header;
round-trip the `after` token from that URL as a query param.

## Signed interactions & webhook deliveries

Both directions use Discord's Ed25519 signature scheme — the signature is
`ed25519(timestamp + raw_body)` in hex, carried in the
`X-Signature-Ed25519` + `X-Signature-Timestamp` headers:

- **Inbound** `POST /interactions` is verified against the adapter's public key
  (`_ED25519_PUBLIC_KEY` in `scripts/lib.star`) before being acknowledged; a
  missing or invalid signature returns `401`.
- **Outbound**, `POST /channels/{id}/messages` emits an Ed25519-signed
  `MESSAGE_CREATE` event (fire-and-forget) to any registered webhook receiver,
  signed with the adapter's private key over the same `timestamp + body`
  formula. Verify deliveries against the same public key.

See the signed-webhook roster in [`../README.md`](../README.md) for the
adapter-by-adapter list of schemes, headers, and mock keys.

## WebSocket Gateway

Connect to `ws /gateway` to exercise a gateway client (e.g. discordgo's
websocket session) locally. The lifecycle is:

1. Server sends **HELLO** (op 10) with `heartbeat_interval: 4500`.
2. Client sends **IDENTIFY** (op 2) — validated leniently (any frame accepted).
3. Server sends **READY** (op 0, `t: READY`) with the mock bot user,
   `session_id: "stunt-session-1"`, and a `resume_gateway_url`.
4. Server dispatches a synthetic **MESSAGE_CREATE** (op 0, `s: 1`) so the client
   immediately receives an event.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `oauth_codes` | Single-use OAuth authorization codes |
| `access_tokens` | OAuth access token → user binding |
| `refresh_tokens` | Refresh token → user binding (reusable) |
| `guilds` | Seeded guild data |
| `channels` | Seeded channel data |
| `messages` | Stateful message records (per channel) |

KV is used for monotonic sequence counters (`user_seq`, `code_seq`,
`message_seq`, etc.) and a `seeded` flag.

## Auth

- **Bot REST endpoints** require a VALID `Authorization: Bot <token>` (or
  `Bearer`) credential: the token must exist in the `access_tokens`
  collection and not be past its `expires_at`. A missing, unknown, or
  expired token returns `401 {"code": 0, "message": "401: Unauthorized"}`.
  The well-known static mock bot token `mock-bot-token` is seeded into the
  store on first use (1-year expiry) so existing local clients keep working.
- **OAuth2 endpoints** (`/oauth2/@me`) validate the Bearer token against the
  `access_tokens` collection and enforce the same expiry; tokens minted by
  `POST /oauth2/token` carry a 7-day `expires_at` (matching `expires_in`).

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  discord:
    adapter: ./adapters/discord-style
```

Then `stunt up` and make requests to the served address. Point your Discord
client (e.g. discordgo) at the stunt server address as the API base URL.
