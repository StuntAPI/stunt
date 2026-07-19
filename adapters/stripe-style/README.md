# Stripe-style adapter

A stunt adapter for simulating a **Stripe-style payments API** locally,
including **Stripe Connect** (marketplace/platform flows: connected accounts,
onboarding links, transfers, payouts).
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Stripe. "Stripe" is a trademark of its respective owner.
> See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

A broader-than-minimal MVP of a Stripe-style payments API: charges (create,
retrieve, list, capture, refund), customers (CRUD), and account balance —
plus **Stripe Connect**: connected accounts (create/retrieve/update/list),
account links (onboarding URLs), transfers (create/retrieve/list/reverse),
and payouts (create/list).

State persists in an on-disk SQLite-backed collection store (under `.stunt/state/`),
so data you create in one request is visible in subsequent requests and survives
across `stunt up` restarts. Run `stunt clean` to reset state to the seed fixtures.

Webhook events are emitted on charge and Connect lifecycle transitions to a
configurable webhook sink.

## Auth

All endpoints (except `/v1/tokens`) require a valid `Authorization: Bearer
<token>` header.

### Token validation

The adapter validates bearer tokens via the identity primitive
(`identity_validate`). If the token is missing or invalid, the adapter
returns `401` with a JSON body:

```json
{"error": {"type": "authentication_error", "message": "..."}}
```

### Dev bypass (`sk_test`)

For frictionless local testing, **any token starting with `sk_test`** is
accepted **without** `identity_validate`. This lets you use a well-known dev
token like `sk_test_local` in scripts, curl commands, and tests without
needing to mint a real token first:

```bash
curl -H "Authorization: Bearer sk_test_local" http://localhost:PORT/v1/charges
```

This bypass exists **only** in the local simulator and never touches a real
API.

### Minting a real token

For integration tests that need a real (validated) token, `POST /v1/tokens`
mints one via the identity issuer:

```bash
curl -X POST http://localhost:PORT/v1/tokens
# → {"token": "eyJhbGciOiJIUzI1NiIs..."}
```

The returned token can then be used as a Bearer token for subsequent
requests. Optional body fields `subject` and `scopes` customise the claims
(defaults: `subject="test_user"`, `scopes=["write"]`).

## Webhooks

The adapter emits webhook events on lifecycle transitions. Events are
**fire-and-forget**: if no webhook sink is configured or the delivery fails,
the operation still succeeds.

| Trigger | Event type |
|---------|-----------|
| `POST /v1/charges` (create) | `charge.created` |
| `POST /v1/charges/{id}/capture` | `charge.updated` |
| `POST /v1/charges/{id}/refund` | `charge.refunded` |
| `POST /v1/accounts` (create) | `account.updated` |
| `POST /v1/accounts/{id}` (update) | `account.updated` |
| `POST /v1/transfers` (create) | `transfer.created` |
| `POST /v1/transfers/{id}/reversals` | `transfer.reversed` |
| `POST /v1/payouts` (create) | `payout.created` |

### Configuring the webhook sink

Set `config.webhook_url` in the service definition to receive events:

```yaml
services:
  stripe:
    adapter: ./adapters/stripe-style
    config:
      webhook_url: http://localhost:9090/webhook
```

The webhook body is a JSON envelope:

```json
{
  "type": "charge.created",
  "payload": { "id": "ch_1", "amount": 5000, "currency": "usd", "status": "pending", ... }
}
```

## Stripe Connect

Stripe Connect is the marketplace/platform surface. It lets a platform create
connected accounts (sellers/service providers), onboard them via hosted forms,
transfer funds to them, and let them pay out to their bank.

### Connected accounts

Create a Custom/Express/Standard connected account:

```bash
curl -X POST http://localhost:PORT/v1/accounts \
  -H "Authorization: Bearer sk_test_local" \
  -H "Content-Type: application/json" \
  -d '{"type":"express","country":"US","email":"seller@example.com"}'
# → {"id":"acct_1","object":"account","type":"express",...}
```

Update capabilities (triggers `charges_enabled`/`payouts_enabled` flags and
emits `account.updated`):

```bash
curl -X POST http://localhost:PORT/v1/accounts/acct_1 \
  -H "Authorization: Bearer sk_test_local" \
  -H "Content-Type: application/json" \
  -d '{"capabilities":{"card_payments":"active","transfers":"active"}}'
```

### Account links (onboarding)

Generate a synthetic onboarding URL for a connected account:

```bash
curl -X POST http://localhost:PORT/v1/account_links \
  -H "Authorization: Bearer sk_test_local" \
  -H "Content-Type: application/json" \
  -d '{"account":"acct_1","refresh_url":"https://app.example.com/refresh","return_url":"https://app.example.com/return","type":"account_onboarding"}'
# → {"object":"account_link","url":"https://onboarding.stunt.local/acct_1/1","expires_at":1700003600}
```

### Transfers (platform → connected account)

Move funds from the platform balance to a connected account:

```bash
curl -X POST http://localhost:PORT/v1/transfers \
  -H "Authorization: Bearer sk_test_local" \
  -H "Content-Type: application/json" \
  -d '{"amount":15000,"currency":"usd","destination":"acct_1"}'
# → {"id":"tr_1","object":"transfer","amount":15000,...}
```

The destination account's balance increases by the transfer amount (tracked in KV).

### Per-account balance

Use the `Stripe-Account` header to scope `/v1/balance` to a connected account:

```bash
curl http://localhost:PORT/v1/balance \
  -H "Authorization: Bearer sk_test_local" \
  -H "Stripe-Account: acct_1"
# → {"object":"balance","available":[{"amount":15000,"currency":"usd"}],...}
```

Without the header, the platform balance is returned (synthetic defaults).

### Payouts (connected account → bank)

Create a payout from a connected account's balance:

```bash
curl -X POST http://localhost:PORT/v1/payouts \
  -H "Authorization: Bearer sk_test_local" \
  -H "Stripe-Account: acct_1" \
  -H "Content-Type: application/json" \
  -d '{"amount":5000,"currency":"usd","method":"standard"}'
# → {"id":"po_1","object":"payout","amount":5000,"status":"pending",...}
```

The connected account's balance is debited by the payout amount.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/v1/tokens` | `tokens.star#on_mint_token` | Mint a test token (no auth required) |
| POST | `/v1/charges` | `charges.star#on_create_charge` | Create a charge (status → `pending`) |
| GET | `/v1/charges/{id}` | `charges.star#on_retrieve_charge` | Retrieve a charge |
| GET | `/v1/charges` | `charges.star#on_list_charges` | List all charges |
| POST | `/v1/charges/{id}/capture` | `charges.star#on_capture_charge` | Capture a charge (→ `succeeded`) |
| POST | `/v1/charges/{id}/refund` | `charges.star#on_refund_charge` | Refund a charge (→ `refunded`) |
| POST | `/v1/customers` | `customers.star#on_create_customer` | Create a customer |
| GET | `/v1/customers/{id}` | `customers.star#on_retrieve_customer` | Retrieve a customer |
| GET | `/v1/customers` | `customers.star#on_list_customers` | List all customers |
| POST | `/v1/customers/{id}` | `customers.star#on_update_customer` | Update a customer |
| DELETE | `/v1/customers/{id}` | `customers.star#on_delete_customer` | Delete a customer |
| GET | `/v1/balance` | `balance.star#on_get_balance` | Return account balance (supports `Stripe-Account` header) |
| POST | `/v1/accounts` | `accounts.star#on_create_account` | Create a connected account |
| GET | `/v1/accounts/{id}` | `accounts.star#on_retrieve_account` | Retrieve a connected account |
| POST | `/v1/accounts/{id}` | `accounts.star#on_update_account` | Update a connected account (e.g. capabilities) |
| GET | `/v1/accounts` | `accounts.star#on_list_accounts` | List connected accounts |
| POST | `/v1/account_links` | `account_links.star#on_create_account_link` | Create an account link (onboarding URL) |
| POST | `/v1/transfers` | `transfers.star#on_create_transfer` | Create a transfer to a connected account |
| GET | `/v1/transfers/{id}` | `transfers.star#on_retrieve_transfer` | Retrieve a transfer |
| GET | `/v1/transfers` | `transfers.star#on_list_transfers` | List transfers (`?destination=` filter) |
| POST | `/v1/transfers/{id}/reversals` | `transfers.star#on_reverse_transfer` | Reverse a transfer |
| POST | `/v1/payouts` | `payouts.star#on_create_payout` | Create a payout |
| GET | `/v1/payouts` | `payouts.star#on_list_payouts` | List payouts (`?destination=` filter) |

Any unmatched route returns `404 {"error":"resource_not_found"}`.

## Backing stores

| Collection | Seed fixture | Purpose |
|------------|-------------|---------|
| `charges` | `fixtures/charges.jsonl` | Charge records |
| `customers` | `fixtures/customers.jsonl` | Customer records |
| `connect_accounts` | `fixtures/connect_accounts.jsonl` | Connected accounts |
| `transfers` | — | Transfer records (start empty) |
| `payouts` | — | Payout records (start empty) |

IDs are generated with provider-style prefixes (`ch_`, `cus_`, `acct_`, `tr_`, `po_`)
via a KV-backed sequence counter.

Per-account balances for Connect are tracked in the KV store under
`bal_<account_id>` keys.

## Shared library

Shared helpers (`_bearer_token`, `_require_auth`, `_next_id`, `_to_int`,
`_stripe_account`, `_get_balance`, `_set_balance`, `_not_found`) are defined
in `scripts/lib.star` and preloaded into every handler script via stunt's
`LoadWithLib` mechanism. This avoids code duplication across handler scripts.

## Layout

```
adapter.yaml                    Manifest: endpoints, resources, rules, identity
DISCLAIMER                      Not affiliated / synthetic-only notice
README.md                       This file
scripts/
  lib.star                      Shared helpers (auth, IDs, balance, etc.)
  tokens.star                   Token mint endpoint (POST /v1/tokens)
  charges.star                  Charge CRUD + capture/refund
  customers.star                Customer CRUD
  balance.star                  Balance endpoint (platform + per-account via Stripe-Account header)
  accounts.star                 Connect: connected accounts (CRUD + capabilities)
  account_links.star            Connect: account links (onboarding URLs)
  transfers.star                Connect: transfers (create/retrieve/list/reverse)
  payouts.star                  Connect: payouts (create/list)
fixtures/
  charges.jsonl                 Seed data for the charges collection
  customers.jsonl               Seed data for the customers collection
  connect_accounts.jsonl        Seed data for connected accounts
templates/
  charge.json                   Example charge response (faker placeholders)
  customer.json                 Example customer response (faker placeholders)
schemas/
  charge.schema.json            JSON Schema for a charge object
```

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  stripe:
    adapter: ./adapters/stripe-style
    config:
      webhook_url: http://localhost:9090/webhook   # optional
```

Then `stunt up` and make requests to the served address.
