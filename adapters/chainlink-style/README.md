# Chainlink Data Feeds + Functions + Automation + CCIP simulator

A local development and testing simulator that mimics the **structure** of the
Chainlink off-chain services API (version `1.0`). It does **not** call the real
Chainlink API — all data is synthetic.

## Quick start

```bash
stunt plan --add chainlink-style --port 8080
stunt up
```

```bash
# List price feeds (public, no auth)
curl http://localhost:8080/feeds

# Latest round (AggregatorV3Interface shape)
curl http://localhost:8080/feeds/0x01-ETH-USD/latestRoundData

# Encrypt secrets (auth required)
curl -X POST http://localhost:8080/v2/functions/encryptSecrets \
  -H "Authorization: Bearer cl_mock_test_token" \
  -H "Content-Type: application/json" \
  -d '{"secrets": {"API_KEY": "secret123"}}'

# Register an Automation upkeep
curl -X POST http://localhost:8080/v2/automation/registerUpkeep \
  -H "Authorization: Bearer cl_mock_test_token" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-upkeep", "triggerType": "cron", "amount": 10}'
```

## Auth

- **Data Feeds** (`/feeds`): public, no auth required.
- **Functions / Automation / CCIP** (`/v2/*`): Bearer token required.

The token store is seeded with a static test token, `cl_mock_test_token`
(the simulator's published test credential — the same pattern as the real
services' documented test keys). Requests without auth, or with any other
token, return `401`:

```json
{ "error": { "code": "UNAUTHORIZED", "message": "Invalid or expired API token: ..." } }
```

## Endpoints

| Method | Route | Auth | Description |
|--------|-------|------|-------------|
| GET | `/feeds` | No | List price feeds (filter via `?network=`) |
| GET | `/feeds/{feedID}` | No | Feed detail with the latest derived round |
| GET | `/feeds/{feedID}/latestRoundData` | No | Latest round (`roundId`, `answer`, `startedAt`, `updatedAt`, `answeredInRound`) |
| GET | `/feeds/{feedID}/rounds` | No | Round history (newest first; `?limit=&cursor=`) |
| GET | `/feeds/{feedID}/rounds/{roundId}` | No | One round by id (`getRoundData`) |
| POST | `/v2/functions/createSecrets` | Yes | Upload DON secrets (new slot version) |
| POST | `/v2/functions/encryptSecrets` | Yes | Encrypt a secrets payload (unstored) |
| GET | `/v2/functions/secrets/{secretID}` | Yes | Fetch a stored secrets upload |
| POST | `/v2/functions/createRequest` | Yes | Create a Functions request (starts `queued`) |
| GET | `/v2/functions/request/{id}` | Yes | Poll a request — status derived from the clock |
| POST | `/v2/automation/registerUpkeep` | Yes | Register an upkeep (keepers-registry shape) |
| GET | `/v2/automation/upkeeps` | Yes | List registered upkeeps |
| GET | `/v2/automation/{id}` | Yes | Get a single upkeep (derived state) |
| POST | `/v2/automation/{id}/fund` | Yes | Add LINK to the upkeep balance (`addFunds`) |
| POST | `/v2/automation/{id}/cancel` | Yes | Cancel an upkeep (`cancelUpkeep`) |
| POST | `/v2/automation/{id}/withdraw` | Yes | Withdraw remaining funds (`withdrawFunds`, cancelled only) |
| GET | `/v2/automation/{id}/check` | Yes | Simulated `checkUpkeep` (`upkeepNeeded`, `performData`) |
| POST | `/v2/automation/{id}/perform` | Yes | Perform the upkeep (`performUpkeep`) |
| GET | `/v2/automation/{id}/performs` | Yes | Performed history (derived on read, newest first) |
| GET | `/v2/ccip/messages` | Yes | List CCIP messages |
| GET | `/v2/ccip/lane/{src}/{dst}` | Yes | Lane status |

## Data Feeds: rounds

Feeds are real aggregators here, not static values. Each feed aggregates one
round per **heartbeat** (60s simulated — the real ETH/USD feed updates on
deviation or a 3600s heartbeat) and each round's answer drifts
deterministically (HMAC-derived, bounded to ±0.25% of the feed's base). A
freshly seeded feed carries ~2 hours of backdated round history (real
aggregators have long histories), and `latestRoundData` keeps advancing with
the clock; `/rounds` walks the history back:

```json
{
  "data": {
    "roundId": "18446744073709560616",
    "answer": "345012345678",
    "startedAt": 1755400000,
    "updatedAt": 1755400060,
    "answeredInRound": "18446744073709560616"
  }
}
```

Round ids use mainnet's **phase-1 encoding** — `roundId = 2^64 +
aggregatorRoundId` — and are serialized as strings because the uint80 exceeds
float64/JS safe-integer precision (the same reason data APIs ship them as
strings). `/feeds` reflects the latest derived round in `latestAnswer` /
`latestTimestamp` / `latestRoundId`.

## Functions request lifecycle

A Functions request is a real async state machine (derive-on-read): status is
computed from the engine clock when polled and persisted back, so repeated
polls agree.

```
queued (0-1s) -> running (1-3s) -> fulfilled (>=3s)
                                \-> failed   (>=3s, via simulate_fail)
```

A fulfilled request carries the returned **bytes32 result** plus the on-chain
**`RequestFulfilled`** event shape the DON emits when fulfilling
(`requestId`, `subscriptionId`, `data`, `gasUsed`, `gasUsedAndChainIdCode`
— chainId and gasUsed packed per the coordinator's encoding):

```json
{
  "requestID": "6000000001",
  "status": "fulfilled",
  "result": "0x9f2b…",
  "fulfillEvent": {
    "name": "RequestFulfilled",
    "requestId": "0x0000…6000000001",
    "subscriptionId": 1234,
    "data": "0x9f2b…",
    "gasUsed": 78432,
    "gasUsedAndChainIdCode": "18446744073709551617"
  },
  "completedAt": 1755400062
}
```

**Failure injection (simulator extension):** the real sandbox has no failure
trigger for Functions, so `createRequest` accepts `"simulate_fail"`: `true`
or one of the failure kinds, and the terminal failure uses the real
fulfillment-code vocabulary:

| `simulate_fail` | `fulfillmentCode` | `errorMessage` |
|---|---|---|
| `computation` / `timeout` (default for `true`) | 2 `FULFILLMENT_CODE_COMPUTED_FAILED` | `code 2: computation exceeded` |
| `js_error` | 2 `FULFILLMENT_CODE_COMPUTED_FAILED` | `code 2: Uncaught exception inside Functions source` |
| `user_error` / `response_size` | 1 `FULFILLMENT_CODE_USER_ERROR` | `code 1: response exceeds 256 bytes` |
| `balance` / `cost` | 3 `FULFILLMENT_CODE_COST_EXCEEDS_COMMITMENT` | `code 3: cost exceeds commitment (subscription balance too low)` |

```bash
curl -X POST http://localhost:8080/v2/functions/createRequest \
  -H "Authorization: Bearer cl_mock_test_token" -H "Content-Type: application/json" \
  -d '{"subscriptionId": 1234}'
# -> { "requestID": "6000000001", "status": "queued", ... }

sleep 4
curl http://localhost:8080/v2/functions/request/6000000001 \
  -H "Authorization: Bearer cl_mock_test_token"
# -> { "requestID": "6000000001", "status": "fulfilled", "result": "0x...", ... }
```

There are no webhooks — the real provider delivers results on-chain via the
fulfillment callback — so the only side effect is persisting the transition.

## Functions secrets — encryption deviation (documented)

The real service encrypts DON secrets to the DON's public key with randomized
**ECIES**; the ciphertext is what gets uploaded and referenced on-chain.
stunt's crypto module deliberately has **no encryption primitives** (MAC,
hash, and asymmetric signatures only — see `internal/starlark/cryptomod.go`),
so this adapter does **not** pretend to AES/ECIES-encrypt. Instead
`createSecrets`/`encryptSecrets` compute an **honest, deterministic
integrity envelope**:

```
encryptedSecrets = "0x" + "01" + hex(HMAC-SHA256(key, donId:slot:version:payload))
```

- **Integrity + versioning, not confidentiality.** The plaintext is never
  stored and cannot be recovered from the envelope, but unlike real ECIES the
  envelope is *deterministic*: the same payload + slot version always yields
  the same string (handy for golden tests; real ECIES is randomized).
- **Real slot/version semantics.** `createSecrets` bumps a per-`(donId,
  slotID)` version each upload (the real slot-id versioning model), and a new
  version changes the envelope. `encryptSecrets` uses version 0 (unstored).
- Requests reference the envelope by value (`encryptedSecrets` in
  `createRequest`), exactly like the real off-chain flow.

## Automation: upkeep lifecycle (keepers registry)

`registerUpkeep` takes the registry's shape — `name`, `encryptedEmail`,
`gasLimit` (max 5000000), `adminAddr`, `checkData`, `triggerType`
(`condition`/`cron`), plus funding — and returns `status: "registered"`.
Balances are tracked in **juels** (1e18 wei-of-LINK) and serialized as
strings (uint96 scale).

- **fund** `POST /{id}/fund {amount}` — adds LINK (`addFunds`); rejected on a
  cancelled upkeep.
- **check** `GET /{id}/check` — the simulated `checkUpkeep` view:
  `upkeepNeeded` is true once the upkeep's cadence (`interval`, default 300s)
  has elapsed since the last perform, and `performData` is the bytes the
  registry would hand to `performUpkeep`.
- **perform** `POST /{id}/perform` — performs the upkeep (only when eligible):
  deducts the LINK premium (0.25 LINK), records the perform with a
  deterministic transaction hash and gas usage.
- **Performed history is derived on read.** The real keepers network performs
  eligible upkeeps automatically; `GET /{id}/performs` (and every upkeep read)
  simulates each elapsed cadence tick since registration as an automatic
  perform — persisting entries and premium deductions — so history and
  balance advance with wall-clock time. History is newest-first, paged.
- **cancel** `POST /{id}/cancel` — freezes the upkeep (`cancelUpkeep`); no
  further performs, balance retained.
- **withdraw** `POST /{id}/withdraw {to}` — pays out the remaining balance
  (`withdrawFunds`); only a cancelled upkeep can be withdrawn from.

Errors carry registry-real semantics: `UPKEEP_CANCELLED`,
`UPKEEP_ALREADY_CANCELLED`, `UPKEEP_NOT_CANCELLED`, `UPKEEP_NOT_NEEDED`,
`NO_FUNDS`, `NOT_FOUND`, `BAD_REQUEST` — all in the
`{"error": {"code", "message"}}` envelope.

## Response shapes

```json
// Feeds list
{
  "data": [{
    "feedID": "0x01-ETH-USD",
    "title": "ETH / USD",
    "feedCategory": "crypto",
    "latestAnswer": "345012345678",
    "latestTimestamp": 1755400060,
    "latestRoundId": "18446744073709560616",
    "decimals": 8,
    "network": "ethereum"
  }]
}

// Error
{ "error": { "code": "UNAUTHORIZED", "message": "..." } }
```
