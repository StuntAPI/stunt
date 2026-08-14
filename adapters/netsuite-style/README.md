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
- **Idempotent creates:** POSTs carrying an `externalId` are deduped — see
  [Idempotent creates (externalId upsert)](#idempotent-creates-externalid-upsert).
- **SuiteQL:** `POST /services/rest/query/v1/suiteql` with body `{"q":"SELECT *
  FROM customer"}` — pattern-matches the FROM table and returns the (stateful)
  rows for that table; other clauses (`WHERE`, `ORDER BY`, ...) are ignored.
- **Metadata catalog:** `GET /services/rest/record/v1/metadata-catalog` — the list
  of supported record types (the "NetSuite metadata pain").

Records are **stateful**: a customer created via POST appears in the GET list and
in SuiteQL results.

## Idempotent creates (externalId upsert)

NetSuite dedupes writes by `externalId`, and so does this adapter. If a POST to a
record collection includes a non-empty `externalId` that already exists in that
record type's collection, no duplicate is created: the adapter responds
`204 No Content` with the **existing** record's `Location` header. A new record
(and a fresh internal ID) is only generated when the `externalId` is absent or
not yet seen.

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

Both schemes are accepted, as is a plain `Authorization: Bearer <token>` header
(also structurally). Requests without authentication return **401** with
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
| POST | `/services/rest/record/v1/customer` | Create customer (`204` + `Location`; dedupes by `externalId`) |
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

Pagination via `?offset=0&limit=1000` (defaults: `offset=0`, `limit=1000`,
matching the real collection paging default). When another page exists the
response sets `hasMore: true` and appends a `{"rel": "next", "href": ...}`
link with the next `offset`/`limit`.

Record list endpoints also honor the documented `q` Record Collection
Filtering param (`q=email START_WITH barbara`, `q=creditlimit
GREATER_OR_EQUAL 1000`) with operators IS / IS_NOT, CONTAIN / CONTAIN_NOT,
START_WITH / START_WITH_NOT, END_WITH / END_WITH_NOT, GREATER /
GREATER_OR_EQUAL / LESS / LESS_OR_EQUAL, ANY_OF `[a,b]`, BETWEEN `[lo,hi]`,
AFTER / BEFORE / ON / ON_OR_AFTER / ON_OR_BEFORE (dates), and EMPTY /
NOT_EMPTY. Conditions join with `AND`, and `OR` is supported between AND
groups (top level); matching is case-sensitive. The symbolic operators
`= != > < >= <=` and word aliases `EQ NE GT LT GE LE LIKE IS IN BETWEEN`
are also accepted. An unparseable `q` returns `400` with
`o:errorCode: INVALID_SEARCH_PARAMETER` instead of silently unfiltered
results. `orderBy` (`orderBy=tranId DESC`) is applied before paging.
SuiteQL honors its SQL `LIMIT`/`OFFSET` clauses (falling back to the
`limit`/`offset` query params) and appends a `next` link when more rows
remain.

## Write response shape

Successful `POST` (create), `PATCH` (update), and `DELETE` return `204 No
Content`; creates and `externalId` dedupe hits carry the record's URL in the
`Location` header. Unknown record types return `404` with
`o:errorCode: RCRD_TYPE_DSNT_EXIST`; unknown record IDs return `404` with
`RCRD_DSNT_EXIST`.
