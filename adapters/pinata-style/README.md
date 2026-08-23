# Pinata-style IPFS pinning API simulator

A local development and testing simulator that mimics the **structure** of the
Pinata API (version `1.0`). It does **not** call the real Pinata API — all
data is synthetic.

## Quick start

```bash
stunt plan --add pinata-style --port 8080
stunt up
```

```bash
# Test authentication
curl http://localhost:8080/data/testAuthentication \
  -H "pinata_api_key: your-api-key" \
  -H "pinata_secret_api_key: your-secret"

# Pin a JSON object
curl -X POST http://localhost:8080/pinning/pinJSONToIPFS \
  -H "Authorization: Bearer your-jwt" \
  -H "Content-Type: application/json" \
  -d '{
    "pinataContent": { "hello": "world" },
    "pinataMetadata": { "name": "my-pin" }
  }'

# List pins
curl http://localhost:8080/data/pinList \
  -H "Authorization: Bearer your-jwt"

# Unpin
curl -X DELETE http://localhost:8080/pinning/unpin/Qm... \
  -H "Authorization: Bearer your-jwt"
```

## Auth

Pinata accepts either:
- `pinata_api_key` + `pinata_secret_api_key` headers (API key pair)
- `Authorization: Bearer <JWT>` header

Requests without auth return `401`.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/pinning/pinFileToIPFS` | Pin a file (multipart upload, file part required) → CID |
| POST | `/pinning/pinJSONToIPFS` | Pin a JSON object → CID |
| DELETE | `/pinning/unpin/{cid}` | Unpin by CID |
| GET | `/data/pinList` | List pins (params: `hashContains`, `pinStart`/`pinEnd`, `pinSizeMin`/`pinSizeMax`, `status`, `metadata` name, `pageLimit` default 10, `pageOffset`) |
| GET | `/data/testAuthentication` | Verify auth |
| GET | `/data/pinByHash` | Lookup pin by hash (`hash` query param required) |

## Stateful behavior

Pins are stored in a local collection. CIDs are real CIDv0 values — base58 of
the sha2-256 multihash of the pinned bytes (the compact, key-sorted JSON
serialization for `pinJSONToIPFS`, the file part's bytes for
`pinFileToIPFS`) — so pinning identical content returns the same `IpfsHash`
with `isDuplicate: true` and adds no row. `PinSize` is that content's byte
length; `Timestamp`/`date_pinned` are the request time.

`pinList` shows all previously pinned CIDs and honors the real pinList
filters (`hashContains`, `pinStart`/`pinEnd` date range,
`pinSizeMin`/`pinSizeMax`, `status`, `metadata` name) plus
`pageLimit`/`pageOffset` paging at the real default of 10 rows, with `count`
still reflecting the filtered total before slicing. Unpinning removes them.

## Response shapes

```json
// Pin result (pinFileToIPFS / pinJSONToIPFS)
{
  "IpfsHash": "Qm...",
  "PinSize": 17,
  "Timestamp": "2026-02-03T12:00:00.000Z",
  "isDuplicate": false
}

// Pin list
{
  "count": 1,
  "rows": [{
    "id": "7000000001",
    "ipfs_pin_hash": "Qm...",
    "size": 17,
    "date_pinned": "2026-02-03T12:00:00.000Z",
    "metadata": { "name": "my-pin" }
  }]
}

// Error
{ "error": { "reason": "UNAUTHORIZED", "details": "..." } }
```
