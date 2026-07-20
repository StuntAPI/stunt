# OpenSea API v2 + Seaport adapter

A stunt adapter for simulating the **OpenSea API v2** (including **Seaport**
order protocol) locally. All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by OpenSea. "OpenSea" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A behavioral mock of OpenSea's API v2 surface — assets, collections, events,
and the Seaport order protocol (listings + offers).

### Auth

Auth is via the `X-API-KEY` header. The mock accepts any non-empty value.
A missing header returns `401`.

### Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/api/v2/assets` | List assets (filter by `collection_slug`) |
| GET | `/api/v2/assets/{chain}/{address}/{identifier}` | Get single asset |
| GET | `/api/v2/collections/{slug}` | Get collection with stats |
| GET | `/api/v2/events` | List events (filter by `collection_slug`, `event_type`) |
| GET | `/api/v2/orders/{chain}/{protocol}/listings` | List Seaport listings |
| GET | `/api/v2/orders/{chain}/{protocol}/offers` | List Seaport offers |
| POST | `/api/v2/offers` | Create offer (STATEFUL — appears in offers list) |

### Seaport order shape

Orders reproduce the **exact Seaport parameter shape**:

```json
{
  "order_hash": "0x...",
  "protocol_address": "0x0000000068F116a894984e2DB1123eB395",
  "parameters": {
    "offerer": "0x...",
    "zone": "0x...",
    "offer": [{
      "itemType": 2,
      "token": "0x...",
      "identifierOrCriteria": "1",
      "startAmount": "1",
      "endAmount": "1"
    }],
    "consideration": [{
      "itemType": 0,
      "token": "0x...",
      "identifierOrCriteria": "0",
      "startAmount": "50000000000000000",
      "endAmount": "50000000000000000",
      "recipient": "0x..."
    }],
    "startTime": "1700000000",
    "endTime": "1700086400",
    "salt": "0x...",
    "signature": "0x..."
  }
}
```

**ItemType enum:** `0` = NATIVE (ETH), `1` = ERC20, `2` = ERC721, `3` = ERC1155.

Listings have the NFT in `offer` and payment in `consideration`. Offers are the
inverse: payment in `offer` and the NFT in `consideration`.

## Determinism & State

| Store | Purpose |
|-------|---------|
| `assets` | Seeded synthetic NFT assets |
| `collections` | Seeded synthetic collections with stats |
| `events` | Seeded synthetic sale events |
| `listings` | Seeded + created Seaport listings |
| `offers` | Seeded + created Seaport offers |

Order hashes are deterministic: the same offer parameters always produce the same
hash. Created offers via `POST /api/v2/offers` are **stateful** — they immediately
appear in the `GET /api/v2/orders/.../offers` list.

## Usage

```yaml
services:
  opensea:
    adapter: ./adapters/opensea-style
```

Then `stunt up` and point your OpenSea SDK / Seaport client at the served address.
