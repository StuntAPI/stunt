# fattureincloud-style

An unofficial, local-testing simulator for a **Fatture in Cloud-style
invoicing/bookkeeping API (v2)** — Italian e-invoicing and bookkeeping, useful
for testing anything that reads supplier-side documents ("what does this
company spend, and on what?") or issues invoices.

## Conventions modelled

These are the parts of the real API's *shape* that client code gets wrong if
a simulator doesn't reproduce them:

- **Envelopes** — single resources come as `{"data": {...}}`; lists use a
  Laravel-style pagination envelope (`current_page`, `data`, `from`,
  `last_page`, `path`, `per_page`, `to`, `total`). Follow `last_page`; never
  assume one page.
- **Auth** — `Authorization: Bearer <token>`. Any non-empty bearer is
  accepted (frictionless local testing); a missing or malformed header is a
  genuine `401 {"error": {"code": "unauthorized", ...}}`.
- **Scoping** — everything lives under `/c/{company_id}`. A company id
  that doesn't exist (or belongs to nobody) is a plain 404.
- **Amounts are decimal strings** (`"9800.00"`), as the real API returns
  them. `JSON.parse` + cast makes them NaN; parse them.
- **Dates** are `YYYY-MM-DD`; month bucketing is `date.slice(0, 7)`.

## Surface

| Family | Endpoints |
| --- | --- |
| companies | `GET/POST /entities`, `GET/PUT /entities/{c}/info` |
| received documents | `GET/POST /entities/{c}/received_documents`, `GET/PUT/DELETE .../{id}`, `GET .../info` |
| issued documents | same shape as received |
| suppliers / clients / products | `GET/POST .../{resource}`, `GET/PUT/DELETE .../{id}` |
| taxes | `GET /user/companies/{c}/taxes` (the standard Italian VAT bands) |
| cashbook | `GET /user/companies/{c}/cashbook/{year}/{month}` |
| webhooks | `GET/POST /c/{company_id}/subscriptions`, `GET/PUT/DELETE /c/{company_id}/subscriptions/{id}` |
| archive | `POST /entities/{c}/archive` (JSON metadata — the real one is multipart) |

List filters: `page`, `per_page` (default 50, max 200), `q` (substring on
name/description/category), `type`, `date_start`/`date_end` (inclusive, on
document date).

`GET /user/companies/{c}/received_documents/info` returns the categories in use —
the v2 "metodata" endpoint.

## Quick start

```yaml
# stunt.yaml
network: { mode: port, base_port: 4210 }
services:
  fatture:
    adapter: ./adapters/fattureincloud-style
```

```sh
curl -X POST localhost:4210/entities -H 'Authorization: Bearer any' \
     -H 'Content-Type: application/json' -d '{"name": "Acme SRL"}'
curl -X POST localhost:4210/entities/1/received_documents \
     -H 'Authorization: Bearer any' -H 'Content-Type: application/json' \
     -d '{"date": "2026-06-10", "category": "groceries", "amount_net": "9800.00"}'
curl -H 'Authorization: Bearer any' \
     'localhost:4210/entities/1/received_documents?date_start=2026-06-01&date_end=2026-06-30'
```

Not modelled: the OAuth2 token dance (pass any bearer), e-invoice
send/retrieve side effects, email dispatch. Those have no local-test
observable behaviour.

## Webhook delivery

Subscriptions (`POST /c/{company_id}/subscriptions`, real shape `{data:{sink,
types, verification_method, config}}`, ids `SUBxxx`) register the sink and
deliver signed notifications on subscribed document/entity events:

```
X-Signature: base64(HMAC-SHA256("fic-stunt-webhook-signing-secret", raw_body))
```

The secret is a fixed synthetic value (local sim only).

## Taxes

`/c/{company_id}/taxes` is the v2 **F24** document collection (amount,
due_date, status, attachment), with full CRUD — not the v1 VAT-band metodata.
