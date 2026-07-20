# Open Banking / PSD2 (Berlin Group NextGenPSD2) API simulator

A local development and testing simulator that mimics the **structure** of the
Berlin Group NextGenPSD2 API (version `1.3.6`). It does **not** call any real
bank API — all data is synthetic.

This adapter lets you test the full **consent → SCA redirect → account access**
flow without a real bank login page.

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
  -d '{"access":{"accounts":[],"balances":[],"transactions":[]},"recurringIndicator":true,"validUntil":"2025-12-31","frequencyPerDay":4}' \
  | jq -r .consentId)

# 3. Start authorisation (SCA flow)
AUTH=$(curl -s -X POST http://localhost:8080/v1/consents/$CONSENT/authorisations \
  -H "Authorization: Bearer $TOKEN" \
  | jq -r .authorisationId)

# 4. Finalise SCA (simulate PSU completing the bank login)
curl -X PUT http://localhost:8080/v1/consents/$CONSENT/authorisations/$AUTH \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"authenticationMethodId":"901","scaAuthenticationData":"123456"}'

# 5. Access accounts (now consent is valid)
curl http://localhost:8080/v1/accounts \
  -H "Authorization: Bearer $TOKEN"

# 6. Get balances
curl http://localhost:8080/v1/accounts/acc-001/balances \
  -H "Authorization: Bearer $TOKEN"

# 7. Get transactions
curl http://localhost:8080/v1/accounts/acc-001/transactions \
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

3. GET /v1/consents/{id}/authorisations/{authId}
   → scaStatus:"started" (the PSU would authenticate at the bank page)

4. PUT /v1/consents/{id}/authorisations/{authId}
   → scaStatus:"finalised", consentStatus becomes "valid"

5. GET /v1/accounts, /v1/accounts/{id}/balances, /v1/accounts/{id}/transactions
   (now accessible with a valid consent)
```

### SCA status lifecycle

```
started → psuAuthenticated → finalised
```

## Authentication

- **TPP-level**: OAuth2 client-credentials bearer token for consent management
- **Account access**: bearer token + valid consent (consent must have `consentStatus: "valid"`)

Missing the bearer token returns `401` with a `tppMessages` error.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/v1/oauth/token` | OAuth2 client-credentials token |
| POST | `/v1/consents` | Create a consent |
| GET | `/v1/consents/{consentId}` | Get consent status |
| DELETE | `/v1/consents/{consentId}` | Terminate consent |
| POST | `/v1/consents/{consentId}/authorisations` | Start SCA authorisation |
| GET | `/v1/consents/{consentId}/authorisations/{authorisationId}` | Get SCA status |
| PUT | `/v1/consents/{consentId}/authorisations/{authorisationId}` | Update SCA (finalise) |
| GET | `/v1/accounts` | List accounts |
| GET | `/v1/accounts/{resourceId}/balances` | Get account balances |
| GET | `/v1/accounts/{resourceId}/transactions` | Get account transactions |

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
