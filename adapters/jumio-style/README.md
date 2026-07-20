# jumio-style

Jumio Netverify API simulator (unofficial) for local testing.

## Pain point

Jumio's ID verification flow is **asynchronous**: you create a scan, Jumio processes it
(PENDING→DONE), extracts document data, and sends a webhook. The async timing + extracted
data retrieval is the integration pain.

## What it simulates

| Endpoint | Method | Description |
|---|---|---|
| `/netverify/v2/scans` | POST | Create scan |
| `/netverify/v2/scans/{ref}` | GET | Get scan — PENDING→DONE |
| `/netverify/v2/scans/{ref}/data` | GET | Get extracted data |
| `/netverify/v2/webhooks` | POST | Webhook receiver (X-Jumio-Webhook-Signature) |

## Auth

Bearer token (`Authorization: Bearer <token>`).

## API version

`v1`

## Scan lifecycle

```
PENDING → DONE
```

First GET after creation completes the scan with extractedData.

---

*Synthetic. No real Jumio data. See [DISCLAIMER](DISCLAIMER).*
