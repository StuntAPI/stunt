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
| `/v0/webhooks` | POST | Register a webhook |
| `/v0/webhooks` | GET | List webhooks |
| `/v0/webhooks/{id}` | GET | Get a webhook |
| `/v0/webhooks/{id}` | PUT | Edit a webhook |
| `/v0/webhooks/{id}` | DELETE | Delete a webhook |

## Auth

API key via query param (`?api-key=<key>`).

## API version

`v0`

## Webhooks

Helius webhooks deliver enhanced parsed transactions to your URL as on-chain
activity happens. Register one the way the real API does:

```bash
curl -X POST "http://localhost:8080/v0/webhooks?api-key=your-key" \
  -H "Content-Type: application/json" \
  -d '{
    "webhookURL": "http://localhost:9999/helius",
    "transactionTypes": ["ANY"],
    "authHeader": "my-shared-secret"
  }'
# -> {"webhookID": "wh_1", "webhookURL": ..., "transactionTypes": ["ANY"], ...}
```

- `transactionTypes` — event filter (`["ANY"]` subscribes to everything).
- `authHeader` — optional static value sent as the `Authorization` header on
  every delivery; use it as the shared secret.

This simulator fires a webhook when a sent transaction first **confirms**
(see the lifecycle below), with the real parsed-transaction shape. Real Helius
batches deliveries as a JSON **array** of these objects; stunt's event engine
delivers each transaction as a single object — treat every received payload
as one array element:

```json
{
  "signature": "2zYv…",
  "timestamp": 1755170000,
  "slot": 250000042,
  "type": "TRANSFER",
  "source": "SYSTEM_PROGRAM",
  "description": "Transfer 0.5 SOL",
  "fee": 5000,
  "feePayer": "…base58…",
  "nativeTransfers": [{ "fromUserAccount": "…", "toUserAccount": "…", "amount": 500000000 }],
  "events": {}
}
```

### Signature

**Unsigned by design.** Real Helius does not HMAC-sign webhook deliveries —
there is no signature header and no body MAC. Verification is the optional
`authHeader` (delivered as `Authorization`); reply 401 when it does not match.

## Transaction lifecycle

`sendTransaction` no longer lands instantly: the stored transaction carries
clock-derived milestones and `getSignatureStatuses` computes the current
status on read (derive-on-read), persisting each transition so repeated polls
agree:

```
null / not found (0-1s) -> processed (1-2s) -> confirmed (2-3s) -> finalized (>=3s)
```

That is Solana's real `confirmationStatus` vocabulary (`processed`,
`confirmed`, `finalized`), with the real value-item shape
(`{slot, confirmations, err, status: {Ok|Err}, confirmationStatus}`) and
`null` for a just-submitted signature that has no status yet. The enhanced
webhook delivery fires exactly once, when a transaction first reaches
`confirmed`.

**Failure injection (simulator extension):** pass
`{"simulate_fail": true}` as the `sendTransaction` config object (`params[1]`)
to land the transaction with an on-chain error — `err`/`status.Err` is set to
an `InstructionError` while confirmation still progresses, exactly like a real
failed Solana transaction.

## Deterministic

Balances, NFTs, and token holdings are deterministic based on the address hash.

---

*Synthetic. No real Helius data. See [DISCLAIMER](DISCLAIMER).*
