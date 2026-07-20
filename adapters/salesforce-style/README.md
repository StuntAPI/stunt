# Salesforce-style adapter

A stunt adapter for simulating the **Salesforce REST API** (v60.0) locally. All
data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Salesforce. "Salesforce" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter
> is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of the Salesforce REST API surface, designed to unblock
CRM integrations during local development:

- **OAuth2:** `POST /services/oauth2/token` (password, authorization_code, or
  refresh_token grants) → `{access_token:"00D...", instance_url, token_type:"Bearer",
  id, issued_at, signature}`.
- **sObjects describe global:** `GET /services/data/v60.0/sobjects` → list of
  available objects (Account, Contact, Opportunity, Lead, User).
- **sObjects describe object:** `GET /services/data/v60.0/sobjects/Account` →
  object metadata with fields.
- **SOQL query:** `GET /services/data/v60.0/query?q=SELECT+Id,+Name+FROM+Account` →
  `{totalSize, records:[{attributes:{type,url}, Id, Name, ...}], done:true}`.
  Pattern-matches the `FROM <Entity>` token and SELECT field list — no full SOQL
  parser. Supports `WHERE Id = '...'` for single-record queries.
- **queryAll:** Same as query (includes deleted records conceptually).
- **Account/Contact/Opportunity CRUD:** `POST` (create, 201), `GET /{id}`
  (retrieve), `PATCH /{id}` (update, 204), `DELETE /{id}` (204).
- **Composite batch:** `POST /services/data/v60.0/composite` → processes
  sub-requests sequentially, returns per-request results.

Salesforce ID format: 3-char key prefix + 15-char alphanumeric (Account=001,
Contact=003, Opportunity=006, Lead=00Q, User=005).

## Auth

OAuth2 bearer tokens. API calls require `Authorization: Bearer <token>`. The token
endpoint supports the password grant for local testing convenience (real Salesforce
requires the web-server or JWT flow). The session token is `00D`-prefixed (the org
key prefix).

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/services/oauth2/token` | `oauth.star#on_token` | OAuth2 token (password/code/refresh) |
| GET | `/services/data/v60.0/sobjects` | `sobjects.star#on_describe_global` | Describe global |
| GET | `/services/data/v60.0/sobjects/Account` | `sobjects.star#on_describe_object` | Describe object |
| POST | `/services/data/v60.0/sobjects/Account` | `sobjects.star#on_create` | Create record |
| GET | `/services/data/v60.0/sobjects/Account/{id}` | `sobjects.star#on_retrieve` | Retrieve record |
| PATCH | `/services/data/v60.0/sobjects/Account/{id}` | `sobjects.star#on_update` | Update record |
| DELETE | `/services/data/v60.0/sobjects/Account/{id}` | `sobjects.star#on_delete` | Delete record |
| GET | `/services/data/v60.0/query` | `query.star#on_query` | SOQL query |
| GET | `/services/data/v60.0/queryAll` | `query.star#on_query` | SOQL query (incl. deleted) |
| POST | `/services/data/v60.0/composite` | `composite.star#on_composite` | Composite batch |

(Contact, Opportunity, Lead, User have the same CRUD pattern as Account.)

## Error shape

Salesforce uses an **array** error envelope:

```json
[
  {
    "message": "Session expired or invalid",
    "errorCode": "INVALID_SESSION_ID",
    "fields": []
  }
]
```

401 when no/invalid bearer → `errorCode:"INVALID_SESSION_ID"`.

## SOQL pattern-matching

The query handler does NOT implement a full SOQL parser. It:

1. Extracts the `FROM <Entity>` token to determine the object type.
2. Splits the SELECT field list on commas to determine which fields to project.
3. If `WHERE Id = 'value'` is present, filters records to that single Id.
4. Returns seeded + created records with the `attributes: {type, url}` block.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `access_tokens` | OAuth2 session tokens |
| `accounts` | Account records (seeded) |
| `contacts` | Contact records (seeded) |
| `opportunities` | Opportunity records (seeded) |
| `leads` | Lead records (seeded) |
| `users` | User records (seeded) |

## Usage

```yaml
services:
  salesforce:
    adapter: ./adapters/salesforce-style
```

Then `stunt up` and point your Salesforce client at the served address.

## Governor limits (documentation)

Salesforce enforces governor limits (100 SOQL queries per transaction, 10,000 DML
rows, etc.). This mock does NOT hard-fail on these — they are documented for
reference. Real integrations must design for these limits.
