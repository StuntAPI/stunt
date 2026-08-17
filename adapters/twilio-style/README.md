# Twilio-style adapter

A stunt adapter for simulating a **Twilio REST API (2010-04-01)** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Twilio. "Twilio" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of Twilio's Programmable Messaging, Voice, and
Verify surfaces, designed for local integration testing without a real Twilio
account:

- **Send SMS/MMS:** `POST /2010-04-01/Accounts/{Sid}/Messages.json` (`{To, From, Body}`).
- **List messages:** `GET /2010-04-01/Accounts/{Sid}/Messages.json` (cursor-paginated
  via `PageSize` + `PageToken`, with a Twilio-style `next_page_uri`; filters
  `To`, `From`, `DateSent` (also `DateSent>`/`DateSent<` windows — queued
  messages with a null `date_sent` are excluded by date filters, like the
  real API)).
- **Retrieve message:** `GET .../Messages/{Sid}.json`.
- **Create call:** `POST /2010-04-01/Accounts/{Sid}/Calls.json` (`{To, From, Url}`).
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
AuthToken  = feed0000face1111beef2222cafe3333
```

Base64 of `AC0123456789abcdef0123456789abcdef:feed0000face1111beef2222cafe3333`:

```
QUMwMTIzNDU2Nzg5YWJjZGVmMDEyMzQ1Njc4OWFiY2RlZjp0d2lsaW9fYXV0aF90b2tlbg==
```

### Example

```bash
curl -u "AC0123456789abcdef0123456789abcdef:feed0000face1111beef2222cafe3333" \
  http://localhost:PORT/2010-04-01/Accounts/AC0123456789abcdef0123456789abcdef/Messages.json \
  -d 'To=+15551234567' \
  -d 'From=+15557654321' \
  -d 'Body=Hello from stunt'
```

### 401 without auth

```bash
curl http://localhost:PORT/2010-04-01/Accounts/AC.../Messages.json
# → 401 {"code":20003,"message":"Missing or invalid Basic Auth credentials",...}
```

## Message lifecycle (async status machine)

Real Twilio messages take time: `POST .../Messages.json` returns `status:
"queued"`, then the message moves to `sent` and finally `delivered`. This
adapter reproduces that with a **derive-on-read** state machine:

```
queued -> sent -> delivered          (timings: sent at +1s, delivered at +3s)
```

Every read (`GET .../Messages/{Sid}.json` and `GET .../Messages.json`)
derives the current status from the clock, **persists** the transition, and
fires the signed status-callback webhook **exactly once per new stage**
(`message.sent` on queued→sent; `message.delivered` at the terminal), so
polls, lists, and webhooks always agree. Integration tests can simply sleep
past the 3s window and poll again.

**Failure injection:**

- `To = +15005550001` — Twilio's *real* magic test number that always fails:
  terminal state `failed` (error code 21211, "Invalid 'To' Phone Number").
- `"simulate_fail": true` in the POST body — **simulator extension**: the
  terminal state becomes `undelivered` (error code 30007) and the success
  side effects are skipped; the `message.undelivered` callback still fires.

## Verify flow

The verification code is **deterministic** for local testing: the last 6 digits
of the `To` phone number, zero-padded to 6 digits. This lets you write realistic
verification round-trip tests:

1. `POST /v2/Services/{ServiceSid}/Verification` with `{To: "+15555123456"}`.
2. The expected code is the last 6 digits of `15555123456` → `123456`.
3. `POST /v2/Services/{ServiceSid}/VerificationCheck` with `{To: "+15555123456", Code: "123456"}`.
4. Response: `{"status":"approved"}`.

Wrong code → status stays `"pending"` (and `send_code_attempts` is
incremented). This matches Twilio's behaviour where the verification status
remains `"pending"` until a correct code is submitted. Checking a `To` with no
verification on record returns `404` (Twilio error `20404`).

## Webhooks

As a message advances through its [lifecycle](#message-lifecycle-async-status-machine),
the adapter emits a `message.sent` / `message.delivered` / `message.undelivered`
/ `message.failed` webhook event (fire-and-forget, once per transition) to the
registered webhook sink. See the stunt docs for webhook configuration
(`events_target`).

### Signed deliveries — `X-Twilio-Signature`

Webhook deliveries are signed exactly the way Twilio signs its webhook
requests — the header carries a base64 HMAC-SHA1 over the delivery URL plus
the raw request body:

```
X-Twilio-Signature = base64(HMAC-SHA1(key=feed0000face1111beef2222cafe3333,
                                      msg=events_target_url + raw_body))
```

The URL is the webhook destination configured as this service's
`events_target` (Twilio MACs the full request URL, so a receiver must validate
against the same URL stunt delivered to), and the body is the exact JSON
envelope on the wire. The signing key is the documented mock AuthToken:

```
feed0000face1111beef2222cafe3333
```

A receiver can therefore exercise real signature-verification code paths
(`crypto.hmac_sha1` + `events_target()` under the hood in `lib.star`'s
`_signed_emit`). See the signed-webhook roster in
[`../../README.md`](../../README.md) for the full list of adapters that sign
their deliveries and their mock secrets.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/2010-04-01/Accounts/{account_sid}/Messages.json` | `messages.star#on_send_message` | Send a message (→ `queued`) |
| GET | `/2010-04-01/Accounts/{account_sid}/Messages.json` | `messages.star#on_list_messages` | List messages (stateful, cursor-paginated) |
| GET | `/2010-04-01/Accounts/{account_sid}/Messages/{sid}.json` | `messages.star#on_get_message` | Retrieve a message |
| POST | `/2010-04-01/Accounts/{account_sid}/Calls.json` | `calls.star#on_create_call` | Create a call (→ `queued`) |
| POST | `/v2/Services/{service_sid}/Verification` | `verify.star#on_create_verification` | Start a verification |
| POST | `/v2/Services/{service_sid}/VerificationCheck` | `verify.star#on_check_verification` | Check a verification code |

Any unmatched route returns `404` with a Twilio-style error body.

### List pagination

The messages list endpoint supports Twilio-style cursor pagination built on
stunt's `paginate` builtin:

- `PageSize` — page size; omitted or `<= 0` returns all messages in one page
  (the reported default `page_size` is 50).
- `PageToken` — the opaque cursor carried in `next_page_uri` from a prior
  page; pass it back to fetch the next page.

The response includes `first_page_uri`, `next_page_uri` (present only when
more pages remain), `page`, `page_size`, and `previous_page_uri`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `messages` | Stateful SMS/message records |
| `calls` | Call records |
| `verifications` | Verification records (with expected code) |

KV is used for monotonic sequence counters (`SM_seq`, `CA_seq`, `VL_seq`, etc.).

## Shared library

Shared helpers (`_basic_auth`, `_require_auth`, `_next_sid`, `_b64decode`,
`_to_int`, `_list_page`, `_signed_emit`) are defined in `scripts/lib.star` and
preloaded into every handler script via stunt's `LoadWithLib` mechanism.
`_list_page` applies `PageSize`/`PageToken` paging via the `paginate` builtin;
`_signed_emit` emits webhook events with the `X-Twilio-Signature` header.

## Layout

```
adapter.yaml                    Manifest: endpoints, resources, rules, identity
DISCLAIMER                      Not affiliated / synthetic-only notice
README.md                       This file
scripts/
  lib.star                      Shared helpers (Basic auth, SID generation, base64,
                                pagination, signed webhook emits)
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
