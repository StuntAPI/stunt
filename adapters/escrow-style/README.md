# escrow-style

An unofficial, local-testing simulator for an **Escrow.com-style transaction
API** (public `2017-09-01` surface) — licensed-escrow transaction lifecycle
without creating remote accounts or moving money.

## Lifecycle modelled

```
created → both parties agree → buyer funds (secured) → shipped/received →
buyer accepts → closed
```

`PATCH /2017-09-01/transaction/{id}` with `{"action": "agree", "customer":
"<email>"}` records one party's agreement; the transaction becomes `secured`
when every schedule entry has funds behind it.

## Surface

| Endpoint | Behaviour |
| --- | --- |
| `GET /2017-09-01/customer/me` | The authenticated customer record |
| `POST /2017-09-01/transaction` | Create: parties (buyer + seller required), items with payment schedules, optional fee split; fee defaults to the buyer at 3.25% |
| `GET /2017-09-01/transaction/{id}` | Retrieve by numeric id |
| `GET /2017-09-01/transaction/reference/{reference}` | Retrieve by your own reference |
| `PATCH /2017-09-01/transaction/{id}` | `action: agree` (with the agreeing customer's email) |
| `GET/POST /2017-09-01/customer/me/webhook` | Webhook URL registration |
| `POST /sim/transaction/{id}/fund` | **Simulator affordance, not a real endpoint**: funds the transaction, standing in for the buyer paying on the provider's hosted page — which no API can drive |

## Why the simulator affordance

In the real product, funding happens on the provider's own website. A test
suite cannot click through that, so the adapter exposes a clearly-marked
`/sim/...` endpoint that flips the same state the hosted flow would. It is
namespaced so it can never be confused with API surface.

## Quick start

```yaml
# stunt.yaml
network: { mode: port, base_port: 4210 }
services:
  escrow:
    adapter: ./adapters/escrow-style
```

```sh
curl -X POST localhost:4210/2017-09-01/transaction -H 'Content-Type: application/json' \
     -d '{"description": "Test deal", "currency": "usd",
          "parties": [{"role": "buyer", "customer": "buyer@sim.invalid"},
                      {"role": "seller", "customer": "seller@sim.invalid"}],
          "items": [{"title": "Website", "description": "d", "schedule":
                     [{"amount": 1000.0, "payer_customer": "buyer@sim.invalid",
                       "beneficiary_customer": "seller@sim.invalid"}]}]}'
```

## Response shapes

The Transaction object carries **no top-level status**: funding state lives on
`items[].schedule[].status.secured`, item lifecycle on `items[].status`
(accepted/received/shipped/rejected/canceled/in_dispute). Amounts and fees
render as decimal strings ("1000.00"). The initiating (first-listed) party is
auto-agreed at creation, matching the real API's creator-agrees rule.
Validation errors use the real nested shape
(`{"errors":{"parties":{"0":["Transaction must have 1 seller"]}}}`).

## Auth

HTTP Basic with the documented synthetic credentials
`escrow-test` / `escrow-test-api-key`; a 401 carries `WWW-Authenticate`.
Webhook deliveries are unsigned-by-design (Escrow.com does not sign).
