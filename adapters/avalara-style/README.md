# Avalara AvaTax REST API simulator

A local development and testing simulator that mimics the **structure** of the
Avalara AvaTax REST API (version `2`). It does **not** call the real Avalara API —
all data is synthetic.

## Quick start

```bash
stunt plan --add avalara-style --port 8080
stunt up
```

```bash
# Quick tax calculation
curl -X POST http://localhost:8080/v2/tax/calculate \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{
    "addresses": {
      "singleLocation": { "line1": "100 Main St", "city": "San Francisco", "region": "CA", "country": "US", "postalCode": "94016" }
    },
    "lines": [
      { "number": "1", "quantity": 1, "amount": "100.00", "taxCode": "P0000000", "description": "Widget" }
    ]
  }'

# Create a transaction
curl -X POST http://localhost:8080/v2/transactions/create \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{
    "companyCode": "DEFAULT",
    "type": "SalesInvoice",
    "date": "2024-06-15",
    "customerCode": "CUST001",
    "addresses": {
      "singleLocation": { "line1": "100 Main St", "city": "San Francisco", "region": "CA", "country": "US", "postalCode": "94016" }
    },
    "lines": [
      { "number": "1", "quantity": 1, "amount": "100.00", "taxCode": "P0000000", "description": "Widget" }
    ]
  }'

# List nexus
curl http://localhost:8080/v2/definitions/nexuses \
  -H "Authorization: Bearer your-token"
```

## Auth

Avalara accepts either:
- `Authorization: Bearer <account/license key>`
- HTTP Basic auth

Requests without auth return `401`.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/v2/tax/calculate` | Quick tax estimate (jurisdiction breakdown) |
| POST | `/v2/transactions/create` | Create a transaction |
| GET | `/v2/transactions` | List transactions |
| GET | `/v2/transactions/{id}` | Get transaction |
| POST | `/v2/transactions/{id}/void` | Void transaction |
| GET | `/v2/companies` | List companies |
| GET | `/v2/definitions/nexuses` | List nexus (tax obligation) |
| GET | `/v2/definitions/taxcodes` | List tax codes |

## Tax calculation model

The effective rate is deterministically split into jurisdiction components:
- **State** (~50% of effective rate)
- **County** (~25%)
- **City** (~20%)
- **Special** (~5%)

Each line's `details` array shows the per-jurisdiction breakdown (rate + tax).

## Response shapes

```json
// Tax calculation
{
  "totalTax": "9.5",
  "totalTaxable": "100.0",
  "totalRate": 0.095,
  "lines": [{
    "number": "1",
    "tax": "9.5",
    "details": [
      { "jurisdiction": "CA", "jurisdictionType": "State", "rate": 0.0475, "tax": "4.75" },
      { "jurisdiction": "CA County", "jurisdictionType": "County", "rate": 0.0238, "tax": "2.38" },
      ...
    ]
  }],
  "summary": [{ "jurisName": "CA", "jurisdictionType": "State", "rate": 0.0475, "tax": "4.75" }]
}

// Error
{ "error": { "code": "AuthenticationRequired", "message": "...", "target": "", "details": [] } }
```
