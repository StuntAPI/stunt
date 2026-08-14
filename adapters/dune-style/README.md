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
| `/api/v1/execution/{id}/status` | GET | Poll status (async state machine) |
| `/api/v1/execution/{id}/results` | GET | Get results → rows + metadata |
| `/api/v1/auth/validate` | GET | Validate API key |

## Auth

Bearer token (`Authorization: Bearer <key>`).

## API version

`v1`

## Execution lifecycle

```
QUERY_STATE_PENDING → QUERY_STATE_EXECUTING → QUERY_STATE_COMPLETED
```

Derive-on-read state machine (timings: EXECUTING at +1s, COMPLETED at +3s
after execute, computed from the injectable clock). Status polls and result
fetches derive the current state from the clock and persist the transition
back to the executions collection. GET results returns 404 until the
execution reaches COMPLETED. Results are deterministic based on query_id.

### Failure injection (simulator extension)

The real Dune API has no sandbox failure trigger, so this adapter accepts a
simulator-only flag in the POST `/execute` body:

```json
{"simulate_fail": true}
```

The execution then terminates in `QUERY_STATE_FAILED` (Dune's real failure
vocabulary) instead of COMPLETED, and GET results returns 404.

Dune has no execution webhooks, so no events are emitted on transitions.

---

*Synthetic. No real Dune data. See [DISCLAIMER](DISCLAIMER).*
