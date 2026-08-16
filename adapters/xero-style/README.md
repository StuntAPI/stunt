# Xero Accounting API simulator

A local development and testing simulator that mimics the **structure** of the
Xero Accounting API (version `2.0`, OAuth2). It does **not** call the real Xero
API — all data is synthetic.

## Quick start

```bash
stunt plan --add xero-style --port 8080
stunt up
```

```bash
# List tenants (connections)
curl http://localhost:8080/connections \
  -H "Authorization: Bearer your-access-token"

# List contacts (requires xero-tenant-id)
curl http://localhost:8080/api.xro/2.0/Contacts \
  -H "Authorization: Bearer your-access-token" \
  -H "xero-tenant-id: a1b2c3d4-e5f6-7890-abcd-ef1234567890"

# Create a contact
curl -X PUT http://localhost:8080/api.xro/2.0/Contacts \
  -H "Authorization: Bearer your-access-token" \
  -H "xero-tenant-id: a1b2c3d4-e5f6-7890-abcd-ef1234567890" \
  -H "Content-Type: application/json" \
  -d '{ "Contacts": [{ "Name": "Acme Corp", "EmailAddress": "acme@example.com" }] }'
```

## Auth

- **OAuth2 Bearer**: `Authorization: Bearer <token>` — required on all endpoints.
- **xero-tenant-id**: Required on all `/api.xro/*` endpoints (multi-tenant pain).
  `/connections` does NOT require it (it IS the tenant list).

Tokens are validated: the bearer must be registered in the KV store
(`token_<token>` → unix-seconds expiry). An unknown or expired token returns
`401` with Xero's error envelope `{"ErrorNumber": "TokenExpired", "Type":
"Unauthorized", "Message": "The access token has expired or is invalid"}` —
the same envelope as a missing token. The static test token `xero-token` is
seeded automatically on first use with a far-future expiry; send any other
bearer to exercise the 401 path.

Requests without Bearer return `401`. Requests without `xero-tenant-id` return `400`.

## Webhooks

Inbound webhook receiver (`POST /webhooks`) — Xero's signature scheme,
verified **for real**:

```
x-xero-signature: base64(HMAC-SHA256(webhook_key, raw_request_body))
```

The `webhook_key` is configured in the Xero app. The signature is computed
over the **raw** request bytes (verbatim, never re-serialized JSON).

For this simulator the synthetic `webhook_key` is the documented constant
`stunt-xero-webhook-key` (public + low-entropy; local stunt only — never
reuse outside the simulator), so a conforming client sends:

```
x-xero-signature: base64(HMAC-SHA256("stunt-xero-webhook-key", raw_request_body))
```

Verification in Go:

```go
mac := hmac.New(sha256.New, []byte("stunt-xero-webhook-key"))
mac.Write(rawBody) // the verbatim request bytes
expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), []byte(r.Header.Get("x-xero-signature"))) {
    http.Error(w, "Unauthorized", http.StatusUnauthorized) // 401
}
```

Behavior:

- Correct signature → `200 {"status":"OK"}`.
- Missing header, or a signature that does not match the MAC of the raw
  body → `401 Unauthorized` with Xero's error envelope
  (`{"ErrorNumber":"Unauthorized","Type":"Unauthorized","Message":...}`).
  Xero requires 401 on verification failure; any other status is treated as
  retryable and eventually disables the webhook.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/connections` | List tenants |
| GET | `/api.xro/2.0/Contacts` | List contacts (archived contacts included, `ContactStatus:"ARCHIVED"`) |
| PUT | `/api.xro/2.0/Contacts` | Create contacts |
| GET | `/api.xro/2.0/Contacts/{id}` | Get contact (404 envelope if not found) |
| PUT | `/api.xro/2.0/Contacts/{id}` | Update / archive / reactivate contact (`ContactStatus:"ARCHIVED"` or `"ACTIVE"`) |
| GET | `/api.xro/2.0/Invoices` | List invoices |
| PUT | `/api.xro/2.0/Invoices` | Create invoices |
| GET | `/api.xro/2.0/Invoices/{id}` | Get invoice (404 envelope if not found) |
| DELETE | `/api.xro/2.0/Invoices/{id}` | VOID invoice → `204 No Content` (kept with `Status:"VOIDED"`, `AmountDue` 0.00) |
| POST | `/api.xro/2.0/Invoices/{id}/Payments` | Record payment (marks the invoice `PAID`, `AmountDue` 0.00) |
| GET | `/api.xro/2.0/Accounts` | Chart of accounts |
| GET | `/api.xro/2.0/BankTransactions` | Bank transactions |
| GET | `/api.xro/2.0/Items` | Inventory items |
| GET | `/api.xro/2.0/TrackingCategories` | Tracking categories |
| POST | `/webhooks` | Inbound webhook |

Any unmatched route returns Xero's `404` envelope
(`{ ErrorNumber: 404, Type: "NotFound", Message: ... }`).

## Soft delete (void + archive)

Xero never destroys invoices or contacts; stunt reproduces the terminal
soft-delete states:

- **`DELETE /api.xro/2.0/Invoices/{id}`** voids the invoice (real Xero
  invoices are not deletable — the terminal state is `VOIDED`): the record is
  kept with `Status:"VOIDED"` and `AmountDue:"0.00"`, and remains retrievable
  via `GET /Invoices/{id}` and filterable via `Statuses=VOIDED` /
  `where=Status=="VOIDED"`. Voiding a `PAID` or already-`VOIDED` invoice →
  `400 { ErrorNumber:"ValidationError", Type:"BadRequest" }`; unknown id → the
  usual `404` envelope.
- **`PUT /api.xro/2.0/Contacts/{id}`** with `{ "ContactStatus": "ARCHIVED" }`
  archives the contact (Xero's archive IS an update). The contact stays
  readable by id and in list results (real Xero lists include archived
  contacts; filter with `where=ContactStatus=="ARCHIVED"`), and is
  reactivatable with `"ACTIVE"`. An invalid `ContactStatus` → `400`
  `ValidationError`; unknown id → `404` envelope.

## Filtering and sorting

Before paging, the list endpoints honor Xero's documented filter/sort query
params: `where` (AND'ed conditions like `Status=="AUTHORISED"` or
`Name.Contains("acme")`, joined with `&&`/`AND`) and `order` (`InvoiceNumber`
or `Date DESC`) on all `api.xro` list endpoints; `Invoices` additionally
supports `Statuses` (comma list), `InvoiceNumber` (comma list) and
`ContactID`; `Contacts` additionally supports `search` (case-insensitive
partial match on name or email).

## Pagination

List endpoints (`GET /connections`, `GET .../Contacts`, `GET .../Invoices`)
support Xero-style page-number pagination:

- `page` — 1-based page number (defaults to `1`).
- `pageSize` — items per page.

When `pageSize` is omitted or `<= 0`, paging is **disabled** and the whole
list is returned in one response. When paging is active and more pages remain,
the response carries a top-level `nextPage` field with the next page number —
echo it back as the `page` query param to walk the list:

```bash
curl "http://localhost:8080/api.xro/2.0/Invoices?page=1&pageSize=50" \
  -H "Authorization: Bearer your-access-token" \
  -H "xero-tenant-id: a1b2c3d4-e5f6-7890-abcd-ef1234567890"
# → { "Id": "...", "Status": "OK", "Invoices": [...], "nextPage": "2" }
```

## Dates

Dates on newly written entities are **clock-derived**, never frozen
literals: an invoice created without a `Date` gets the current instant
(RFC3339), and one created without a `DueDate` gets now + 30 days (Xero's
default terms). Payments recorded without a `Date` likewise default to the
current instant.

## Response shapes

```json
// Xero envelope: { Id, Status, <Entities>: [...] }
{
  "Id": "00000000-0000-0000-0000-000000000001",
  "Status": "OK",
  "Contacts": [{
    "ContactID": "...",
    "Name": "Acme Corp",
    "EmailAddress": "acme@example.com",
    "IsSupplier": false,
    "IsCustomer": true
  }]
}

// Error
{ "ErrorNumber": "TokenExpired", "Type": "Unauthorized", "Message": "..." }
```
