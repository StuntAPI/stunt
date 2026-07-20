# oneinch-style

1inch DEX Aggregation Protocol API simulator (unofficial) for local testing.

## Pain point

1inch aggregates DEX quotes and returns unsigned calldata that you submit on-chain.
The pain: deterministic quote generation, calldata formatting, and approve flow integration.

## What it simulates

| Endpoint | Method | Description |
|---|---|---|
| `/v6.0/1/quote` | GET | Get quote (src, dst, amount) |
| `/v6.0/1/swap` | GET | Get swap calldata (+ fromAddress) |
| `/v6.0/1/approve/spender` | GET | Get spender address |
| `/v6.0/1/approve/calldata` | GET | Get approve calldata (token) |
| `/v6.0/1/tokens` | GET | List supported tokens |

## Auth

None (public API).

## API version

`v6.0`

## Deterministic quotes

Quotes are deterministic based on src/dst token addresses and amount — the same input
always produces the same `toAmount` and protocol split.

---

*Synthetic. No real 1inch data. See [DISCLAIMER](DISCLAIMER).*
