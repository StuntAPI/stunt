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
| PUT | `/api.xro/2.0/Contacts` | Upsert contacts (ContactID/ContactNumber match updates, otherwise create) |
| GET | `/api.xro/2.0/Contacts/{id}` | Get contact (404 envelope if not found) |
| PUT | `/api.xro/2.0/Contacts/{id}` | Update / archive / reactivate contact (`ContactStatus:"ARCHIVED"` or `"ACTIVE"`) |
| GET | `/api.xro/2.0/Invoices` | List invoices |
| PUT | `/api.xro/2.0/Invoices` | Create invoices (totals computed from every line item) |
| GET | `/api.xro/2.0/Invoices/{id}` | Get invoice (404 envelope if not found) |
| DELETE | `/api.xro/2.0/Invoices/{id}` | VOID invoice → `204 No Content` (kept with `Status:"VOIDED"`, `AmountDue` 0.00) |
| POST | `/api.xro/2.0/Invoices/{id}/Payments` | Record payment (partial payments accumulate; over-payment → 400) |
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

## Invoice totals

`PUT /Invoices` computes the invoice's money the way the real API does —
summing **every** line item, not just the first. Per line:

- net `LineAmount` = `UnitAmount` × `Quantity` less `DiscountRate`% (a line
  carrying only a `LineAmount` is taken as-is; `Quantity` defaults to 1);
- tax = the line's `TaxAmount` (no tax-rate registry is simulated).

The computed `LineAmount` is echoed back on each line, and the invoice
exposes:

| Field | Meaning |
|-------|---------|
| `SubTotal` | Σ line nets (excludes tax) |
| `TotalTax` | Σ per-line `TaxAmount` |
| `Total` | `SubTotal` + `TotalTax` |
| `TotalDiscount` | Σ per-line discount portions |
| `AmountDue` | outstanding balance — starts at `Total` |
| `AmountPaid` | Σ payments applied so far |

Example — `100.00 × 2 @ 10% off` (180.00) + `49.99 × 3` (149.97) +
`250.00 × 1 @ 20% off, 15.00 tax` (200.00) → `SubTotal` 529.97,
`TotalTax` 15.00, `Total` 544.97, `TotalDiscount` 70.00.

## Payments

`POST /Invoices/{id}/Payments` maintains a real running ledger on the
stored invoice:

- Payments **accumulate**: each application grows `AmountPaid` and shrinks
  `AmountDue` (the outstanding balance), so 2nd/3rd partial payments
  decrement the balance correctly; the balance never goes negative.
- The invoice flips to `PAID` exactly when the balance reaches `0.00`
  (a partial payment leaves it `AUTHORISED`).
- Over-payment (amount > `AmountDue`) → `400` with the real Xero
  validation error, carried in `Elements`:

  ```json
  {
    "ErrorNumber": "ValidationError",
    "Type": "BadRequest",
    "Message": "A validation exception has occurred.",
    "Elements": [{ "ValidationErrors": [{
      "Message": "PaymentAmount exceeds the amount outstanding on this document"
    }]}]
  }
  ```

- Payments are only accepted against `AUTHORISED` (or `SUBMITTED`) invoices;
  paying a `DRAFT`/`PAID`/`VOIDED` one → `400` with
  `"Payments can only be made against AUTHORISED documents"`.
- Every payment is persisted (payments collection) and echoed with
  `PaymentID`, `Amount`, `Date` and `Status:"AUTHORISED"`; an unknown
  invoice id → the `404` envelope.

## Contacts upsert

`PUT /Contacts` is a true upsert, like the real API:

- A contact whose `ContactID` or `ContactNumber` matches an existing one
  **updates** that record in place — the `ContactID` is stable across
  updates (no duplicate insert) and unspecified fields keep their stored
  values.
- A contact that identifies nothing existing is **created**.
- Creating — or renaming to — a `Name` held by another **active** contact →
  `400` with Xero's real validation message:
  `"The contact name <Name> is already assigned to another contact. The contact name must be unique across all active contacts."`
  (archiving a contact releases its name for reuse).

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
    "ContactNumber": "",
    "ContactStatus": "ACTIVE",
    "Name": "Acme Corp",
    "EmailAddress": "acme@example.com",
    "IsSupplier": false,
    "IsCustomer": true
  }]
}

// Invoices carry the computed totals as 2-decimal strings
{
  "Invoices": [{
    "InvoiceID": "...",
    "InvoiceNumber": "INV-000001",
    "Type": "ACCREC",
    "Status": "AUTHORISED",
    "LineItems": [{ "UnitAmount": "100.00", "Quantity": 2, "DiscountRate": "10", "LineAmount": "180.00" }],
    "SubTotal": "180.00",
    "TotalTax": "0.00",
    "Total": "180.00",
    "TotalDiscount": "20.00",
    "AmountDue": "180.00",
    "AmountPaid": "0.00"
  }]
}

// Error
{ "ErrorNumber": "TokenExpired", "Type": "Unauthorized", "Message": "..." }
```
