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
- **Send message:** `POST /channels/{id}/messages` (JSON `{content, embeds?}`).
- **List messages:** `GET /channels/{id}/messages?limit=N` (bare array).
- **Reactions:** `POST /channels/{id}/messages/{msg}/reactions/{emoji}/@me` → 204.

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
| GET | `/channels/{channel_id}/messages` | `messages.star#on_list_messages` | List messages (stateful) |
| POST | `/channels/.../reactions/{emoji}/@me` | `messages.star#on_react` | Add reaction → 204 |

Any unmatched route returns `404`.

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

- **Bot REST endpoints** accept any `Authorization: Bot <token>` or
  `Authorization: Bearer <token>` header (the token value is not validated —
  only presence is checked). A missing header returns `401`.
- **OAuth2 endpoints** (`/oauth2/@me`) validate the Bearer token against the
  `access_tokens` collection.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  discord:
    adapter: ./adapters/discord-style
```

Then `stunt up` and make requests to the served address. Point your Discord
client (e.g. discordgo) at the stunt server address as the API base URL.
