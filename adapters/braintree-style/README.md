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

## Idempotency

`POST /merchants/{id}/transactions` honors the `Idempotency-Key` header: a
create carrying the header remembers its response, and a retry with the same
key replays the original transaction response instead of creating a duplicate.
Keys are scoped by method + path + collection, so a reused key never collides
across endpoints. This makes client retry/idempotency logic testable.

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
| POST | `/webhooks` | Register webhook (`{url, kinds}`) or inbound notification verification (`{bt_signature, bt_payload}`) |

## Webhooks

### Registration

Real Braintree webhooks are configured in the Control Panel (no public REST
registration endpoint), so the simulator exposes registration on the webhook
path itself:

```bash
curl -X POST http://localhost:8080/webhooks \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{ "url": "http://localhost:9090/braintree/webhook", "kinds": ["transaction_settled", "refund_opened"] }'
```

Registering stores the hook `{id, url, kinds}` and immediately delivers a
signed `check` notification (the kind Braintree uses to verify a webhook
URL). An empty `kinds` list subscribes to everything.

### Outbound signature (bt_signature / bt_payload)

Real Braintree delivers notifications as a form-encoded POST with two fields;
the engine POSTs JSON, so stunt delivers the same values as a JSON body and
duplicates them as headers:

| Where | Field | Value |
|-------|-------|-------|
| body + header | `bt_signature` | `"<public_key>\|<hex(HMAC-SHA1(private_key, bt_payload))>"` |
| body + header | `bt_payload` | base64-encoded notification payload |
| header | `bt-hash` | the hex HMAC-SHA1 over `bt_payload` |
| header | `bt-kind` | the notification kind |

The notification payload (base64'd into `bt_payload`) is
`{timestamp, kind, subject}`. Mock keys — configure your verifier with these
exact strings (public + low-entropy, local stunt only):

- public key: `stunt_mock_public_key_2026`
- private key: `stunt_mock_private_key_2026`

### Emitted kinds

| Kind | Emitted when |
|------|--------------|
| `check` | Webhook registered |
| `transaction_settled` | `POST /merchants/{id}/transactions` (type `sale`) or GraphQL `chargePaymentMethod`/`chargeCreditCard` — the simulator settles immediately |
| `refund_opened` | REST refund or GraphQL `refundTransaction` |

(`authorizePaymentMethod` emits nothing — real Braintree has no webhook kind
for a bare authorization.)

### Inbound verification

`POST /webhooks` with `{bt_signature, bt_payload}` still behaves as the
inbound verification endpoint: any non-empty pair returns `200`, a missing
field returns `400`.

## Response shapes

```json
// GraphQL
{ "data": { "chargePaymentMethod": { "transaction": { "id": "ta000001", "status": "submitted_for_settlement" } } } }

// REST
{ "transaction": { "id": "ta000001", "status": "authorized", "amount": "100.00" } }
```
