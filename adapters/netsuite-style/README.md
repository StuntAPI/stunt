# NetSuite-style adapter

A stunt adapter for simulating a **NetSuite SuiteTalk REST API** (v1) locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by NetSuite. "NetSuite" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of NetSuite's SuiteTalk REST API surface, designed to
unblock ERP integrations during local development:

- **Record CRUD:** `GET/POST /services/rest/record/v1/customer`, `GET/PATCH/DELETE
  .../customer/{id}` — and the same pattern for `salesOrder`, `invoice`, `item`,
  `employee`, `vendor`.
- **SuiteQL:** `POST /services/rest/query/v1/suiteql` with body `{"q":"SELECT *
  FROM customer"}` — pattern-matches the FROM table and returns seeded rows.
- **Metadata catalog:** `GET /services/rest/record/v1/metadata-catalog` — the list
  of supported record types (the "NetSuite metadata pain").

Records are **stateful**: a customer created via POST appears in the GET list and
in SuiteQL results.

## Authentication

This adapter supports **Token-Based Authentication (TBA)** and **NLAuth**:

### TBA (Token-Based Authentication) — OAuth 1.0a-style

```
Authorization: OAuth realm="TSTDRV123",
    oauth_consumer_key="abc...",
    oauth_token="xyz...",
    oauth_signature_method="HMAC-SHA256",
    oauth_timestamp="1700000000",
    oauth_nonce="...",
    oauth_version="1.0",
    oauth_signature="..."
```

TBA signs every request with HMAC-SHA256. The canonical signing process is:

1. Build a **base string**: `METHOD & urlencode(url) & urlencode(sorted_params)`
   where params = query params + OAuth params, each as `key=value`, joined by `&`.
2. Build the **signing key**: `urlencode(consumer_secret) & urlencode(token_secret)`.
3. Compute `signature = base64(HMAC-SHA256(signing_key, base_string))`.

This mock does a **structural check** (presence of `oauth_signature` in the header)
— it does NOT validate the HMAC, which would require the real consumer/token
secrets. Full HMAC validation is the stretch goal.

### NLAuth (legacy)

```
Authorization: NLAuth realm=ACCT123, email=admin@example.com, password=secret
```

Both schemes are accepted. Requests without authentication return **401** with
NetSuite's distinctive `o:`-prefixed error envelope.

## Error shape

NetSuite uses a distinctive error envelope with `o:` prefixed keys:

```json
{
  "type": "https://docs.oracle.com/.../not-found",
  "title": "Not Found",
  "status": 404,
  "o:errorDetails": [
    {
      "detail": "That record does not exist.",
      "o:errorCode": "RCRD_DSNT_EXIST",
      "o:errorPath": ""
    }
  ]
}
```

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/services/rest/record/v1/customer` | List customers (paginated) |
| POST | `/services/rest/record/v1/customer` | Create customer |
| GET | `/services/rest/record/v1/customer/{id}` | Retrieve customer |
| PATCH | `/services/rest/record/v1/customer/{id}` | Update customer |
| DELETE | `/services/rest/record/v1/customer/{id}` | Delete customer |
| GET/POST/PATCH/DELETE | `/services/rest/record/v1/salesOrder[/{id}]` | Sales Order CRUD |
| GET/POST/PATCH/DELETE | `/services/rest/record/v1/invoice[/{id}]` | Invoice CRUD |
| GET/POST/PATCH/DELETE | `/services/rest/record/v1/item[/{id}]` | Item CRUD |
| GET/POST/PATCH/DELETE | `/services/rest/record/v1/employee[/{id}]` | Employee CRUD |
| GET/POST/PATCH/DELETE | `/services/rest/record/v1/vendor[/{id}]` | Vendor CRUD |
| POST | `/services/rest/query/v1/suiteql` | SuiteQL query |
| POST | `/services/rest/v1/suiteql` | SuiteQL query (alt path) |
| GET | `/services/rest/record/v1/metadata-catalog` | Record type catalog |

## List response shape

```json
{
  "items": [{"id": "1", "companyName": "Acme Corporation", "email": "ap@acme.example"}],
  "count": 1,
  "hasMore": false,
  "links": [{"rel": "self", "href": "/services/rest/record/v1/customer"}]
}
```

Pagination via `?offset=0&limit=50`.
