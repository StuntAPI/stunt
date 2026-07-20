# helius-style

Helius Solana RPC + Enhanced API simulator (unofficial) for local testing.

## Pain point

Helius combines JSON-RPC methods (getBalance, sendTransaction) with enhanced REST
endpoints (parsed transactions, NFT holdings, token balances). The pain: two different
API styles + complex Solana data shapes.

## What it simulates

| Endpoint | Method | Description |
|---|---|---|
| `/?api-key=<key>` | POST | JSON-RPC (getBalance, getLatestBlockhash, getSignatureStatuses, sendTransaction) |
| `/v0/transactions` | POST | Parse base64 transactions |
| `/v0/addresses/{addr}/balances` | GET | Token balances |
| `/v0/addresses/{addr}/nfts` | GET | NFT holdings |
| `/v0/names` | POST | Domain names |

## Auth

API key via query param (`?api-key=<key>`).

## API version

`v0`

## Deterministic

Balances, NFTs, and token holdings are deterministic based on the address hash.

---

*Synthetic. No real Helius data. See [DISCLAIMER](DISCLAIMER).*
