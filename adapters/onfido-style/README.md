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
| `/v3.6/checks/{id}` | GET | Get check — in_progress→complete |
| `/v3.6/webhooks` | POST | Webhook receiver (X-SHA2-Signature) |

## Auth

Token auth (`Authorization: Token <key>`).

## API version

`v3.6`

## Check lifecycle

```
in_progress → complete (result: clear|consider)
```

First GET after creation completes the check with result "clear".

---

*Synthetic. No real Onfido data. See [DISCLAIMER](DISCLAIMER).*
