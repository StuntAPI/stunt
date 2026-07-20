# Braintree GraphQL + REST API simulator

A local development and testing simulator that mimics the **structure** of the
Braintree GraphQL + REST API (version `2024-09-01`). It does **not** call the
real Braintree API — all data is synthetic.

## Quick start

```bash
stunt plan --add braintree-style --port 8080
stunt up
```

```bash
# GraphQL — create a customer
curl -X POST http://localhost:8080/graphql \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{
    "query": "mutation($input: CreateCustomerInput!) { createCustomer(input: $input) { customer { id email } } }",
    "variables": { "input": { "firstName": "John", "lastName": "Doe", "email": "john@example.com" } }
  }'

# GraphQL — charge a payment method
curl -X POST http://localhost:8080/graphql \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{
    "query": "mutation($input: ChargePaymentMethodInput!) { chargePaymentMethod(input: $input) { transaction { id status amount } } }",
    "variables": { "input": { "paymentMethodId": "pm-token", "amount": "50.00" } }
  }'

# REST — create a transaction
curl -X POST http://localhost:8080/merchants/merchant123/transactions \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{ "amount": "100.00", "type": "sale", "paymentMethodNonce": "fake-nonce" }'

# Generate a client token
curl -X POST http://localhost:8080/merchants/merchant123/client_token \
  -H "Authorization: Bearer your-token"
```

## Auth

Braintree accepts either:
- `Authorization: Bearer <token>` header
- HTTP Basic auth (public_key:private_key)

Requests without auth return `401`.

## GraphQL Operations

| Mutation | Description |
|----------|-------------|
| `createCustomer` | Create a customer |
| `chargePaymentMethod` / `chargeCreditCard` | Charge → settled |
| `authorizePaymentMethod` / `authorizeCreditCard` | Authorize → authorized |
| `refundTransaction` | Refund a transaction |
| `voidTransaction` | Void a transaction |
| `searchTransactions` | Search transactions |

## REST Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/graphql` | GraphQL endpoint |
| POST | `/merchants/{id}/transactions` | Create transaction |
| GET | `/merchants/{id}/transactions/{id}` | Get transaction |
| POST | `/merchants/{id}/transactions/{id}/refund` | Refund |
| POST | `/merchants/{id}/payment_methods` | Vault payment method |
| POST | `/merchants/{id}/client_token` | Generate client token |
| POST | `/webhooks` | Inbound webhook |

## Webhooks

Braintree webhook signature scheme:
```
bt_signature: "public_key|signature_hex"
bt_payload:   base64-encoded payload
```
Verification: `signature_hex = hex(HMAC-SHA256(private_key, bt_payload))`

## Response shapes

```json
// GraphQL
{ "data": { "chargePaymentMethod": { "transaction": { "id": "ta000001", "status": "submitted_for_settlement" } } } }

// REST
{ "transaction": { "id": "ta000001", "status": "authorized", "amount": "100.00" } }
```
