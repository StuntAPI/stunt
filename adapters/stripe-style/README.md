# Stripe-style adapter

A stunt adapter for simulating a **Stripe-style payments API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Stripe. "Stripe" is a trademark of its respective owner.
> See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

A broader-than-minimal MVP of a Stripe-style payments API: charges (create,
retrieve, list, capture, refund), customers (CRUD), and account balance.
State persists in an in-process SQLite-backed collection store, so data you
create in one request is visible in subsequent requests within the same
`stunt up` session.

Webhook events are emitted on charge lifecycle transitions (created, updated,
refunded) to a configurable webhook sink.

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

The adapter emits webhook events on charge lifecycle transitions. Events are
**fire-and-forget**: if no webhook sink is configured or the delivery fails,
the charge operation still succeeds.

| Trigger | Event type |
|---------|-----------|
| `POST /v1/charges` (create) | `charge.created` |
| `POST /v1/charges/{id}/capture` | `charge.updated` |
| `POST /v1/charges/{id}/refund` | `charge.refunded` |

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

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/v1/tokens` | `tokens.star#on_create` | Mint a test token (no auth required) |
| POST | `/v1/charges` | `charges.star#on_create` | Create a charge (status → `pending`) |
| GET | `/v1/charges/{id}` | `charges.star#on_retrieve` | Retrieve a charge |
| GET | `/v1/charges` | `charges.star#on_list` | List all charges |
| POST | `/v1/charges/{id}/capture` | `charges.star#on_capture` | Capture a charge (→ `succeeded`) |
| POST | `/v1/charges/{id}/refund` | `charges.star#on_refund` | Refund a charge (→ `refunded`) |
| POST | `/v1/customers` | `customers.star#on_create` | Create a customer |
| GET | `/v1/customers/{id}` | `customers.star#on_retrieve` | Retrieve a customer |
| GET | `/v1/customers` | `customers.star#on_list` | List all customers |
| POST | `/v1/customers/{id}` | `customers.star#on_update` | Update a customer |
| DELETE | `/v1/customers/{id}` | `customers.star#on_delete` | Delete a customer |
| GET | `/v1/balance` | `balance.star#on_get` | Return a synthetic balance |

Any unmatched route returns `404 {"error":"resource_not_found"}`.

## Backing stores

| Collection | Seed fixture | Purpose |
|------------|-------------|---------|
| `charges` | `fixtures/charges.jsonl` | Charge records |
| `customers` | `fixtures/customers.jsonl` | Customer records |

IDs are generated with provider-style prefixes (`ch_`, `cus_`) via a KV-backed
sequence counter.

## Layout

```
adapter.yaml              Manifest: endpoints, resources, rules, identity
DISCLAIMER                Not affiliated / synthetic-only notice
README.md                 This file
scripts/
  tokens.star             Token mint endpoint (POST /v1/tokens)
  charges.star            Charge CRUD + capture/refund + auth + webhooks
  customers.star          Customer CRUD + auth
  balance.star            Balance endpoint + auth
fixtures/
  charges.jsonl           Seed data for the charges collection
  customers.jsonl         Seed data for the customers collection
templates/
  charge.json             Example charge response (faker placeholders)
  customer.json           Example customer response (faker placeholders)
schemas/
  charge.schema.json      JSON Schema for a charge object
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
