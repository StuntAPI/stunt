# tenderly-style

Tenderly Simulation API simulator (unofficial) for local testing.

## Pain point

Tenderly simulates transactions without broadcasting them, returning gas usage,
status, and call traces. The pain: complex nested simulation request/response shapes
and bundle simulation.

## What it simulates

| Endpoint | Method | Description |
|---|---|---|
| `/api/v1/account/{acct}/project/{proj}/simulate` | POST | Simulate a single transaction |
| `/api/v1/account/{acct}/project/{proj}/simulate-bundle` | POST | Simulate a bundle of transactions |
| `/api/v1/networks` | GET | List supported networks |

## Auth

Bearer token (`Authorization: Bearer <key>`).

Tokens are validated: the key must be registered in the KV store
(`token_<key>` → unix-seconds expiry). An unknown or expired token returns
`401` with Tenderly's error envelope `{"slug": "unauthorized", "message":
"Missing or invalid API key"}`. The static test token `test-token-tenderly`
is seeded automatically on first use with a far-future expiry; send any
other bearer to exercise the 401 path.

## API version

`v1`

## Simulation result

Deterministic: `status` defaults to `true` with gas_used based on input length
and a call trace. The revert path is reachable two ways — an explicit
`{"revert": true, "revert_reason": "..."}` body flag, or calldata beginning
with the `Error(string)` selector `0x08c379a0` — producing `status: false`, a
`revert_reason`, and an ABI-encoded revert `output`. Value transfers
(`value > 0`) synthesize a Transfer event log and balance overrides.

Stored simulations are retrievable via
`GET .../simulations/{id}` and listable via `GET .../simulations`.

---

*Synthetic. No real Tenderly data. See [DISCLAIMER](DISCLAIMER).*
