# jumio-style

Jumio Netverify API simulator (unofficial) for local testing.

## Pain point

Jumio's ID verification flow is **asynchronous**: you create a scan, Jumio processes it
(PENDING→DONE), extracts document data, and sends a webhook. The async timing + extracted
data retrieval is the integration pain.

## What it simulates

| Endpoint | Method | Description |
|---|---|---|
| `/netverify/v2/scans` | POST | Create scan (400 without `merchantScanReference`) |
| `/netverify/v2/scans/{ref}` | GET | Get scan — PENDING→DONE/FAILED (derive-on-read) |
| `/netverify/v2/scans/{ref}` | DELETE | Delete scan (200; subsequent GETs 404) |
| `/netverify/v2/scans/{ref}/data` | GET | Get extracted data (once DONE; 409 if FAILED) |
| `/netverify/v2/webhooks` | POST | Webhook receiver (X-Jumio-Webhook-Signature) |

## Auth

Bearer token (`Authorization: Bearer <token>`).

## API version

`v1`

## Scan lifecycle

```
PENDING (0-3s after create) → DONE (+3s) | FAILED (+3s if simulate_fail)
```

The scan is a real async state machine: status is derived from the clock on
each GET and persisted. A freshly created scan stays `PENDING` for ~3s
(Jumio reports no distinct in-flight state), then transitions to `DONE` with
extracted document data. When the terminal transition first fires, the
adapter emits a `scan.completed` (or `scan.failed`) webhook signed with
`X-Jumio-Webhook-Signature` (HMAC-SHA256 hex over the raw body).

Deleting a scan first advances its lifecycle (so terminal side effects still
fire), then removes it; every later retrieve returns 404.

### Failure injection (simulator extension)

Pass `"simulate_fail": true` in the `POST /netverify/v2/scans` body: the scan
transitions to `FAILED` (Netverify's real failure status) at +3s instead of
`DONE`, emits `scan.failed`, and `/data` returns 409 with no extracted data.
This is a stunt-only flag.

### Rejection reasons

FAILED scans carry Netverify's real reject-reason vocabulary in the scan
status response (and in the `scan.failed` webhook payload):

```
"rejectionReason": "DOCUMENT_EXPIRED",
"rejectReasonDescription": "The document has expired."
```

Known codes: `CANCELLED_BY_USER`, `COMPROMISED_DOCUMENT`,
`DATE_OF_BIRTH_MISMATCH`, `DOCUMENT_EXPIRED`, `DOCUMENT_NOT_FOUND`,
`DOCUMENT_TYPE_MISMATCH`, `DUPLICATE`, `EXPIRED_TRANSACTION`,
`FORGED_IMAGES`, `MANIPULATED_DOCUMENT`, `PAPER_DOCUMENT`, `PHOTOCOPY`,
`SCREEN_CAPTURE`, `SELFIE_WITH_PAPER_DOCUMENT`, `SUSPECTED_DOCUMENT`,
`SYSTEM_ABORT`.

Select the reason with the stunt-only `"simulate_reject_reason"` create-body
field (it implies failure, so `simulate_fail` is not needed alongside it);
`simulate_fail` alone defaults to `MANIPULATED_DOCUMENT`. The `/data` 409
body repeats the `rejectionReason`.

---

*Synthetic. No real Jumio data. See [DISCLAIMER](DISCLAIMER).*
