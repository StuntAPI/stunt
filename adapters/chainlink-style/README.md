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

# Get a specific feed
curl http://localhost:8080/feeds/0x01-ETH-USD

# Encrypt secrets (auth required)
curl -X POST http://localhost:8080/v2/functions/encryptSecrets \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{"secrets": {"API_KEY": "secret123"}}'

# Register an Automation upkeep
curl -X POST http://localhost:8080/v2/automation/registerUpkeep \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-upkeep", "triggerType": "cron"}'
```

## Auth

- **Data Feeds** (`/feeds`): public, no auth required.
- **Functions / Automation / CCIP** (`/v2/*`): Bearer token required.

Requests without auth on protected endpoints return `401`.

## Endpoints

| Method | Route | Auth | Description |
|--------|-------|------|-------------|
| GET | `/feeds` | No | List price feeds (filter via `?network=`) |
| GET | `/feeds/{feedID}` | No | Get a feed's latestAnswer |
| POST | `/v2/functions/createSecrets` | Yes | Create encrypted secrets |
| POST | `/v2/functions/encryptSecrets` | Yes | Encrypt a secrets payload |
| POST | `/v2/functions/createRequest` | Yes | Create a Functions request (starts `pending`) |
| GET | `/v2/functions/request/{id}` | Yes | Poll a request — status derived from the clock |
| POST | `/v2/automation/registerUpkeep` | Yes | Register an upkeep |
| GET | `/v2/automation/upkeeps` | Yes | List registered upkeeps |
| GET | `/v2/automation/{id}` | Yes | Get a single upkeep |
| GET | `/v2/ccip/messages` | Yes | List CCIP messages |
| GET | `/v2/ccip/lane/{src}/{dst}` | Yes | Lane status |

## Response shapes

```json
// Feeds list
{
  "data": [{
    "feedID": "0x01-ETH-USD",
    "title": "ETH / USD",
    "feedCategory": "crypto",
    "latestAnswer": "345012345678",
    "latestTimestamp": 1718453400,
    "decimals": 8,
    "network": "ethereum"
  }]
}

// Error
{ "error": { "code": "UNAUTHORIZED", "message": "..." } }
```

## Functions request lifecycle

A Functions request is a real async state machine (derive-on-read): status is
computed from the engine clock when polled and persisted back, so repeated
polls agree.

```
pending (0-1s) -> in_flight (1-3s) -> fulfilled (>=3s)
                                     \-> failed   (>=3s, via simulate_fail)
```

The states are the real Chainlink Functions vocabulary (the subscription
manager shows a request as *Pending* while the DON has not picked it up,
*In flight* while executing, then *Fulfilled* or failed/timed out). There are
no webhooks — the real provider delivers results on-chain via the fulfillment
callback — so the only side effect is persisting the transition (terminal
requests gain `result` + `completedAt`, or `errorMessage` on failure).

**Failure injection (simulator extension):** pass `"simulate_fail": true` in
the `createRequest` body to make the request land in `failed` instead of
`fulfilled` (the real sandbox has no failure trigger for Functions).

```bash
curl -X POST http://localhost:8080/v2/functions/createRequest \
  -H "Authorization: Bearer your-token" -H "Content-Type: application/json" \
  -d '{"subscriptionId": 1234, "simulate_fail": false}'
# -> { "requestID": "6000000001", "status": "pending", ... }

sleep 4
curl http://localhost:8080/v2/functions/request/6000000001 \
  -H "Authorization: Bearer your-token"
# -> { "requestID": "6000000001", "status": "fulfilled", "result": "0x...", ... }
```
