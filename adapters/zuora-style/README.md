# Zuora-style adapter

A stunt adapter for simulating the **Zuora Billing REST API** (v1) locally. All data is
synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed by, or
> sponsored by Zuora. "Zuora" and related marks are trademarks of their respective
> owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

A faithful behavioral mock of the Zuora Billing REST API surface, designed to unblock
subscription billing integrations during local development:

- **Auth:** Bearer token (OAuth) OR legacy Zuora auth via `apiAccessKeyId` +
  `apiSecretAccessKey` (passed as body fields or HTTP headers — the Zuora-specific auth
  pain where credentials are non-standard fields instead of standard bearer).
- **Accounts (stateful):** `GET /v1/accounts/{accountKey}` → account by ID or number.
  `GET /v1/accounts` → list. `POST /v1/accounts` → create.
- **Subscriptions (stateful):** `GET /v1/subscriptions/{key}` → get subscription with
  plans. `POST /v1/subscriptions` → create subscription (the complex billing-data-model
  pain with `subscribeToRatePlans`). `PUT .../subscriptions/{key}` → update/renew.
  `POST .../subscriptions/{key}/cancel` → cancel.
- **Usage (metered billing):** `POST /v1/usage` → record usage
  (`{AccountId, Quantity, StartDateTime, UOM}`). `GET /v1/usage` → list usage records.
- **Invoices:** `GET /v1/invoices/{id}` → invoice details.
- **Payments:** `GET /v1/payments` → list payments.
- **Payment methods:** `POST /v1/payment-methods/credit-cards` → credit card
  tokenization (the payment-method pain).
- **Billing preview:** `POST /v1/transactions/billing/preview` → preview invoice for a
  subscription.
- **ZOQL query:** `POST /v1/action/query` → Zuora Object Query Language
  (`{queryString:"select Id from Account"}`).

All accounts, subscriptions, and usage records are **stateful** — seed data is
pre-loaded so lists return data immediately. Created subscriptions and usage records
appear in subsequent queries.

## Auth

Zuora supports two authentication schemes:

1. **Bearer (OAuth):** `Authorization: Bearer <token>`
2. **Legacy (apiAccessKeyId/apiSecretAccessKey):** Pass `apiAccessKeyId` and
   `apiSecretAccessKey` as either request body fields or HTTP headers.

This mock accepts either scheme. The legacy scheme uses non-standard credential fields
instead of the usual Authorization header — a well-known Zuora-specific pain point.

## Zuora response envelope

Success responses use:

```json
{
  "success": true,
  "accountId": "ACC-A",
  ...
}
```

Error responses use Zuora's distinctive `{success, reasons}` envelope:

```json
{
  "success": false,
  "processId": "synthetic-process",
  "reasons": [{"code": "90000010", "message": "Authentication required"}]
}
```

401 when no auth → `success:false` with `reasons` array. 404 → `success:false`.

## ZOQL (Zuora Object Query Language)

ZOQL queries are sent via `POST /v1/action/query`:

```json
{
  "queryString": "select Id, Name from Account"
}
```

This mock pattern-matches the query to determine which collection to search and returns
synthetic records. Supports `SELECT <fields> FROM <Object> [WHERE Id = 'value']`.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/v1/accounts` | `accounts.star#on_list_accounts` | List accounts |
| POST | `/v1/accounts` | `accounts.star#on_create_account` | Create account |
| GET | `/v1/accounts/{accountKey}` | `accounts.star#on_get_account` | Get account |
| GET | `/v1/subscriptions/{key}` | `subscriptions.star#on_get_subscription` | Get subscription |
| POST | `/v1/subscriptions` | `subscriptions.star#on_create_subscription` | Create subscription |
| PUT | `/v1/subscriptions/{key}` | `subscriptions.star#on_update_subscription` | Update subscription |
| POST | `/v1/subscriptions/{key}/cancel` | `subscriptions.star#on_cancel_subscription` | Cancel subscription |
| POST | `/v1/usage` | `usage.star#on_record_usage` | Record usage |
| GET | `/v1/usage` | `usage.star#on_list_usage` | List usage |
| GET | `/v1/invoices/{id}` | `billing.star#on_get_invoice` | Get invoice |
| GET | `/v1/payments` | `billing.star#on_list_payments` | List payments |
| POST | `/v1/payment-methods/credit-cards` | `billing.star#on_create_payment_method` | Create payment method |
| POST | `/v1/transactions/billing/preview` | `billing.star#on_preview_billing` | Preview billing |
| POST | `/v1/action/query` | `query.star#on_query` | ZOQL query |

## Backing stores

| Collection | Purpose |
|------------|---------|
| `accounts` | Account records (seeded) |
| `subscriptions` | Subscription records (seeded) |
| `usage` | Usage records (stateful) |
| `invoices` | Invoice records (seeded) |
| `payments` | Payment records |
| `payment_methods` | Payment method records |

## Usage

```yaml
services:
  zuora:
    adapter: ./adapters/zuora-style
```

Then `stunt up` and point your Zuora client at the served address.
