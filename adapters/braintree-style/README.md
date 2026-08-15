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

# GraphQL — charge a payment method (created submitted_for_settlement,
# settles 3s later, derived on read)
curl -X POST http://localhost:8080/graphql \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{
    "query": "mutation($input: ChargePaymentMethodInput!) { chargePaymentMethod(input: $input) { transaction { id status amount } } }",
    "variables": { "input": { "paymentMethodId": "pm-token", "amount": "50.00" } }
  }'

# REST — create a transaction (authorized; amount must be positive)
curl -X POST http://localhost:8080/merchants/merchant123/transactions \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{ "amount": "100.00", "type": "sale", "paymentMethodNonce": "fake-nonce" }'

# REST — capture it, wait 3s, read it back settled, then refund it
curl -X POST http://localhost:8080/merchants/merchant123/transactions/<id>/settle \
  -H "Authorization: Bearer your-token" -H "Content-Type: application/json" -d '{}'
sleep 3
curl http://localhost:8080/merchants/merchant123/transactions/<id> \
  -H "Authorization: Bearer your-token"
curl -X POST http://localhost:8080/merchants/merchant123/transactions/<id>/refund \
  -H "Authorization: Bearer your-token" -H "Content-Type: application/json" -d '{}'

# Generate a client token
curl -X POST http://localhost:8080/merchants/merchant123/client_token \
  -H "Authorization: Bearer your-token"
```

## Auth

Braintree accepts either:
- `Authorization: Bearer <token>` header
- HTTP Basic auth (public_key:private_key)

Requests without auth return `401`.

## Transaction lifecycle (derive-on-read)

The simulator implements the real state machine with compressed timings so
clients can watch a transaction move:

```
authorized -> submitted_for_settlement (+1s) -> settled (+3s)
```

- `POST /merchants/{id}/transactions` with `options.submit_for_settlement:
  true` creates the sale `authorized`, then it auto-advances on its own.
- Without the option the transaction stays `authorized` until you
  `POST .../settle` (capture) or `POST .../void`.
- GraphQL `chargePaymentMethod`/`chargeCreditCard` creates the transaction
  already `submitted_for_settlement` (like a real charge) and settles at +3s;
  `authorizePaymentMethod`/`authorizeCreditCard` creates `authorized`.
- Every read derives the current stage from the clock, persists the
  transition, and fires the signed `transaction_settled` webhook exactly once
  per new state (`submitted_for_settlement` has no real webhook kind, so
  nothing fires for it).

Terminal states:

| Status | Reached via |
|--------|-------------|
| `settled` | settlement completing (+3s) |
| `voided` | `POST .../void` — legal **only** from `authorized` |
| `authorization_expired` | an authorization left uncaptured past its window (7 simulated days; pass the simulator-only `simulate_authorization_expiry: true` on create to expire after 1s instead) |

Transition guards (all `422` with real Braintree error codes):

| Operation | Legal from | Error code |
|-----------|-----------|------------|
| void | `authorized` only | `91506` |
| settle (capture) | `authorized` or `submitted_for_settlement` | `91510` |
| refund | `settled` only | `91507` |
| any amount | must be `> 0` | `81501` |

`POST .../settle` takes an optional `amount` for a **partial capture** — it
must be positive and cannot exceed the transaction amount (`91522`).

Refunds (`POST .../refund`, optional `amount` defaulting to the full
unrefunded balance) enforce the over-refund guard: the sum of refunds never
exceeds the original amount (`91521`). A refund of a nonexistent transaction
returns `404`. The REST error envelope is
`{"error": {"code": ..., "message": ...}}`; GraphQL failures use the GraphQL
`{data: null, errors: [...]}` envelope.

## Transaction search

`POST /merchants/{id}/transactions/advanced_search` accepts the real
search-criteria vocabulary — a bare value (`is`), a list (`in`), or an
operator object — mapped onto typed filtering:

```json
{
  "search": {
    "status": { "in": ["settled", "submitted_for_settlement"] },
    "amount": { "min": "50.00", "max": "150.00" },
    "id": "ta00001",
    "created_at": { "min": "2026-01-01T00:00:00Z" }
  }
}
```

Supported criteria: `id`, `status`, `type`, `amount`, `currency`,
`created_at`/`createdAt`, `customer_id`/`customerId`, and
`credit_card_number` (`ends_with` — matches the card last 4). The response is
`{"transactions": [...], "total_count": n}`. GraphQL `searchTransactions`
accepts the same criteria via `variables.search`.

## Subscriptions (recurring billing)

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/merchants/{id}/plans` | Create a plan (`{id?, name, price, number_of_billing_cycles?}`) |
| GET | `/merchants/{id}/plans` | List plans |
| GET | `/merchants/{id}/plans/{planId}` | Fetch a plan |
| POST | `/merchants/{id}/subscriptions` | Subscribe (`{plan_id, payment_method_token?, number_of_billing_cycles?}`) |
| GET | `/merchants/{id}/subscriptions/{subId}` | Fetch a subscription |
| POST | `/merchants/{id}/subscriptions/{subId}/cancel` | Cancel (Active only) |

Subscription lifecycle (derive-on-read, compressed): `Active` runs one
billing cycle every **2 seconds**; after `number_of_billing_cycles` cycles it
becomes `Expired`. `POST .../cancel` moves an `Active` subscription to
`Canceled` immediately and terminally (canceling a non-Active subscription is
`422`, code `81902`). Each completed cycle fires a signed
`subscription_charged_successfully` notification; the final cycle also fires
`subscription_expired`, and a cancel fires `subscription_cancelled`
(Braintree's British spelling).

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
| `chargePaymentMethod` / `chargeCreditCard` | Charge → `submitted_for_settlement`, settles at +3s |
| `authorizePaymentMethod` / `authorizeCreditCard` | Authorize → `authorized` |
| `refundTransaction` | Refund a settled transaction (over-refund guarded) |
| `voidTransaction` | Void an authorized transaction |
| `searchTransactions` | Search with the REST search-criteria vocabulary |

## REST Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/graphql` | GraphQL endpoint |
| POST | `/merchants/{id}/transactions` | Create transaction |
| POST | `/merchants/{id}/transactions/advanced_search` | Search transactions |
| GET | `/merchants/{id}/transactions/{id}` | Get transaction |
| POST | `/merchants/{id}/transactions/{id}/settle` | Submit for settlement / capture |
| POST | `/merchants/{id}/transactions/{id}/void` | Void |
| POST | `/merchants/{id}/transactions/{id}/refund` | Refund |
| POST | `/merchants/{id}/payment_methods` | Vault payment method |
| POST | `/merchants/{id}/client_token` | Generate client token |
| POST | `/merchants/{id}/plans` | Create plan |
| GET | `/merchants/{id}/plans` | List plans |
| GET | `/merchants/{id}/plans/{planId}` | Get plan |
| POST | `/merchants/{id}/subscriptions` | Create subscription |
| GET | `/merchants/{id}/subscriptions/{subId}` | Get subscription |
| POST | `/merchants/{id}/subscriptions/{subId}/cancel` | Cancel subscription |
| POST | `/webhooks` | Register webhook (`{url, kinds}`) or inbound notification verification (`{bt_signature, bt_payload}`) |

Handlers that read-modify-write the same transaction or subscription are
serialized through a per-`{id}` concurrency key, so concurrent capture/void/
refund/refund-sum checks cannot interleave.

## Webhooks

### Registration

Real Braintree webhooks are configured in the Control Panel (no public REST
registration endpoint), so the simulator exposes registration on the webhook
path itself:

```bash
curl -X POST http://localhost:8080/webhooks \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{ "url": "http://localhost:9090/braintree/webhook", "kinds": ["transaction_settled", "refund_opened", "subscription_charged_successfully"] }'
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
| `transaction_settled` | A transaction's settlement completes (the +3s derive-on-read transition) |
| `refund_opened` | REST refund or GraphQL `refundTransaction` |
| `subscription_charged_successfully` | Each completed subscription billing cycle |
| `subscription_expired` | A subscription completes its final billing cycle |
| `subscription_cancelled` | A subscription is canceled |

(A bare authorization, a submission for settlement, and a void trigger no
notification — matching the real webhook kind list.)

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

## Test coverage

`internal/engine/braintree_style_test.go` walks the full lifecycle: creates
with and without `submit_for_settlement`, partial capture, void and its state
guard, simulated authorization expiry, full/partial/over refunds, the
nonexistent-transaction 404, `advanced_search` (status `in` + amount
range), plan/subscription create-list-get, `Active -> Canceled`,
`Active -> Expired`, and the cancel-non-Active failure path.

## Concurrency note

Transitions persist before webhook emission, and id-scoped routes carry
`concurrency_key`. Two remaining narrow windows exist by design: list/bulk
surfaces (advanced_search, runs list, RPC) can race a keyed single-resource
read on the same record and in principle double-emit the transition webhook;
and GraphQL mutations (braintree) key on a body id the engine cannot lock —
the REST surface is the concurrency-safe one.
