# helius-style

Helius Solana RPC + Enhanced API simulator (unofficial) for local testing.

## Pain point

Helius combines JSON-RPC methods (getBalance, sendTransaction) with enhanced REST
endpoints (parsed transactions, NFT holdings, token balances). The pain: two different
API styles + complex Solana data shapes.

## What it simulates

| Endpoint | Method | Description |
|---|---|---|
| `/?api-key=<key>` | POST | JSON-RPC (getBalance, getLatestBlockhash, getSignatureStatuses, sendTransaction, getTransaction, getTokenAccountsByOwner) |
| `/v0/transactions` | POST | Parse encoded transactions (1-100 per request) |
| `/v0/addresses/{addr}/transactions` | GET | Enhanced Transactions API — parsed transaction history (see below) |
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

## Enhanced Transactions API (flagship)

`GET /v0/addresses/{address}/transactions?api-key=<key>` returns the address's
parsed transaction history as a bare JSON array, newest first, with the real
Helius query parameters:

| Param | Description |
|---|---|
| `before` | signature cursor — page backwards, starting after this transaction |
| `until` | signature cursor — stop before this transaction |
| `limit` | page size (default 100, max 100) |
| `type` | comma-separated list of types (`SWAP`, `TRANSFER`, …) |
| `source` | program/app source (`SYSTEM_PROGRAM`, `SPL_TOKEN`, `JUPITER`, …) |

```bash
curl "http://localhost:8080/v0/addresses/7xKX…/transactions?api-key=your-key&limit=10&type=SWAP"
```

Each item is the real parsed-transaction vocabulary — `signature`,
`timestamp`, `slot`, `fee`, `feePayer`, `nativeTransfers`,
`tokenTransfers` (with `mint` + `tokenAmount {amount, decimals, uiAmount}`),
`accountData`, `events`, `type`, `source`, `description`. `SWAP`
transactions carry the `events.swap` object (`tokenInput` / `tokenOutput`
with `tokenAmount`s, `tokenFees`, `nativeFees`).

The history is backed by the shared transaction collection:

- a deterministic history (TRANSFER/SYSTEM_PROGRAM, TRANSFER/SPL_TOKEN and
  SWAP/JUPITER, all finalized) is seeded once per address on first query;
- every transaction submitted via `sendTransaction` appears here as soon as
  it lands, so the Enhanced API, `getSignatureStatuses` and webhooks all
  observe the same lifecycle.

## JSON-RPC

| Method | Behavior |
|---|---|
| `getBalance` | `{context: {slot}, value: <lamports>}` for the address |
| `getLatestBlockhash` | blockhash + `lastValidBlockHeight` |
| `sendTransaction` | returns a signature; see the lifecycle below |
| `getSignatureStatuses` | derive-on-read confirmation statuses |
| `getTransaction` | full Solana shape by signature: `blockTime`, `slot`, `transaction.message` (accountKeys + instructions) and `meta` with `fee`, `preBalances`/`postBalances` (moved by the parsed native transfers, fee paid by the fee payer), `pre/postTokenBalances`, `logMessages`, `status: {Ok\|Err}` — `null` for an unknown signature |
| `getTokenAccountsByOwner` | `jsonParsed` subset: `{address, lamports, owner, mint, data: {program: spl-token, parsed: {info: {mint, owner, tokenAmount}}}}`; filter by `{"mint": ...}` |

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

**Simulator extensions in the config object** (`params[1]`):

- `{"simulate_fail": true}` — land with an on-chain error.
- `{"simulate_type": "SWAP"}` — store a SWAP/JUPITER parsed transaction
  (with `events.swap` and `tokenTransfers`) instead of the default TRANSFER.
- `{"simulate_address": "<pubkey>"}` — attribute the transaction to a known
  address (fee payer / transfer sender) so it shows up in that address's
  Enhanced Transactions history.

## Deterministic

Balances, NFTs, and token holdings are deterministic based on the address hash.

---

*Synthetic. No real Helius data. See [DISCLAIMER](DISCLAIMER).*

## Concurrency note

Transitions persist before webhook emission, and id-scoped routes carry
`concurrency_key`. Two remaining narrow windows exist by design: list/bulk
surfaces (advanced_search, runs list, RPC) can race a keyed single-resource
read on the same record and in principle double-emit the transition webhook;
and GraphQL mutations (braintree) key on a body id the engine cannot lock —
the REST surface is the concurrency-safe one.
