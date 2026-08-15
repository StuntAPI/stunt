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

## Webhook verification

Both directions use Jumio's scheme:

```
X-Jumio-Webhook-Signature: <hex(HMAC-SHA256(secret, raw_body))>
```

- **Outbound** (signed delivery): at a scan's terminal transition the adapter
  emits a `scan.completed` (or `scan.failed`) event signed exactly the way
  Jumio signs its webhooks.
- **Inbound** (receiver): `POST /netverify/v2/webhooks` is the local stand-in
  for YOUR webhook endpoint. It verifies the MAC for real — recomputing
  HMAC-SHA256 over the exact bytes on the wire (`req.raw_body`, never a
  re-serialized copy) — and returns **401** (`{"httpStatus": 401, ...}`) when
  the header is missing or the signature does not match.

The signing secret is the fixed synthetic constant documented here and in
`scripts/lib.star`:

```
stunt_jumio_mock_signing_key
```

Public + low-entropy: local stunt only. Tests and receivers compute the same
MAC with it:

```go
mac := hmac.New(sha256.New, []byte("stunt_jumio_mock_signing_key"))
mac.Write(rawBody) // verbatim request bytes
expected := hex.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Jumio-Webhook-Signature"))) {
    // 401
}
```

---

*Synthetic. No real Jumio data. See [DISCLAIMER](DISCLAIMER).*
