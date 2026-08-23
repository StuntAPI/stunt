# smartbill-style

An unofficial, local-testing simulator for a **SmartBill-style Romanian
invoicing/bookkeeping API (v1)** — useful for testing anything that issues
invoices and proforma, records payments, tracks stock, or reads PURCHASE
invoices ("what does this company spend, and on what?").

## Conventions modelled

- **Auth** — `Authorization: Basic base64(<username>:<token>)`. Any
  credentials pair is accepted (frictionless local testing); a missing or
  malformed header is a genuine `401` carrying the `errorText` shape.
- **Envelopes** — bare JSON, unlike REST-wrapper APIs: document create
  endpoints return an **empty body**; single reads return the object
  itself; stocks and metadata (`/tax`, `/series`) use `{list: [...]}`.
- **Scoping** — every endpoint requires the `cif` (company tax id) of a
  registered company; a foreign or unknown cif is a plain 404. Documents
  are read by `?cif=&seriesname=&number=` — there are no list endpoints.
- **Money and quantities are JSON numbers**, not decimal strings.
- **Payments** use the real `{payment: {companyVatCode, value, type,
  isCash, invoicesList}}` envelope; a body without it is a 422.
- **Dates** are `YYYY-MM-DD`.

## Surface

| Family | Endpoints |
| --- | --- |
| invoices | `POST /invoice`, `GET /invoice`, `PUT /invoice/cancel`, `PUT /invoice/restore`, `GET /invoice/paymentstatus` |
| estimates (proforma) | `POST /estimate`, `GET /estimate`, `PUT /estimate/cancel` |
| purchase invoices (spend) | `POST /purchase`, `GET /purchase` |
| payments | `POST /payment`, `POST /payment/v2`, `DELETE /payment/v2` (by `?paymentId=`) |
| stocks | `GET /stocks` |
| messages | `POST /document/send` (recorded; no delivery) |
| metadata | `GET /tax`, `GET /series` |

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
curl -X POST "localhost:4210/purchase?cif=RO12345678" \
     -H "Authorization: Basic $CRED" -H 'Content-Type: application/json' \
     -d '{"issueDate": "2026-06-05", "supplierName": "Metro",
          "products": [{"name": "Beans", "category": "groceries",
                        "quantity": "10", "price": "98.00"}]}'
curl -H "Authorization: Basic $CRED" \
     "localhost:4210/purchase/list?cif=RO12345678&startDate=2026-06-01&endDate=2026-06-30"
```

## Corrections (review)

The real SmartBill Cloud API has **no version segment** (base
`…/api/`), uses `seriesname` (not `series`), `client`/`supplier` objects
(not buyer/supplier scalar fields), the `{payment: {companyVatCode,
value, type, isCash, invoicesList}}` envelope, `{list: [...]}` for stocks
and metadata, `errorText` errors, and **JSON numbers** for all money and
quantities. Amounts are NOT decimal strings. There are no list endpoints
for invoices/estimates/purchases — documents are read by
`?cif=&seriesname=&number=`. `GET /invoice/paymentstatus` reports
`{invoiceTotalAmount, paidAmount, unpaidAmount, paid}`. The
`paymentId` disclosed by `POST /payment` is a simulator extension so the
delete flow is driveable. `/sim/*` routes are simulator affordances, not
API surface.
