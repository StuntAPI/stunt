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

## API version

`v1`

## Simulation result

Deterministic: status `true`, gas_used based on input length, call trace with
nested calls.

---

*Synthetic. No real Tenderly data. See [DISCLAIMER](DISCLAIMER).*
