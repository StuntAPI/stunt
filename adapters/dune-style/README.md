# dune-style

Dune Analytics SQL API simulator (unofficial) for local testing.

## Pain point

Dune executes SQL queries asynchronously: you submit a query, poll for completion,
then fetch paginated results. The async polling + cursor pagination is the integration pain.

## What it simulates

| Endpoint | Method | Description |
|---|---|---|
| `/api/v1/query/{id}/execute` | POST | Execute a query → PENDING |
| `/api/v1/query/{id}/result` | POST | Run inline → COMPLETED + rows |
| `/api/v1/execution/{id}/status` | GET | Poll status → COMPLETED |
| `/api/v1/execution/{id}/results` | GET | Get results → rows + metadata |
| `/api/v1/auth/validate` | GET | Validate API key |

## Auth

Bearer token (`Authorization: Bearer <key>`).

## API version

`v1`

## Execution lifecycle

```
QUERY_STATE_PENDING → QUERY_STATE_COMPLETED
```

First status poll completes the execution. Results are deterministic based on query_id.

---

*Synthetic. No real Dune data. See [DISCLAIMER](DISCLAIMER).*
