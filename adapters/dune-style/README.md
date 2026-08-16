# dune-style

Dune Analytics SQL API simulator (unofficial) for local testing.

## Pain point

Dune executes SQL queries asynchronously: you submit a query, poll for completion,
then fetch paginated results. The async polling + cursor pagination is the integration pain.

## What it simulates

| Endpoint | Method | Description |
|---|---|---|
| `/api/v1/query/{id}/execute` | POST | Execute a query → PENDING (validates `query_parameters`) |
| `/api/v1/query/{id}/result` | POST | Run inline → COMPLETED + rows (accepts `query_parameters`) |
| `/api/v1/execution/{id}/status` | GET | Poll status (async state machine) |
| `/api/v1/execution/{id}/results` | GET | Get results → rows + metadata (`limit`/`offset` paging, `next_uri`) |
| `/api/v1/execution/{id}/results/csv` | GET | Same rows as CSV (`text/csv`, honors `limit`/`offset`) |
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
execution reaches COMPLETED. Status/result responses carry Dune's execution
envelope (`query_id`, `submitted_at`, `expires_at`, `execution_started_at`,
`execution_ended_at`, `is_execution_finished`). The three execution routes
run with `concurrency_key: execution_id`, so concurrent polls of the same
execution can't lose state transitions.

## Query parameters

Executes accept Dune's `query_parameters` body object. Both value shapes the
API supports work: plain values and the SDK `{type, value}` form:

```json
{"query_parameters": {"wallet_address": {"type": "TEXT", "value": "0xAbC..."},
                      "min_usd": 250}}
```

Parameters are validated against a static query catalog (the simulator's
stand-in for Dune's saved queries — `{{param}}` placeholders in the query
SQL stand in for the real thing):

| Query | Parameters | Rows |
|---|---|---|
| `3971` | `token_symbol` (TEXT, optional, default `USDC`) | 12 |
| `4242` | `wallet_address` (TEXT, **required**), `min_usd` (NUMBER, optional, default `100`) | 8 |
| other ids | `token_symbol` (TEXT, optional, default `USDC`) | 5 |

Executing a query without a **required** parameter returns Dune's documented
400 envelope: `{"error": "Bad Request"}`. Supplied parameters flow through
to the generated rows: the resolved values are stored on the execution, so
results, pagination and CSV all regenerate the same rows — identical
parameters reproduce identical rows, distinct parameter sets produce
distinct rows (`token_symbol` column, amounts, row count).

## Result pagination & CSV

`GET /execution/{id}/results` honors Dune's `limit`/`offset` query params
(default page: 10,000 rows). `result.metadata` uses Dune's documented shape
— `row_count`/`result_set_bytes` describe the returned page,
`total_row_count`/`total_result_set_bytes` the full set. When rows remain
beyond the page, the response carries `next_uri` (an absolute
`.../results?offset=N&limit=M` continuation URL minted from the request
host, followable directly) plus `next_offset`; both are `null` on the last
page. `GET /execution/{id}/results/csv` returns the same rows (same
parameters, same paging) as `text/csv` via a raw body.

## Errors

Dune's error envelope is `{"error": "<message>"}`: `400 {"error": "Bad
Request"}` (missing required parameter), `401 {"error": "Invalid API Key"}`,
`404 {"error": "Object not found"}` (unknown execution). Results endpoints
also 404 with an explanatory message while an execution is still running or
after it failed — there is nothing to fetch yet.

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
