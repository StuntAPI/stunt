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
| POST | `/pinning/pinFileToIPFS` | Pin a file (multipart upload) → CID |
| POST | `/pinning/pinJSONToIPFS` | Pin a JSON object → CID |
| DELETE | `/pinning/unpin/{cid}` | Unpin by CID |
| GET | `/data/pinList` | List all pins |
| GET | `/data/testAuthentication` | Verify auth |
| GET | `/data/pinByHash` | Lookup pin by hash |

## Stateful behavior

Pins are stored in a local collection. `pinList` shows all previously pinned
CIDs. Unpinning removes them.

## Response shapes

```json
// Pin result (pinFileToIPFS / pinJSONToIPFS)
{
  "IpfsHash": "Qm...",
  "PinSize": 1024,
  "Timestamp": "2024-06-15T12:30:00.000Z",
  "isDuplicate": false
}

// Pin list
{
  "count": 1,
  "rows": [{
    "id": "7000000001",
    "ipfs_pin_hash": "Qm...",
    "size": 1024,
    "date_pinned": "2024-06-15T12:30:00.000Z",
    "metadata": { "name": "my-pin" }
  }]
}

// Error
{ "error": { "reason": "UNAUTHORIZED", "details": "..." } }
```
