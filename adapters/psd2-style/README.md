# Open Banking / PSD2 (Berlin Group NextGenPSD2) API simulator

A local development and testing simulator that mimics the **structure** of the
Berlin Group NextGenPSD2 API (version `1.3.6`). It does **not** call any real
bank API — all data is synthetic.

This adapter lets you test the full **consent → SCA redirect → account access**
and **payment initiation (PIS)** flows without a real bank login page.

## Quick start

```bash
stunt plan --add psd2-style --port 8080
stunt up
```

```bash
# 1. Get a TPP access token
TOKEN=$(curl -s -X POST http://localhost:8080/v1/oauth/token \
  -H "Content-Type: application/json" \
  -d '{"grant_type":"client_credentials","client_id":"tpp","client_secret":"secret"}' \
  | jq -r .access_token)

# 2. Create consent
CONSENT=$(curl -s -X POST http://localhost:8080/v1/consents \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"access":{"accounts":[],"balances":[],"transactions":[]},"recurringIndicator":true,"validUntil":"2027-12-31","frequencyPerDay":4}' \
  | jq -r .consentId)

# 3. Start authorisation (SCA flow)
AUTH=$(curl -s -X POST http://localhost:8080/v1/consents/$CONSENT/authorisations \
  -H "Authorization: Bearer $TOKEN" \
  | jq -r .authorisationId)

# 4. Walk the staged SCA chain: select a method (-> psuAuthenticated),
#    then submit the OTP (-> scaReceived)
curl -X PUT http://localhost:8080/v1/consents/$CONSENT/authorisations/$AUTH \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"authenticationMethodId":"901"}'
curl -X PUT http://localhost:8080/v1/consents/$CONSENT/authorisations/$AUTH \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"scaAuthenticationData":"123456"}'

# 5. SCA finalises derive-on-read once the 1s challenge window elapses;
#    reading the authorisation (or the consent-bound account endpoints)
#    advances it to "finalised" and makes the consent "valid"
sleep 2
curl http://localhost:8080/v1/consents/$CONSENT/authorisations/$AUTH \
  -H "Authorization: Bearer $TOKEN"

# 6. Access accounts (now consent is valid)
curl http://localhost:8080/v1/accounts \
  -H "Authorization: Bearer $TOKEN"

# 7. Get balances
curl http://localhost:8080/v1/accounts/acc-001/balances \
  -H "Authorization: Bearer $TOKEN"

# 8. Get transactions (bookingStatus and dateFrom are required)
curl "http://localhost:8080/v1/accounts/acc-001/transactions?bookingStatus=booked&dateFrom=2024-01-01" \
  -H "Authorization: Bearer $TOKEN"

# 9. Initiate a SEPA payment, then watch the status progress
PAYMENT=$(curl -s -X POST http://localhost:8080/v1/payments/sepa-credit-transfers \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"instructedAmount":{"currency":"EUR","amount":"42.00"},"debtorAccount":{"iban":"DEZZTEST0AA0BB0CC0D01"},"creditorAccount":{"iban":"DEZZTEST0AA0BB0CC0D99"},"creditorName":"Wire Beneficiary"}' \
  | jq -r .paymentId)
curl http://localhost:8080/v1/payments/sepa-credit-transfers/$PAYMENT/status \
  -H "Authorization: Bearer $TOKEN"
```

## The consent + SCA redirect flow

This is the core PSD2 pain point — the PSU (Payment Service User / end
customer) must authenticate at their bank via **Strong Customer Authentication
(SCA)**. The flow:

```
1. POST /v1/consents
   → consentId, consentStatus:"received", _links.startAuthorisation

2. POST /v1/consents/{id}/authorisations
   → authorisationId, scaStatus:"started", _links.scaRedirect
   (the scaRedirect URL points to the bank's SCA login page — in this mock,
    it's a synthetic URL)

3. PUT /v1/consents/{id}/authorisations/{authId}
   → one hop per call: {"authenticationMethodId":"901"} → "psuAuthenticated",
     {"scaAuthenticationData":"123456"} → "scaReceived".
     An update carrying neither field is a 400 PARAMETER_INVALID and leaves
     the chain where it is — no jumping straight to the terminal state.

4. GET /v1/consents/{id}/authorisations/{authId} (after the 1s challenge
   window) → scaStatus:"finalised" (derive-on-read); the consent becomes
   "valid" at the same moment. Reading any consent-bound endpoint derives
   the same transition.

5. GET /v1/accounts, /v1/accounts/{id}/balances, /v1/accounts/{id}/transactions
   (now accessible with a valid consent)
```

### SCA status lifecycle

```
started → psuAuthenticated → scaReceived → finalised
          (PUT, method)      (PUT, OTP)    (derive-on-read after the
                                            1s challenge window)
```

Payment authorisations (`/v1/payments/{product}/{paymentId}/authorisations`)
run the exact same staged chain; finalising one unlocks the payment's
accepted → settled progression.

## Consent-bound access (AIS)

Account, balance and transaction reads require a **valid, unexpired AIS
consent covering that account**:

- The request MAY select the authorising consent with a `Consent-ID` header.
  An unknown consent is `400 CONSENT_INVALID`; a consent that is not (yet)
  `valid` is `401 CONSENT_INVALID`; a consent past its `validUntil` is
  `401 CONSENT_EXPIRED`.
- Without the header, any valid, unexpired consent grants access; if only
  expired ones exist you get `401 CONSENT_EXPIRED`, otherwise
  `401 CONSENT_INVALID`.
- A non-empty `access.{accounts,balances,transactions}` IBAN list restricts
  reads to those IBANs (empty list = the "all accounts" grant). Reads of an
  uncovered account are `404 RESOURCE_UNKNOWN`, and `GET /v1/accounts` lists
  only the covered accounts.

## Payment initiation (PIS)

`POST /v1/payments/{product}` with `product` one of `sepa-credit-transfers`,
`instant-credit-transfers` or `target-2-payments` (unknown product →
`400 PRODUCT_INVALID`; missing `instructedAmount` or `creditorAccount.iban`
→ `400 FORMAT_ERROR`). The response carries `paymentId` and
`transactionStatus:"RCVD"`.

When the request references a consent (via the `Consent-ID` header or a
`consentId` body field) that consent must exist, be `valid` and be unexpired
(unknown/not-yet-valid → `400 CONSENT_INVALID`, expired →
`401 CONSENT_EXPIRED`). Without a consent reference the implicit
authorisation flow applies: the payment starts its own SCA sub-resource.

### Payment status lifecycle (derive-on-read)

```
RCVD → ACTC → ACSC     (accepted → settlement completed; 1s / 3s)
RCVD → RJCT            (simulate_fail: true in the POST body — simulator-only)
RCVD/ACTC → CANC       (DELETE before the payment goes terminal)
```

Every read of the payment, its `/status` sub-resource or its authorisation
chain derives the current state from the clock, persists each transition and
emits the signed webhook below exactly once per NEW status. `DELETE` on a
terminal payment (ACSC/RJCT/CANC) is `400 PRODUCT_INVALID`; a successful
cancellation is `204 No Content`.

## Authentication

- **TPP-level**: OAuth2 client-credentials bearer token for consent management
- **Account access**: bearer token + valid consent (consent must have `consentStatus: "valid"`)

Bearer tokens are validated: the token must be one minted by
`POST /v1/oauth/token` (stored in the `access_tokens` collection with an
`expires_at` unix timestamp, TTL `3600`s matching the response `expires_in`)
and unexpired. A missing, unknown or expired token returns `401` with a
`tppMessages` error (`TOKEN_INVALID` for missing/unknown, `TOKEN_EXPIRED`
for expired).

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/v1/oauth/token` | OAuth2 client-credentials token |
| POST | `/v1/consents` | Create a consent |
| GET | `/v1/consents/{consentId}` | Get consent status |
| DELETE | `/v1/consents/{consentId}` | Terminate consent |
| POST | `/v1/consents/{consentId}/authorisations` | Start SCA authorisation |
| GET | `/v1/consents/{consentId}/authorisations/{authorisationId}` | Get SCA status (derive-on-read finalisation) |
| PUT/POST | `/v1/consents/{consentId}/authorisations/{authorisationId}` | Advance SCA one hop (method → `psuAuthenticated`, OTP → `scaReceived`) |
| GET | `/v1/accounts` | List accounts (honors `withBalance`, `page`/`size` paging; scoped by the covering consent) |
| GET | `/v1/accounts/{resourceId}/balances` | Get account balances (consent must cover the IBAN) |
| GET | `/v1/accounts/{resourceId}/transactions` | Get account transactions (`bookingStatus` and `dateFrom` required — missing either returns `400` with `PARAMETER_MISSING-BOOKINGSTATUS` / `PARAMETER_MISSING-DATEFROM`; `dateTo` optional) |
| POST | `/v1/payments/{product}` | Initiate a payment (`sepa-credit-transfers` / `instant-credit-transfers` / `target-2-payments`) |
| GET | `/v1/payments/{product}/{paymentId}` | Payment details (derive-on-read status) |
| GET | `/v1/payments/{product}/{paymentId}/status` | `transactionStatus` only |
| DELETE | `/v1/payments/{product}/{paymentId}` | Cancel a non-terminal payment (`204`) |
| POST | `/v1/payments/{product}/{paymentId}/authorisations` | Start the payment's SCA authorisation |
| GET | `/v1/payments/{product}/{paymentId}/authorisations/{authorisationId}` | Payment SCA status (derive-on-read) |
| PUT/POST | `/v1/payments/{product}/{paymentId}/authorisations/{authorisationId}` | Advance payment SCA one hop |

## Webhooks

State transitions emit signed webhooks (HMAC-SHA256 over the exact body,
delivered as `X-Stunt-Signature: sha256=<hex>`):

- `consent.status.changed` — when a consent's SCA authorisation finalises and
  the consent becomes `valid`
- `payment.status.changed` — once per NEW `transactionStatus`
  (ACTC / ACSC / RJCT / CANC)

The signing secret is `psd2-style-webhook-secret` (simulator extension —
swap it in `scripts/lib.star` for your own harness).

## Seeded data

Two synthetic bank accounts (obviously-fake test IBANs):
- `acc-001`: DEZZTEST0AA0BB0CC0D01 (EUR, "Main Account")
- `acc-002`: DEZZTEST0AA0BB0CC0D02 (EUR, "Savings Account")

Three seeded transactions on `acc-001`.

## Error responses

NextGenPSD2 wraps errors in `tppMessages`:

```json
{
  "tppMessages": [
    {
      "category": "ERROR",
      "code": "CONSENT_INVALID",
      "text": "Missing or invalid access token"
    }
  ]
}
```

## Disclaimer

See [DISCLAIMER](DISCLAIMER). This is not affiliated with or endorsed by any
bank or the Berlin Group.
