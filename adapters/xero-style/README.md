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

Requests without Bearer return `401`. Requests without `xero-tenant-id` return `400`.

## Webhooks

Xero webhook signature scheme:
```
x-xero-signature: base64(HMAC-SHA256(webhook_key, raw_request_body))
```
The `webhook_key` is configured in the Xero app. The signature is computed
over the **raw** request body (not parsed JSON). This simulator accepts any
non-empty signature for local testing.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/connections` | List tenants |
| GET | `/api.xro/2.0/Contacts` | List contacts |
| PUT | `/api.xro/2.0/Contacts` | Create contacts |
| GET | `/api.xro/2.0/Invoices` | List invoices |
| PUT | `/api.xro/2.0/Invoices` | Create invoices |
| GET | `/api.xro/2.0/Invoices/{id}` | Get invoice |
| POST | `/api.xro/2.0/Invoices/{id}/Payments` | Record payment |
| GET | `/api.xro/2.0/Accounts` | Chart of accounts |
| GET | `/api.xro/2.0/BankTransactions` | Bank transactions |
| GET | `/api.xro/2.0/Items` | Inventory items |
| GET | `/api.xro/2.0/TrackingCategories` | Tracking categories |
| POST | `/webhooks` | Inbound webhook |

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
