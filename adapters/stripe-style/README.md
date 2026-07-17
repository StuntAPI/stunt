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

Webhook event emission on charge creation is **stubbed** — the `events`
primitive is not yet wired into Starlark handler builtins.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
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
  charges.star            Charge CRUD + capture/refund handlers
  customers.star          Customer CRUD handlers
  balance.star            Balance endpoint handler
fixtures/
  charges.jsonl           Seed data for the charges collection
  customers.jsonl         Seed data for the customers collection
templates/
  charge.json             Example charge response (faker placeholders)
  customer.json           Example customer response (faker placeholders)
schemas/
  charge.schema.json      JSON Schema for a charge object
```

## Auth

The adapter declares `identity.token_scheme: bearer` as metadata. Auth is **not
enforced** — any (or no) `Authorization` header is accepted. This is intentional
for local testing convenience.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  stripe:
    adapter: ./adapters/stripe-style
```

Then `stunt up` and make requests to the served address.
