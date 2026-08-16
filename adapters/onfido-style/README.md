# onfido-style

Onfido API simulator (unofficial) for local testing.

## Pain point

Onfido's KYC flow is **asynchronous**: you create an applicant, upload documents + selfie,
then submit a check. The check runs in_progress until it completes, and you receive a
webhook when done. Managing the async lifecycle + webhook verification is the core pain.

## What it simulates

| Endpoint | Method | Description |
|---|---|---|
| `/v3.6/applicants` | POST | Create applicant |
| `/v3.6/applicants/{id}` | GET | Get applicant |
| `/v3.6/documents` | POST | Upload document (multipart) |
| `/v3.6/live_photos` | POST | Upload selfie (multipart) |
| `/v3.6/checks` | POST | Create check (report_names) |
| `/v3.6/checks/{id}` | GET | Get check — in_progress→complete (derive-on-read) |
| `/v3.6/webhooks` | POST | Webhook receiver (X-SHA2-Signature) |

## Auth

Token auth (`Authorization: Token <key>`).

## API version

`v3.6`

## Check lifecycle

```
in_progress (0-3s after create) → complete (+3s, result: clear|consider)
```

The check is a real async state machine: status is derived from the clock on
each GET and persisted. With documents already uploaded (the flow above), a
freshly created check reports `in_progress` for the first ~3s (Onfido's
`awaiting_applicant` phase is skipped), then
transitions to `complete` with result `clear`. When the transition to
`complete` first fires, the adapter emits a `check.completed` webhook signed
with `X-SHA2-Signature` (HMAC-SHA256 hex over the raw body) — the same
transition Onfido notifies.

### Failure injection (simulator extension)

Pass `"simulate_fail": true` in the `POST /v3.6/checks` body: the check still
completes at +3s but with result `consider` (Onfido's flagged outcome) instead
of `clear`, and each report breakdown reports `consider`. This is a stunt-only
flag — Onfido's real sandbox drives `consider` via special sandbox documents.

## Webhook verification

Both directions use Onfido's scheme:

```
X-SHA2-Signature: <hex(HMAC-SHA256(webhook_token, raw_body))>
```

- **Outbound** (signed delivery): when a check first transitions to
  `complete`, the adapter emits a `check.completed` event signed exactly the
  way Onfido signs its webhooks.
- **Inbound** (receiver): `POST /v3.6/webhooks` is the local stand-in for
  YOUR webhook endpoint. It verifies the MAC for real — recomputing
  HMAC-SHA256 over the exact bytes on the wire (`req.raw_body`, never a
  re-serialized copy) — and returns **401** (`authorization_error`) when the
  header is missing or the signature does not match.

The signing token is the fixed synthetic secret documented here and in
`scripts/lib.star`:

```
stunt_onfido_mock_signing_key
```

Public + low-entropy: local stunt only. Tests and receivers compute the same
MAC with it:

```go
mac := hmac.New(sha256.New, []byte("stunt_onfido_mock_signing_key"))
mac.Write(rawBody) // verbatim request bytes
expected := hex.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-SHA2-Signature"))) {
    // 401
}
```

---

*Synthetic. No real Onfido data. See [DISCLAIMER](DISCLAIMER).*
