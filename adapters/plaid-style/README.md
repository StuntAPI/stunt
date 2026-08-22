# Plaid-style adapter

A stunt adapter for simulating a **Plaid API** (2020-09-14) locally. All data is
synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Plaid. "Plaid" and related marks are trademarks of their
> respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is
> for **local development and testing only**.

## What it simulates

A faithful behavioral mock of the Plaid banking-data API surface, designed to
unblock financial-data integrations during local development:

- **Link token:** `POST /link/token/create` → `{link_token, expiration, request_id}`.
  The link_token identifies a Link *session*; public tokens minted from it are
  bound to it.
- **Item exchange:** `POST /item/public_token/exchange` → `{access_token, item_id}`
  (STATEFUL: creates an item with accounts). Accepts an optional `link_token`:
  when present it must be the session that issued the `public_token`, else
  400 `INVALID_PRODUCT`/`INVALID_FIELD`.
- **Balances:** `POST /accounts/balance/get` → `{accounts:[{account_id, balances, name, subtype}]}` (honors `options.account_ids`).
- **Transactions sync:** `POST /transactions/sync` → cursor-based sync
  (`{added, modified, removed, next_cursor}`). Honors `count` (1-500, default 100,
  applied to the combined update set). STATEFUL: after the initial pull,
  `POST /sandbox/item/fire_webhook` (webhook_code `SYNC_UPDATES_AVAILABLE`)
  mutates the item's transactions — one is posted (surfaces in `modified`),
  one is tombstoned (surfaces in `removed`) — and the next sync returns them
  with correct cursor math.
- **Identity:** `POST /identity/get` → `{accounts:[{owners:[{names, emails, phone_numbers}]}]}` (honors `options.account_ids`).
- **Accounts list:** `POST /accounts/get` (honors `options.account_ids`).
- **Item management:** `POST /item/get`, `POST /item/remove` (unknown
  access_token → 400 `INVALID_ACCESS_TOKEN`; a valid token purges the item's
  accounts/transactions and always returns `removed: true`, matching Plaid).
- **Institutions:** `POST /institutions/get` (count/offset paging, filtered by
  `country_codes` + `products`), `POST /institutions/get_by_id`.
- **Sandbox:** `POST /sandbox/public_token/create` (mints a public_token for an
  institution — creating a fresh item with accounts and transactions —
  optionally bound to a Link session; one session can yield several items),
  `POST /sandbox/item/reset_login`, `POST /sandbox/item/fire_webhook`.

Items, accounts, and transactions are **stateful** — a seed item with two accounts
and three transactions is pre-loaded so `transactions/sync` returns data
immediately.

### Sync cursor model

Every transaction carries a per-item monotonic `seq` and a `state` of
`new`/`modified`/`removed`. The cursor (`cursor-N`) is a watermark: the client
has consumed every update with `seq <= N`. A sync returns the item's updates
above the watermark, oldest first, capped at `count`; `next_cursor` advances to
the last seq served, so a truncated batch resumes on the next call.

## Auth

Plaid takes credentials **in the request body** (`{client_id, secret}`), as the
`PLAID-CLIENT-ID` / `PLAID-SECRET` headers (the form every official SDK sends),
or as `Authorization: Bearer <client_id>_<secret>`. Presence is checked, not
validated.

## Webhooks

Plaid fires `TRANSACTIONS_INITIAL_UPDATE` and `SYNC_UPDATES_AVAILABLE` webhooks.
Plaid signs webhooks with an `X-Plaid-Signature` header (JWT-like). To receive
webhook events in your tests, register a webhook URL via the stunt events system
and listen for the `SYNC_UPDATES_AVAILABLE` event type.

The signature scheme: Plaid computes an HMAC over the webhook body and delivers
it in the `X-Plaid-Signature` header as a JWT-like token. Your webhook handler
should verify this signature against the Plaid client secret. **In this local
mock, signatures are documented but not cryptographically enforced** — the mock
emits events via `events_emit` so you can test your handler wiring.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/link/token/create` | `link.star#on_create_link_token` | Create link token |
| POST | `/item/public_token/exchange` | `item.star#on_exchange_public_token` | Exchange public → access token |
| POST | `/accounts/balance/get` | `accounts.star#on_get_balances` | Account balances (`options.account_ids` filter) |
| POST | `/accounts/get` | `accounts.star#on_get_accounts` | Account list (`options.account_ids` filter) |
| POST | `/transactions/sync` | `transactions.star#on_sync` | Cursor-based sync (`count` cap) |
| POST | `/identity/get` | `identity.star#on_get_identity` | Identity/owner info (`options.account_ids` filter) |
| POST | `/item/get` | `item.star#on_get_item` | Item details |
| POST | `/item/remove` | `item.star#on_remove_item` | Remove item |
| POST | `/institutions/get` | `institutions.star#on_list_institutions` | List institutions (count/offset, country/product filters) |
| POST | `/institutions/get_by_id` | `institutions.star#on_get_institution_by_id` | Institution by id |
| POST | `/sandbox/public_token/create` | `sandbox.star#on_create_public_token` | Mint public_token (+ new item) for an institution |
| POST | `/sandbox/item/reset_login` | `sandbox.star#on_reset_login` | Force ITEM_LOGIN_REQUIRED |
| POST | `/sandbox/item/fire_webhook` | `sandbox.star#on_fire_webhook` | Fire webhook / simulate transaction mutations |

## Backing stores

| Collection | Purpose |
|------------|---------|
| `items` | Item bindings (seeded) |
| `accounts` | Bank accounts (seeded) |
| `transactions` | Transactions (seeded) |
| `public_tokens` | Link public tokens (bound to item + Link session) |
| `access_tokens` | Access token → item bindings |
| `link_tokens` | Link tokens (Link sessions) for widget |
| `institutions` | Institutions catalog (seeded) |

## Usage

```yaml
services:
  plaid:
    adapter: ./adapters/plaid-style
```

Then `stunt up` and point your Plaid client at the served address.
