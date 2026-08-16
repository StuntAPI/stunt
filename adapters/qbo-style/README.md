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
  Customer` → `{QueryResponse:{Customer:[...]}, time}`. The FROM entity token
  is matched first (so `... FROM Invoice WHERE CustomerRef.value = '5'` is an
  Invoice query), with a substring fallback for malformed input.
- **Customer CRUD:** `POST /v3/company/{realmId}/customer` (no `Id` creates;
  with `Id` performs a QBO-style full/sparse UPDATE), `GET
  /v3/company/{realmId}/customer` (with `?id=` or list-all), `GET/DELETE
  /v3/company/{realmId}/customer/{id}`.
- **Invoice CRUD:** `POST /v3/company/{realmId}/invoice` (create, or
  `?operation=void` to void), `GET/DELETE
  /v3/company/{realmId}/invoice/{id}`.
- **Customer delete is DEACTIVATION, not destruction** (see below).
- **Invoice void:** `POST /v3/company/{realmId}/invoice?operation=void` with
  `{Id, SyncToken}` flips the invoice to `status:"Voided"` and zeroes its
  `Balance` — the record is kept (QBO's soft-delete terminal state).
- **Fault errors:** QBO's distinctive `{Fault:{Error:[{Message, code, Detail}],
  type}}` envelope. 401 on expired/invalid token → `code:"32001"`.

Customers and invoices are **stateful** — a seed customer and seed invoice are
pre-loaded so queries return data immediately.

## Soft delete (deactivation + void)

QBO never hard-deletes customers or voids invoices by destruction; stunt
reproduces the observable end states:

- **`DELETE /v3/company/{realmId}/customer/{id}`** deactivates the customer:
  the stored record is kept with `Active=false` and a bumped `SyncToken`
  (the same end state a sparse `POST /v3/company/{realmId}/customer` with
  `{"Id": …, "sparse": true, "Active": false}` produces — the real-QBO way to
  deactivate). The response is `{Customer: {Id, domain:"QBO", Active:false,
  status:"Deleted"}, time}`.
  - `GET /customer/{id}` and `GET /customer?id=` still return the deactivated
    customer (with `Active:false`).
  - `GET /customer` (list-all) returns only **active** customers.
  - `query?query=select * from Customer` returns only active customers by
    default; an explicit `WHERE Active = False` surfaces the deactivated ones
    (any `WHERE` mentioning `Active` replaces the implicit `Active = True`).
  - Invoices keep their `CustomerRef` — deactivating a customer orphans
    nothing.
- **`POST /v3/company/{realmId}/invoice?operation=void`** with `{Id,
  SyncToken}` voids the invoice: the record is kept, `status:"Voided"`,
  `Balance: 0`. Unknown `Id` → `404` Fault `code:"620"`; missing `Id` → `400`
  Fault `code:"610"`.
- `DELETE /invoice/{id}` remains a hard delete of the invoice record itself
  (QBO permits deleting draft invoices).

## Auth

OAuth2 bearer tokens. API calls require BOTH `Authorization: Bearer <token>` AND
the `realmId` in the URL path. Access tokens are short-lived (1hr). Each refresh
issues a new access_token AND a new refresh_token (the old refresh_token is
invalidated — the infamous QBO refresh churn).

Tokens are validated against the `access_tokens` store collection. Minted
tokens carry an `expires_at` (unix) set to `now + 3600`, matching the
advertised `expires_in`; an expired or unknown token returns `401` with the
Fault envelope below (`code:"32001"`).

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/oauth/v2/authorize` | `oauth.star#on_authorize` | 302 redirect with code+state+realmId |
| POST | `/oauth/v2/tokens/bearer` | `oauth.star#on_token` | Token exchange + refresh |
| GET/POST | `/v3/company/{realmId}/query` | `query.star#on_query` | SQL-like query (honors `WHERE` = != > >= < <= LIKE IN, `ORDER BY` ASC/DESC and `MAXRESULTS n`) |
| POST | `/v3/company/{realmId}/customer` | `customer.star#on_create_customer` | Create customer (with `Id`: update / sparse deactivation) |
| GET | `/v3/company/{realmId}/customer` | `customer.star#on_read_customer` | List/get customer (list returns active only) |
| GET | `/v3/company/{realmId}/customer/{id}` | `customer.star#on_read_customer_by_id` | Get customer by ID (also inactive) |
| DELETE | `/v3/company/{realmId}/customer/{id}` | `customer.star#on_delete_customer_by_id` | Deactivate customer (`Active:false`, record kept) |
| POST | `/v3/company/{realmId}/invoice` | `invoice.star#on_create_invoice` | Create invoice (`?operation=void` to void) |
| GET | `/v3/company/{realmId}/invoice/{id}` | `invoice.star#on_read_invoice` | Get invoice by ID |
| DELETE | `/v3/company/{realmId}/invoice/{id}` | `invoice.star#on_delete_invoice` | Delete invoice (returns `status:"Deleted"`) |

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
400 on a missing required field (e.g. `DisplayName`, invoice `Line`) →
`code:"610"`. 404 when an entity does not exist → `code:"620"`.

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
