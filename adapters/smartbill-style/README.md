# smartbill-style

An unofficial, local-testing simulator for a **SmartBill-style Romanian
invoicing/bookkeeping API (v1)** — useful for testing anything that issues
invoices and proforma, records payments, tracks stock, or reads PURCHASE
invoices ("what does this company spend, and on what?").

## Conventions modelled

- **Auth** — `Authorization: Basic base64(<username>:<token>)`. Any
  credentials pair is accepted (frictionless local testing); a missing or
  malformed header is a genuine `401 {"error": {"code": "unauthorized", ...}}`.
- **Envelopes** — bare JSON, unlike REST-wrapper APIs: lists are keyed arrays
  with plain pagination metadata; create endpoints return an **empty body**;
  single reads return the object itself.
- **Pagination** — plain `page`/`pageSize` query parameters (default 25,
  max 100); each response carries `page`, `pageSize`, `totalPages`,
  `totalRecords`. Follow `totalPages`.
- **Scoping** — every endpoint requires the `cif` (company tax id) of a
  company registered for the credentials; a foreign or unknown cif is a
  plain 404.
- **Money and quantities are decimal strings** (`"98.00"`, `"10"`).
- **Dates** are `YYYY-MM-DD`; list filters are `startDate`/`endDate`
  (inclusive, on issue date).

## Surface

| Family | Endpoints |
| --- | --- |
| company | `GET /v1/company` |
| invoices | `POST /v1/invoice`, `GET /v1/invoice`, `GET /v1/invoice/list`, `PUT /v1/invoice/cancel` |
| payments | `POST /v1/invoice/payment`, `DELETE /v1/invoice/payment` |
| estimates (proforma) | `POST /v1/estimate`, `GET /v1/estimate/list`, `PUT /v1/estimate/cancel` |
| purchase invoices (spend) | `POST /v1/purchase`, `GET /v1/purchase/list` |
| stocks | `GET /v1/stocks`, `POST /v1/stocks/movement` |
| messages | `POST /v1/message/email` (recorded; no delivery) |

Purchase invoice **product lines** carry an explicit `category` — the
classification in the customer's books, and the natural unit of spend: one
invoice can mix groceries and utilities.

`POST /sim/company` is a simulator bootstrap (register a cif for the
credentials); the real API assumes the company already exists behind them.
Everything else seeds through the real endpoints.

Not modelled: PDF rendering, e-Factura (ANAF SPV) transmission, email/SMS
delivery — no local-test observable behaviour.

## Quick start

```yaml
# stunt.yaml
network: { mode: port, base_port: 4210 }
services:
  smartbill:
    adapter: ./adapters/smartbill-style
```

```sh
CRED=$(printf 'user:token' | base64)
curl -X POST localhost:4210/sim/company -H "Authorization: Basic $CRED" \
     -H 'Content-Type: application/json' -d '{"cif": "RO12345678", "name": "Acme SRL"}'
curl -X POST "localhost:4210/v1/purchase?cif=RO12345678" \
     -H "Authorization: Basic $CRED" -H 'Content-Type: application/json' \
     -d '{"issueDate": "2026-06-05", "supplierName": "Metro",
          "products": [{"name": "Beans", "category": "groceries",
                        "quantity": "10", "price": "98.00"}]}'
curl -H "Authorization: Basic $CRED" \
     "localhost:4210/v1/purchase/list?cif=RO12345678&startDate=2026-06-01&endDate=2026-06-30"
```
