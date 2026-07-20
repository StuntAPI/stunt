# QuickBooks Online-style adapter

A stunt adapter for simulating the **QuickBooks Online API** (v3) locally. All
data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Intuit or QuickBooks. "QuickBooks" and "Intuit" and related
> marks are trademarks of their respective owners. See
> [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local development
> and testing only**.

## What it simulates

A faithful behavioral mock of the QuickBooks Online API surface, designed to
unblock accounting/billing integrations during local development:

- **OAuth2:** `GET /oauth/v2/authorize` → 302 redirect with code+state+realmId.
- **Token exchange:** `POST /oauth/v2/tokens/bearer` → `{access_token,
  refresh_token, token_type, expires_in:3600, x_refresh_token_expires_in}`.
- **Refresh-token churn:** Each refresh returns a **NEW** `refresh_token`; the old
  one is invalidated (modeling QBO's infamous refresh rotation).
- **QSQL query:** `GET/POST /v3/company/{realmId}/query?query=SELECT * FROM
  Customer` → `{QueryResponse:{Customer:[...]}, time}`. Pattern-matches entity
  name (Customer, Invoice, etc.) — no real SQL parsing.
- **Customer CRUD:** `POST /v3/company/{realmId}/customer`, `GET
  /v3/company/{realmId}/customer/{id}`.
- **Invoice CRUD:** `POST /v3/company/{realmId}/invoice`, `GET
  /v3/company/{realmId}/invoice/{id}`.
- **Fault errors:** QBO's distinctive `{Fault:{Error:[{Message, code, Detail}],
  type}}` envelope. 401 on expired/invalid token → `code:"32001"`.

Customers and invoices are **stateful** — a seed customer and seed invoice are
pre-loaded so queries return data immediately.

## Auth

OAuth2 bearer tokens. API calls require BOTH `Authorization: Bearer <token>` AND
the `realmId` in the URL path. Access tokens are short-lived (1hr). Each refresh
issues a new access_token AND a new refresh_token (the old refresh_token is
invalidated — the infamous QBO refresh churn).

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/oauth/v2/authorize` | `oauth.star#on_authorize` | 302 redirect with code+state+realmId |
| POST | `/oauth/v2/tokens/bearer` | `oauth.star#on_token` | Token exchange + refresh |
| GET/POST | `/v3/company/{realmId}/query` | `query.star#on_query` | SQL-like query |
| POST | `/v3/company/{realmId}/customer` | `customer.star#on_create_customer` | Create customer |
| GET | `/v3/company/{realmId}/customer` | `customer.star#on_read_customer` | List/get customer |
| GET | `/v3/company/{realmId}/customer/{id}` | `customer.star#on_read_customer_by_id` | Get customer by ID |
| POST | `/v3/company/{realmId}/invoice` | `invoice.star#on_create_invoice` | Create invoice |
| GET | `/v3/company/{realmId}/invoice/{id}` | `invoice.star#on_read_invoice` | Get invoice by ID |

## Error shape

QBO's distinctive Fault envelope:

```json
{
  "Fault": {
    "Error": [{"Message": "...", "code": "32001", "Detail": "..."}],
    "type": "Service"
  },
  "time": "2024-01-01T00:00:00.000-00:00"
}
```

401 when token expired/invalid → `code:"32001"`, `Message:"Authentication required"`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `oauth_codes` | Single-use OAuth authorization codes |
| `access_tokens` | Access token → realm binding |
| `refresh_tokens` | Refresh token → realm binding (rotates on refresh) |
| `customers` | Customer records (seeded) |
| `invoices` | Invoice records (seeded) |

## Usage

```yaml
services:
  qbo:
    adapter: ./adapters/qbo-style
```

Then `stunt up` and point your QBO client at the served address.
