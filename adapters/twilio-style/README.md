# Twilio-style adapter

A stunt adapter for simulating a **Twilio REST API (2010-06-01)** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Twilio. "Twilio" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of Twilio's Programmable Messaging, Voice, and
Verify surfaces, designed for local integration testing without a real Twilio
account:

- **Send SMS/MMS:** `POST /2010-06-01/Accounts/{Sid}/Messages.json` (`{To, From, Body}`).
- **List messages:** `GET /2010-06-01/Accounts/{Sid}/Messages.json` (paginated).
- **Retrieve message:** `GET .../Messages/{Sid}.json`.
- **Create call:** `POST /2010-06-01/Accounts/{Sid}/Calls.json` (`{To, From, Url}`).
- **Verify:** `POST /v2/Services/{ServiceSid}/Verification` → `{status:"pending"}`.
- **Verify check:** `POST /v2/Services/{ServiceSid}/VerificationCheck` (`{To, Code}`) → `{status:"approved"}` on correct code.

Messages are **stateful**: a message sent via POST appears in the GET list,
enabling round-trip testing locally.

## Auth — HTTP Basic

Twilio uses **HTTP Basic authentication** with the Account SID as the username
and the Auth Token as the password. This adapter **validates** the Basic auth
header:

- Checks for `Authorization: Basic <base64(AccountSid:AuthToken)>`.
- Decodes the base64 and splits on the first colon to extract SID + token.
- Compares against the synthetic test credentials.
- Returns `401` with a Twilio-style error body if missing or invalid.

### Synthetic test credentials

```
AccountSid = AC0123456789abcdef0123456789abcdef
AuthToken  = twilio_auth_token
```

Base64 of `AC0123456789abcdef0123456789abcdef:twilio_auth_token`:

```
QUMwMTIzNDU2Nzg5YWJjZGVmMDEyMzQ1Njc4OWFiY2RlZjp0d2lsaW9fYXV0aF90b2tlbg==
```

### Example

```bash
curl -u "AC0123456789abcdef0123456789abcdef:twilio_auth_token" \
  http://localhost:PORT/2010-06-01/Accounts/AC0123456789abcdef0123456789abcdef/Messages.json \
  -d 'To=+15551234567' \
  -d 'From=+15557654321' \
  -d 'Body=Hello from stunt'
```

### 401 without auth

```bash
curl http://localhost:PORT/2010-06-01/Accounts/AC.../Messages.json
# → 401 {"code":20003,"message":"Missing or invalid Basic Auth credentials",...}
```

## Verify flow

The verification code is **deterministic** for local testing: the last 6 digits
of the `To` phone number, zero-padded to 6 digits. This lets you write realistic
verification round-trip tests:

1. `POST /v2/Services/{ServiceSid}/Verification` with `{To: "+15555123456"}`.
2. The expected code is the last 6 digits of `15555123456` → `123456`.
3. `POST /v2/Services/{ServiceSid}/VerificationCheck` with `{To: "+15555123456", Code: "123456"}`.
4. Response: `{"status":"approved"}`.

Wrong code → status stays `"pending"`. This matches Twilio's behaviour where
the verification status remains `"pending"` until a correct code is submitted.

## Webhooks

When a message is sent, the adapter emits a `message.sent` webhook event
(fire-and-forget) to any registered webhook sink. See the stunt docs for
webhook configuration.

### Real Twilio webhook signing (documented for reference)

The real Twilio API signs webhook requests with an **`X-Twilio-Signature`**
header:

```
X-Twilio-Signature = HMAC-SHA256(key=AuthToken, msg=url + sorted_params)
```

where `url` is the full callback URL (including scheme, host, and query
string) and `sorted_params` is the URL-encoded POST parameters sorted by key
and concatenated as `keyvalue` pairs. The HMAC output is base64-encoded.

This stunt adapter validates **inbound** Basic auth (above) and emits webhook
events via the standard stunt events envelope. The outbound signing scheme is
documented here for reference; it is **not** applied to the synthetic webhook
payload because the events primitive sends a fixed JSON envelope without
custom headers.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/2010-06-01/Accounts/{account_sid}/Messages.json` | `messages.star#on_send_message` | Send a message (→ `queued`) |
| GET | `/2010-06-01/Accounts/{account_sid}/Messages.json` | `messages.star#on_list_messages` | List messages (stateful, paginated) |
| GET | `/2010-06-01/Accounts/{account_sid}/Messages/{sid}.json` | `messages.star#on_get_message` | Retrieve a message |
| POST | `/2010-06-01/Accounts/{account_sid}/Calls.json` | `calls.star#on_create_call` | Create a call (→ `queued`) |
| POST | `/v2/Services/{service_sid}/Verification` | `verify.star#on_create_verification` | Start a verification |
| POST | `/v2/Services/{service_sid}/VerificationCheck` | `verify.star#on_check_verification` | Check a verification code |

Any unmatched route returns `404` with a Twilio-style error body.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `messages` | Stateful SMS/message records |
| `calls` | Call records |
| `verifications` | Verification records (with expected code) |

KV is used for monotonic sequence counters (`SM_seq`, `CA_seq`, `VL_seq`, etc.).

## Shared library

Shared helpers (`_basic_auth`, `_require_auth`, `_next_sid`, `_b64decode`,
`_to_int`) are defined in `scripts/lib.star` and preloaded into every handler
script via stunt's `LoadWithLib` mechanism.

## Layout

```
adapter.yaml                    Manifest: endpoints, resources, rules, identity
DISCLAIMER                      Not affiliated / synthetic-only notice
README.md                       This file
scripts/
  lib.star                      Shared helpers (Basic auth, SID generation, base64)
  messages.star                 Messages: send, list, retrieve
  calls.star                    Calls: create
  verify.star                   Verify v2: create + check
```

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  twilio:
    adapter: ./adapters/twilio-style
```

Then `stunt up` and make requests to the served address.
