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
- **Item exchange:** `POST /item/public_token/exchange` → `{access_token, item_id}` (STATEFUL: creates an item with accounts).
- **Balances:** `POST /accounts/balance/get` → `{accounts:[{account_id, balances, name, subtype}]}`.
- **Transactions sync:** `POST /transactions/sync` → cursor-based pagination
  (`{added, modified, removed, next_cursor}`). STATEFUL: returns new transactions
  each sync; advance the cursor.
- **Identity:** `POST /identity/get` → `{accounts:[{owners:[{names, emails, phone_numbers}]}]}`.
- **Accounts list:** `POST /accounts/get`.
- **Item management:** `POST /item/get`, `POST /item/remove`.

Items, accounts, and transactions are **stateful** — a seed item with two accounts
and three transactions is pre-loaded so `transactions/sync` returns data
immediately.

## Auth

Plaid takes credentials **in the request body** (`{client_id, secret}`) or as
`Authorization: Bearer <client_id>_<secret>`. Presence is checked, not validated.

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
| POST | `/accounts/balance/get` | `accounts.star#on_get_balances` | Account balances |
| POST | `/accounts/get` | `accounts.star#on_get_accounts` | Account list |
| POST | `/transactions/sync` | `transactions.star#on_sync` | Cursor-based sync |
| POST | `/identity/get` | `identity.star#on_get_identity` | Identity/owner info |
| POST | `/item/get` | `item.star#on_get_item` | Item details |
| POST | `/item/remove` | `item.star#on_remove_item` | Remove item |

## Backing stores

| Collection | Purpose |
|------------|---------|
| `items` | Item bindings (seeded) |
| `accounts` | Bank accounts (seeded) |
| `transactions` | Transactions (seeded) |
| `public_tokens` | Link public tokens |
| `access_tokens` | Access token → item bindings |
| `link_tokens` | Link tokens for widget |

## Usage

```yaml
services:
  plaid:
    adapter: ./adapters/plaid-style
```

Then `stunt up` and point your Plaid client at the served address.
