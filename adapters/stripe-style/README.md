# Stripe-style adapter

A stunt adapter for simulating a **Stripe-style payments API** locally at full
API breadth — payments, disputes, refunds, the Billing suite (products,
prices, subscriptions, invoices, credit notes), Checkout Sessions,
SetupIntents, balance transactions, webhook endpoints, files — plus **Stripe
Connect** (connected accounts, capabilities, persons, external accounts,
transfers, application fees, payouts). **158 endpoints.**
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Stripe. "Stripe" is a trademark of its respective owner.
> See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

The complete core surface a Stripe integration exercises:

- **PaymentIntents** (create/confirm/capture/retrieve/list — the SCA/3DS-ready
  flow with `requires_payment_method → succeeded` / `requires_capture` states
  and real decline/SCA test-card behavior), **PaymentMethods**, card
  **tokens**, **Charges** (incl. capture + charge-level refunds)
- **Refunds** — full/partial, `pending → succeeded/failed` derive-on-read
  lifecycle, over-refund guard, **cancel** (with `failure_reason` +
  `refund_failure` ledger row)
- **Disputes** — test-card triggered, `needs_response → under_review →
  won/lost` lifecycle, all 27 evidence fields, submit/close, funds
  withdrawn/reinstated through the ledger
- **Balance + balance transactions** — a real ledger across every money
  movement (charge incl. processing fee, refund, payout, transfer,
  application fee, dispute, and their reversals), account-scoped
- **Billing** — products, prices, subscriptions (clock-driven renewal +
  auto-charge with `past_due` on decline), subscription items, usage records
  (metered), invoices (draft → open → paid/void/uncollectible with
  finalize/pay/void/send/lines/upcoming), invoice items, credit notes
  (issuing real refunds), coupons, promotion codes, tax rates (exclusive +
  inclusive)
- **Checkout Sessions** — payment/subscription/setup modes, hosted completion
  page, expire, line items
- **SetupIntents** — confirm/cancel with SCA challenge + decline behavior
- **Webhook endpoints** — CRUD + registration-gated delivery per
  `enabled_events`
- **Files + file links** — multipart upload with purpose validation
- **Test Clocks** — Stripe's deterministic-time mechanism, so billing cycles,
  dispute settlement and payout lifecycles are assertable **without sleeps**
- **Customers** (CRUD + soft delete), **Events** (recorded webhook copies)
- **Stripe Connect** — connected accounts (capabilities, requirements,
  settings), persons, external bank accounts (last4 only), account links,
  login links, transfers (+ partial reversals), application fees (+ refunds),
  payouts (full lifecycle + cancel)

State persists in an on-disk SQLite-backed collection store (under `.stunt/state/`),
so data you create in one request is visible in subsequent requests and survives
across `stunt up` restarts. Run `stunt clean` to reset state to the seed fixtures.

Webhook events are emitted on lifecycle transitions to a configurable webhook
sink, signed with `Stripe-Signature`. Mutating endpoints honour Stripe's
`Idempotency-Key` header, and all list endpoints use Stripe-style cursor
pagination (`limit`/`starting_after`).

## Auth

All endpoints (except `/v1/tokens` and the hosted `/c/pay/{id}` page) require
a valid `Authorization: Bearer <token>` header.

### Token validation

The adapter validates bearer tokens via the identity primitive
(`identity_validate`). If the token is missing or invalid, the adapter
returns `401` with a JSON body:

```json
{"error": {"type": "authentication_error", "message": "..."}}
```

### Dev bypass (`sk_test`)

For frictionless local testing, **any token starting with `sk_test`** is
accepted **without** `identity_validate`:

```bash
curl -H "Authorization: Bearer sk_test_local" http://localhost:PORT/v1/charges
```

### Minting a real token

`POST /v1/tokens` with an empty body mints a real identity token; with a card
body it creates a card token (`tok_*`) whose stored number drives the
test-card behavior below (the full number is never returned — only
brand/last4).

## Test Clocks (deterministic time)

Real Stripe's Test Clocks make time-dependent flows testable; this adapter
implements them on a KV-backed offset that drives **every** adapter timestamp.
Create a clock, advance it, and every derive-on-read lifecycle settles on the
next request — no sleeps, no flaky timing:

```bash
curl -X POST http://localhost:PORT/v1/test_clocks \
  -H "Authorization: Bearer sk_test_local" \
  -d '{"frozen_time":1735689600}'
# → {"id":"clock_1","object":"test_helpers.test_clock","status":"advancing",...}

curl -X POST http://localhost:PORT/v1/test_clocks/clock_1/advance \
  -H "Authorization: Bearer sk_test_local" \
  -d '{"now":1736208000}'   # +6 days: past the billing period end
# the next GET /v1/subscriptions/sub_1 now shows the renewed period,
# a paid invoice, and the underlying charge + ledger rows
```

Both the `/v1/test_clocks` and the real-path `/v1/test_helpers/test_clocks`
routes work. Simulator note: the clock is **global** (one offset shared by all
objects), unlike real Stripe's per-object clocks.

Clock-driven lifecycles: subscription renewal/cancel-at-period-end, invoice
payment attempts, dispute evidence due-by/settlement, payout
`pending → in_transit → paid`, capability `pending → active`, checkout
session expiry.

## Pagination

All list endpoints use Stripe-style cursor pagination:

- `?limit=<n>` — page size, default **10**, capped at **100**.
- `?starting_after=<id>` — return results after this object id.
- Responses are `{"object": "list", "data": [...], "has_more": bool, "url": ...}`.
- A `starting_after` id that no longer exists returns `400` with
  `param: "starting_after"` (Stripe's `resource_missing`), never a silent
  restart.
- Non-numeric `created` / `created[gt|gte|lt|lte]` filter values return
  Stripe's `400 parameter_invalid_integer`.

## Decline, SCA & dispute test cards

Like real Stripe test mode, specific card numbers deterministically trigger
outcomes. Create a card token with `POST /v1/tokens`, then use it at
PaymentIntent confirm (`payment_method: "tok_..."`), charge create
(`source: "tok_..."`), SetupIntent confirm, subscription/invoice payment, or
the Checkout completion page. A PaymentMethod created with an explicit
`card[number]` behaves the same way.

### Declines

These cards fail with `402` and the real `card_error` shape — `error.code`
plus the real `decline_code`. The PI stays `requires_payment_method` and
records `last_payment_error`; on the Charges API the failed charge object is
still recorded and `charge.failed` fires.

| Card number | `error.code` | `decline_code` |
|-------------|--------------|----------------|
| `4000 0000 0000 0002` | `card_declined` | `generic_decline` |
| `4000 0000 0000 9995` | `card_declined` | `insufficient_funds` |
| `4000 0000 0000 9987` | `card_declined` | `lost_card` |
| `4000 0000 0000 9988` | `card_declined` | `stolen_card` |
| `4000 0000 0000 0069` | `expired_card` | `expired_card` |
| `4000 0000 0000 0127` | `incorrect_cvc` | `incorrect_cvc` |

### SCA / 3DS

These cards force an authentication step on PaymentIntent confirm: the PI
returns `200` with `status: "requires_action"` and a `next_action` object:

| Card number | `next_action.type` |
|-------------|--------------------|
| `4000 0027 6000 3184` | `use_stripe_sdk` (in-app `stripe_js` URL) |
| `4000 0025 0000 3155` | `redirect_to_url` (hosted `url` + your `return_url`) |
| `4000 0000 0000 3220` | `redirect_to_url` |

Confirm the same PaymentIntent again to complete authentication. SetupIntents
run the same challenge flow. The legacy Charges API cannot run 3DS, so SCA
cards on `POST /v1/charges` decline with `authentication_required`. On the
Checkout hosted page SCA cards succeed (the hosted UI runs 3DS — simulator
simplification).

### Disputes

These cards create a dispute on charge success (the charge itself succeeds
and is captured; the dispute then withdraws the funds + a $15 dispute fee
from the ledger, emits `charge.dispute.created` +
`charge.dispute.funds_withdrawn`, and links `charge.dispute`):

| Card number | `reason` |
|-------------|----------|
| `4000 0000 0000 0259` | `fraudulent` |
| `4000 0000 0000 2685` | `product_not_received` |

## Refunds

Refunds have their own async lifecycle: `POST /v1/refunds` (or
`POST /v1/charges/{id}/refund`) creates a refund with `status: "pending"`
linked to its ledger row (`balance_transaction`). Every read derives the
terminal state from the clock — `succeeded` after 3 seconds, or `failed` when
created with the simulator-only `simulate_fail: true` flag — persists the
transition, and fires `refund.updated` exactly once.

- **Amount** — omitted `amount` refunds the full remaining unrefunded balance;
  partial refunds accumulate `amount_refunded` on the charge.
- **Over-refund guard** — computed across all non-failed, non-canceled
  refunds; refunding more than the remainder returns Stripe's real `400`.
- **Cancel** — `POST /v1/refunds/{id}/cancel` cancels a *pending* refund
  (`failure_reason: "merchant_request"`, a `refund_failure` ledger row returns
  the reserved funds); non-pending refunds return the real error.
- Refunding an **uncaptured** charge releases the authorization instead of
  moving money.

## Disputes

`GET /v1/disputes` (filters `charge`, `payment_intent`, `created`),
`GET /v1/disputes/{id}`, `POST /v1/disputes/{id}` (evidence +
`submit: true`), `POST /v1/disputes/{id}/close`.

The lifecycle is derive-on-read from the clock:

```
needs_response ──(submit=true)──▶ under_review ──(+1 day)──▶ won  (funds reinstated)
      │                                                                ▲
      └──(past due_by: created+7d)──▶ lost ◀──(close)──────────────────┘
```

- **Evidence** — all 27 real string fields (`product_description`,
  `customer_name`, `receipt`, `uncategorized_text`, …). Saving evidence
  without `submit` sets `evidence_details.has_evidence` but keeps the status;
  `submit: true` with zero evidence is a `400`.
- **Won** — restores the disputed amount + fee through a
  `dispute_reversal` ledger row, emits `charge.dispute.funds_reinstated` +
  `charge.dispute.closed` (status `won`).
- **Lost** — past `due_by` or `POST /{id}/close`; funds stay withdrawn,
  `charge.dispute.closed` (status `lost`).
- Terminal disputes reject further evidence updates with `400`.

## Billing

### Products & prices

Standard CRUD for products (`DELETE` archives, retrievable afterwards) and
prices (no delete, like the real API). Prices support `unit_amount` /
`unit_amount_decimal`, `recurring` (`interval` day/week/month/year,
`interval_count`, `usage_type` licensed|metered, `aggregate_usage`,
`trial_period_days`), `lookup_key` (unique), filters (`product`, `active`,
`type`, `currency`, `lookup_keys`).

### Subscriptions

Create with `items: [{price, quantity}]` (or the legacy top-level
`price`/`quantity` form), `default_payment_method`, `cancel_at_period_end`,
`trial_end`, `collection_method`, `coupon` / `promotion_code`,
`default_tax_rates`, `billing_cycle_anchor`. The first invoice is created —
and under `charge_automatically` paid — immediately, using the card-behavior
rules above.

**Renewal is derive-on-read from the clock**: when `_now() >=
current_period_end` (i.e. after a test-clock advance), the subscription
either renews (new period + a new invoice, auto-charged — decline/no-PM →
`past_due` + open invoice + `invoice.payment_failed`) or ends
(`cancel_at_period_end` → `canceled` + `customer.subscription.deleted`).
`GET` and `LIST` both settle pending transitions first.

Updates handle item quantity changes, additions, and removals
(`deleted: true`; the last item cannot be removed). Proration params are
accepted but treated as `none` — no proration lines are generated
(simulator simplification).

### Subscription items & usage records

`/v1/subscription_items` (list/create/update/delete, projected from their
subscriptions) and `/v1/subscription_items/{id}/usage_records`
(`quantity`, `timestamp`, `action: increment|set`). Metered prices bill the
sum of records (`last_ever` aggregates the newest record) on the next
invoice.

### Invoices

Draft → open → paid/void/uncollectible, with `finalize`, `pay`
(`paid_out_of_band`, or a real charge via the resolved payment method —
declines keep the invoice open + `invoice.payment_failed`), `void`,
`mark_uncollectible`, `send`, `lines`, and `upcoming` (preview for a customer
+ subscription incl. pending invoice items and tax — never persisted).
Invoices carry `subtotal`, `discount`, `tax`, `total`, `amount_due`, and
`status_transitions`.

### Invoice items, credit notes, coupons, promotion codes, tax rates

- **Invoice items** (`/v1/invoice_items`, CRUD) — pending items are pulled
  into the next invoice.
- **Credit notes** — against a paid invoice; `lines` or
  `refund_amount`/`credit_amount` + real reason enum; `refund: true` issues a
  real refund; `preview` endpoint included.
- **Coupons** — `percent_off` XOR `amount_off`, `duration`
  once/forever/repeating; `duration: once` applies to the first invoice only.
- **Promotion codes** — wrap a coupon, restrictions, `code` auto-generated.
- **Tax rates** — `inclusive` rates are shown in `tax` but not added to the
  total; `exclusive` rates are added. Amounts stay integer cents.

## Checkout Sessions

Create sessions in `payment` / `subscription` / `setup` mode with
`line_items` (`price` + `quantity`, or inline `price_data`), `success_url` /
`cancel_url` (supporting `{CHECKOUT_SESSION_ID}` substitution), `customer` or
`customer_email`.

Completion is driven through the hosted page — the session's `url` is
`/c/pay/{id}` (no auth, like a real hosted page). `GET`ting it:

- **payment mode** — creates + confirms the PaymentIntent and its charge,
  flips the session to `complete`/`paid`, emits
  `checkout.session.completed` + `payment_intent.succeeded` +
  `charge.succeeded`, and `302`s to your `success_url` with the session id
  substituted.
- **subscription mode** — creates the subscription (active, first invoice
  paid) + underlying objects.
- **setup mode** — creates + succeeds a SetupIntent
  (`payment_status: "no_payment_required"`).
- A `?payment_method=` param runs the card behavior: a decline card fails the
  attempt (`checkout.session.async_payment_failed`, session stays open);
  retrying with a good card completes.
- Completion is one-shot (guarded + serialized); expired sessions render an
  expired page; `POST /v1/checkout/sessions/{id}/expire` expires an open
  session; `GET .../line_items` lists the session's items.

## Webhook endpoints & delivery gating

`/v1/webhook_endpoints` (create/list/retrieve/update/delete) mirrors the real
resource: `url`, `enabled_events` (validated non-empty), `description`,
`metadata`, and the mock signing `secret` + `api_version`.

Delivery is **registration-gated**, like real Stripe: while no endpoint is
registered, every event delivers to the configured sink (frictionless local
use); once one exists, only its `enabled_events` (or `*`) deliver. Events are
always recorded in `/v1/events` either way. `DELETE` is a hard delete so the
gate stops counting the endpoint.

## Events

Every emitted webhook is also recorded as a Stripe event object:

- `GET /v1/events` — cursor-paginated (newest first, `type=` and `created`
  filters).
- `GET /v1/events/{id}` — single event.

```json
{
  "id": "evt_1", "object": "event", "type": "charge.created",
  "api_version": "2025-01-27.acacia", "created": 1739577600,
  "data": {"object": {"id": "ch_1", "amount": 5000}},
  "livemode": false, "pending_webhooks": 0,
  "request": {"id": null, "idempotency_key": null}
}
```

## Webhooks

The adapter emits webhook events on lifecycle transitions. Events are
**fire-and-forget**: if no webhook sink is configured or the delivery fails,
the operation still succeeds.

| Area | Event types |
|------|-------------|
| Charges | `charge.created`, `charge.failed`, `charge.updated`, `charge.refunded`, `charge.captured` |
| PaymentIntents | `payment_intent.created` / `.succeeded` / `.requires_capture` / `.requires_action` / `.payment_failed` |
| SetupIntents | `setup_intent.created` / `.succeeded` / `.setup_failed` / `.canceled` |
| Refunds | `refund.created`, `refund.updated` |
| Disputes | `charge.dispute.created` / `.updated` / `.funds_withdrawn` / `.funds_reinstated` / `.closed` |
| Billing | `customer.subscription.created` / `.updated` / `.deleted`, `invoice.created` / `.finalized` / `.paid` / `.payment_failed` / `.payment_succeeded` / `.voided` / `.sent` / `.mark_uncollectible`, `credit_note.created` / `.updated` / `.voided` |
| Checkout | `checkout.session.completed` / `.expired` / `.async_payment_failed` |
| Connect | `account.updated`, `person.created` / `.updated` / `.deleted`, `transfer.created` / `.reversed`, `payout.created` / `.updated` / `.paid` / `.canceled`, `application_fee.refunded` |
| Customers | `customer.created` / `.updated` / `.deleted` |

### Configuring the webhook sink

```yaml
services:
  stripe:
    adapter: ./adapters/stripe-style
    config:
      webhook_url: http://localhost:9090/webhook
```

### Webhook signatures

Every delivery is signed with a Stripe-style `Stripe-Signature` header:

```
Stripe-Signature: t=<unix>,v1=<hex(HMAC-SHA256(secret, "{t}.{raw_body}"))>
```

**Mock signing secret** (also returned by `POST /v1/webhook_endpoints`):

```
whsec_stunt_mock_0123456789abcdef0123456789abcdef
```

```go
mac := hmac.New(sha256.New, []byte("whsec_stunt_mock_0123456789abcdef0123456789abcdef"))
mac.Write([]byte(fmt.Sprintf("%d.%s", t, rawBody)))
expected := hex.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), v1) { return 401 }
```

## Balance & balance transactions

`GET /v1/balance` (platform synthetic defaults; `Stripe-Account` header
scopes to a connected account's tracked balance).

`GET /v1/balance_transactions` (+`/{id}`) is the real ledger: every money
movement records a `txn_*` row with `amount`, `fee`, `net`, `type` (`charge`
— including the 2.9% + 30¢ processing fee, `refund`, `refund_failure`,
`payout`, `transfer`, `transfer_reversal`, `application_fee`,
`application_fee_refund`, `dispute`, `dispute_reversal`), `source`,
`description`, `available_on`. Rows are account-scoped like real Stripe
(`Stripe-Account` header → that account's rows, otherwise platform-only);
filters `payout`/`charge`/`refund`/`dispute`/`transfer`/`source`/`type`/
`currency` + `created` ranges.

## Stripe Connect

Connected accounts (create/retrieve/update/list) with `capabilities`
(requested → `pending` → `active` after a day on the clock; Express/Custom
accounts auto-request the core capabilities), `requirements`, `settings`
(payouts schedule, branding), `business_profile`; **persons** (CRUD +
`relationship` filter); **external bank accounts** (last4/fingerprint only,
first per currency auto-defaults, payouts resolve the default destination);
**login links** (Express only); **account links** (onboarding URLs).

```bash
curl -X POST http://localhost:PORT/v1/accounts \
  -H "Authorization: Bearer sk_test_local" \
  -d '{"type":"express","country":"US","email":"seller@example.com"}'
# → {"id":"acct_1","object":"account","type":"express",...}
```

### Transfers (+ reversals)

`POST /v1/transfers` moves platform balance to a connected account (both
sides get ledger rows). `POST /v1/transfers/{id}/reversals` returns the real
`transfer_reversal` object and supports **partial** amounts (multiple
partials accumulate in `amount_reversed`; over-reversal is a `400`).
Reversals are listable/retrievable under
`GET /v1/transfers/{id}/reversals[/{trr_id}]`.

### Application fees

Charges created with `application_fee_amount` record a `fee_*`
application-fee doc (+ platform ledger row). `GET /v1/application_fees`,
`GET /v1/application_fees/{id}`, and refunds — partial or full — via both
the real `/v1/application_fees/{id}/refunds` route and the legacy
`/v1/application_fees/{id}/refund`, each writing an
`application_fee_refund` ledger row and emitting `application_fee.refunded`.

### Payouts

`POST /v1/payouts` debits the connected account (`Stripe-Account` header) and
records the ledger row; `arrival_date` derives from the method (standard +4
days, instant +60s). The status is derive-on-read from the clock:
`pending → in_transit (+10s) → paid (+60s)`, each transition emitting its
event exactly once. `GET`/`POST /v1/payouts/{id}` (metadata updates),
`POST /v1/payouts/{id}/cancel` returns the funds (allowed while
`pending`/`in_transit`; `paid` is a `400`).

## Simulator notes

Deliberate simplifications (all also commented in the scripts):

- Test clocks are global (one offset), not per-object.
- No proration lines are generated on subscription item changes
  (`proration_behavior` accepted, treated as `none`).
- Checkout's hosted page runs 3DS for SCA cards (succeeds) rather than
  challenging.
- `POST /v1/files` returns `201` (adapter-wide create convention).
- Dispute ids are `dp_*` (real Stripe currently mints `du_*`).
- Webhook delivery goes to the single configured sink; endpoint
  registration gates *which* event types deliver (not per-endpoint URLs).

## Endpoints

| Method | Route | Handler |
|--------|-------|---------|
| POST | `/v1/tokens` | `tokens.star#on_mint_token` |
| GET | `/v1/events` | `events.star#on_list_events` |
| GET | `/v1/events/{id}` | `events.star#on_retrieve_event` |
| POST | `/v1/test_clocks` | `test_clocks.star#on_create_test_clock` |
| GET | `/v1/test_clocks` | `test_clocks.star#on_list_test_clocks` |
| POST | `/v1/test_clocks/{id}/advance` | `test_clocks.star#on_advance_test_clock` |
| GET | `/v1/test_clocks/{id}` | `test_clocks.star#on_retrieve_test_clock` |
| DELETE | `/v1/test_clocks/{id}` | `test_clocks.star#on_delete_test_clock` |
| POST/GET | `/v1/test_helpers/test_clocks…` | real-path aliases of the above |
| POST | `/v1/charges` | `charges.star#on_create_charge` |
| GET | `/v1/charges/{id}` | `charges.star#on_retrieve_charge` |
| GET | `/v1/charges` | `charges.star#on_list_charges` |
| POST | `/v1/charges/{id}/capture` | `charges.star#on_capture_charge` |
| POST | `/v1/charges/{id}/refund` | `charges.star#on_refund_charge` |
| POST | `/v1/payment_intents` | `payment_intents.star#on_create_payment_intent` |
| POST | `/v1/payment_intents/{id}/confirm` | `payment_intents.star#on_confirm_payment_intent` |
| POST | `/v1/payment_intents/{id}/capture` | `payment_intents.star#on_capture_payment_intent` |
| GET | `/v1/payment_intents/{id}` | `payment_intents.star#on_retrieve_payment_intent` |
| GET | `/v1/payment_intents` | `payment_intents.star#on_list_payment_intents` |
| POST | `/v1/payment_methods` | `payment_methods.star#on_create_payment_method` |
| POST | `/v1/payment_methods/{id}/attach` | `payment_methods.star#on_attach_payment_method` |
| POST | `/v1/payment_methods/{id}/detach` | `payment_methods.star#on_detach_payment_method` |
| GET | `/v1/payment_methods/{id}` | `payment_methods.star#on_retrieve_payment_method` |
| GET | `/v1/payment_methods` | `payment_methods.star#on_list_payment_methods` |
| POST | `/v1/refunds` | `refunds.star#on_create_refund` |
| GET | `/v1/refunds/{id}` | `refunds.star#on_retrieve_refund` |
| GET | `/v1/refunds` | `refunds.star#on_list_refunds` |
| POST | `/v1/refunds/{id}/cancel` | `refunds.star#on_cancel_refund` |
| POST | `/v1/customers` | `customers.star#on_create_customer` |
| GET | `/v1/customers/{id}` | `customers.star#on_retrieve_customer` |
| GET | `/v1/customers` | `customers.star#on_list_customers` |
| POST | `/v1/customers/{id}` | `customers.star#on_update_customer` |
| DELETE | `/v1/customers/{id}` | `customers.star#on_delete_customer` |
| POST | `/v1/products` | `products.star#on_create_product` |
| GET | `/v1/products` | `products.star#on_list_products` |
| GET | `/v1/products/{id}` | `products.star#on_retrieve_product` |
| POST | `/v1/products/{id}` | `products.star#on_update_product` |
| DELETE | `/v1/products/{id}` | `products.star#on_delete_product` |
| POST | `/v1/prices` | `prices.star#on_create_price` |
| GET | `/v1/prices` | `prices.star#on_list_prices` |
| GET | `/v1/prices/{id}` | `prices.star#on_retrieve_price` |
| POST | `/v1/prices/{id}` | `prices.star#on_update_price` |
| POST | `/v1/subscriptions` | `subscriptions.star#on_create_subscription` |
| GET | `/v1/subscriptions` | `subscriptions.star#on_list_subscriptions` |
| GET | `/v1/subscriptions/{id}` | `subscriptions.star#on_retrieve_subscription` |
| POST | `/v1/subscriptions/{id}` | `subscriptions.star#on_update_subscription` |
| POST | `/v1/subscriptions/{id}/cancel` | `subscriptions.star#on_cancel_subscription` |
| GET | `/v1/subscription_items` | `subscription_items.star#on_list_subscription_items` |
| POST | `/v1/subscription_items` | `subscription_items.star#on_create_subscription_item` |
| POST | `/v1/subscription_items/{id}` | `subscription_items.star#on_update_subscription_item` |
| DELETE | `/v1/subscription_items/{id}` | `subscription_items.star#on_delete_subscription_item` |
| POST | `/v1/subscription_items/{id}/usage_records` | `subscription_items.star#on_create_usage_record` |
| GET | `/v1/subscription_items/{id}/usage_records` | `subscription_items.star#on_list_usage_records` |
| POST | `/v1/invoices` | `invoices.star#on_create_invoice` |
| GET | `/v1/invoices` | `invoices.star#on_list_invoices` |
| GET | `/v1/invoices/upcoming` | `invoices.star#on_upcoming_invoice` |
| GET | `/v1/invoices/{id}` | `invoices.star#on_retrieve_invoice` |
| POST | `/v1/invoices/{id}` | `invoices.star#on_update_invoice` |
| DELETE | `/v1/invoices/{id}` | `invoices.star#on_delete_invoice` |
| POST | `/v1/invoices/{id}/finalize` | `invoices.star#on_finalize_invoice` |
| POST | `/v1/invoices/{id}/pay` | `invoices.star#on_pay_invoice` |
| POST | `/v1/invoices/{id}/send` | `invoices.star#on_send_invoice` |
| POST | `/v1/invoices/{id}/void` | `invoices.star#on_void_invoice` |
| POST | `/v1/invoices/{id}/mark_uncollectible` | `invoices.star#on_mark_uncollectible_invoice` |
| GET | `/v1/invoices/{id}/lines` | `invoices.star#on_list_invoice_lines` |
| POST | `/v1/invoice_items` | `invoice_items.star#on_create_invoice_item` |
| GET | `/v1/invoice_items` | `invoice_items.star#on_list_invoice_items` |
| GET | `/v1/invoice_items/{id}` | `invoice_items.star#on_retrieve_invoice_item` |
| POST | `/v1/invoice_items/{id}` | `invoice_items.star#on_update_invoice_item` |
| DELETE | `/v1/invoice_items/{id}` | `invoice_items.star#on_delete_invoice_item` |
| POST | `/v1/credit_notes` | `credit_notes.star#on_create_credit_note` |
| GET | `/v1/credit_notes` | `credit_notes.star#on_list_credit_notes` |
| GET | `/v1/credit_notes/preview` | `credit_notes.star#on_preview_credit_note` |
| GET | `/v1/credit_notes/{id}` | `credit_notes.star#on_retrieve_credit_note` |
| POST | `/v1/credit_notes/{id}` | `credit_notes.star#on_update_credit_note` |
| POST | `/v1/credit_notes/{id}/void` | `credit_notes.star#on_void_credit_note` |
| POST | `/v1/coupons` | `coupons.star#on_create_coupon` |
| GET | `/v1/coupons` | `coupons.star#on_list_coupons` |
| GET | `/v1/coupons/{id}` | `coupons.star#on_retrieve_coupon` |
| POST | `/v1/coupons/{id}` | `coupons.star#on_update_coupon` |
| DELETE | `/v1/coupons/{id}` | `coupons.star#on_delete_coupon` |
| POST | `/v1/promotion_codes` | `promotion_codes.star#on_create_promotion_code` |
| GET | `/v1/promotion_codes` | `promotion_codes.star#on_list_promotion_codes` |
| GET | `/v1/promotion_codes/{id}` | `promotion_codes.star#on_retrieve_promotion_code` |
| POST | `/v1/promotion_codes/{id}` | `promotion_codes.star#on_update_promotion_code` |
| POST | `/v1/tax_rates` | `tax_rates.star#on_create_tax_rate` |
| GET | `/v1/tax_rates` | `tax_rates.star#on_list_tax_rates` |
| GET | `/v1/tax_rates/{id}` | `tax_rates.star#on_retrieve_tax_rate` |
| POST | `/v1/tax_rates/{id}` | `tax_rates.star#on_update_tax_rate` |
| DELETE | `/v1/tax_rates/{id}` | `tax_rates.star#on_delete_tax_rate` |
| GET | `/v1/balance` | `balance.star#on_get_balance` |
| GET | `/v1/balance_transactions` | `balance.star#on_list_balance_transactions` |
| GET | `/v1/balance_transactions/{id}` | `balance.star#on_retrieve_balance_transaction` |
| GET | `/v1/disputes` | `disputes.star#on_list_disputes` |
| GET | `/v1/disputes/{id}` | `disputes.star#on_retrieve_dispute` |
| POST | `/v1/disputes/{id}` | `disputes.star#on_update_dispute` |
| POST | `/v1/disputes/{id}/close` | `disputes.star#on_close_dispute` |
| GET | `/v1/application_fees` | `application_fees.star#on_list_application_fees` |
| GET | `/v1/application_fees/{id}` | `application_fees.star#on_retrieve_application_fee` |
| GET | `/v1/application_fees/{id}/refunds` | `application_fees.star#on_list_fee_refunds` |
| POST | `/v1/application_fees/{id}/refunds` | `application_fees.star#on_create_fee_refund` |
| POST | `/v1/application_fees/{id}/refund` | `application_fees.star#on_refund_application_fee` |
| POST | `/v1/checkout/sessions` | `checkout.star#on_create_checkout_session` |
| GET | `/v1/checkout/sessions` | `checkout.star#on_list_checkout_sessions` |
| GET | `/v1/checkout/sessions/{id}/line_items` | `checkout.star#on_list_checkout_session_line_items` |
| POST | `/v1/checkout/sessions/{id}/expire` | `checkout.star#on_expire_checkout_session` |
| GET | `/v1/checkout/sessions/{id}` | `checkout.star#on_retrieve_checkout_session` |
| GET | `/c/pay/{id}` | `checkout.star#on_pay_checkout_session` |
| POST | `/v1/setup_intents` | `setup_intents.star#on_create_setup_intent` |
| GET | `/v1/setup_intents` | `setup_intents.star#on_list_setup_intents` |
| POST | `/v1/setup_intents/{id}/confirm` | `setup_intents.star#on_confirm_setup_intent` |
| POST | `/v1/setup_intents/{id}/cancel` | `setup_intents.star#on_cancel_setup_intent` |
| GET | `/v1/setup_intents/{id}` | `setup_intents.star#on_retrieve_setup_intent` |
| POST | `/v1/setup_intents/{id}` | `setup_intents.star#on_update_setup_intent` |
| POST | `/v1/webhook_endpoints` | `webhook_endpoints.star#on_create_webhook_endpoint` |
| GET | `/v1/webhook_endpoints` | `webhook_endpoints.star#on_list_webhook_endpoints` |
| GET | `/v1/webhook_endpoints/{id}` | `webhook_endpoints.star#on_retrieve_webhook_endpoint` |
| POST | `/v1/webhook_endpoints/{id}` | `webhook_endpoints.star#on_update_webhook_endpoint` |
| DELETE | `/v1/webhook_endpoints/{id}` | `webhook_endpoints.star#on_delete_webhook_endpoint` |
| POST | `/v1/files` | `files.star#on_create_file` |
| GET | `/v1/files` | `files.star#on_list_files` |
| GET | `/v1/files/{id}` | `files.star#on_retrieve_file` |
| POST | `/v1/file_links` | `files.star#on_create_file_link` |
| GET | `/v1/file_links` | `files.star#on_list_file_links` |
| GET | `/v1/file_links/{id}` | `files.star#on_retrieve_file_link` |
| POST | `/v1/file_links/{id}` | `files.star#on_update_file_link` |
| POST | `/v1/accounts` | `accounts.star#on_create_account` |
| GET | `/v1/accounts/{id}` | `accounts.star#on_retrieve_account` |
| POST | `/v1/accounts/{id}` | `accounts.star#on_update_account` |
| GET | `/v1/accounts` | `accounts.star#on_list_accounts` |
| POST | `/v1/accounts/{id}/persons` | `persons.star#on_create_person` |
| GET | `/v1/accounts/{id}/persons` | `persons.star#on_list_persons` |
| GET | `/v1/accounts/{id}/persons/{person_id}` | `persons.star#on_retrieve_person` |
| POST | `/v1/accounts/{id}/persons/{person_id}` | `persons.star#on_update_person` |
| DELETE | `/v1/accounts/{id}/persons/{person_id}` | `persons.star#on_delete_person` |
| GET | `/v1/persons/{id}` | `persons.star#on_retrieve_person_standalone` |
| POST | `/v1/persons/{id}` | `persons.star#on_update_person_standalone` |
| POST | `/v1/accounts/{id}/external_accounts` | `accounts.star#on_create_external_account` |
| GET | `/v1/accounts/{id}/external_accounts` | `accounts.star#on_list_external_accounts` |
| GET | `/v1/accounts/{id}/external_accounts/{ea_id}` | `accounts.star#on_retrieve_external_account` |
| DELETE | `/v1/accounts/{id}/external_accounts/{ea_id}` | `accounts.star#on_delete_external_account` |
| POST | `/v1/accounts/{id}/login_links` | `accounts.star#on_create_login_link` |
| POST | `/v1/account_links` | `account_links.star#on_create_account_link` |
| POST | `/v1/transfers` | `transfers.star#on_create_transfer` |
| GET | `/v1/transfers/{id}` | `transfers.star#on_retrieve_transfer` |
| GET | `/v1/transfers` | `transfers.star#on_list_transfers` |
| POST | `/v1/transfers/{id}/reversals` | `transfers.star#on_reverse_transfer` |
| GET | `/v1/transfers/{id}/reversals` | `transfers.star#on_list_transfer_reversals` |
| GET | `/v1/transfers/{id}/reversals/{tr_id}` | `transfers.star#on_retrieve_transfer_reversal` |
| POST | `/v1/payouts` | `payouts.star#on_create_payout` |
| GET | `/v1/payouts` | `payouts.star#on_list_payouts` |
| GET | `/v1/payouts/{id}` | `payouts.star#on_retrieve_payout` |
| POST | `/v1/payouts/{id}` | `payouts.star#on_update_payout` |
| POST | `/v1/payouts/{id}/cancel` | `payouts.star#on_cancel_payout` |

Any unmatched route returns `404 {"error":"resource_not_found"}`.

List endpoints Stripe documents as newest-first return the most recently
created objects first, like the real API.

### Deleted customers (soft delete)

Real Stripe never destroys a customer object: `DELETE /v1/customers/{id}`
marks it deleted (`200 {"deleted": true}` + signed `customer.deleted`), the
object stays retrievable, lists exclude it, mutations 404, and a
`starting_after` cursor naming it is stale (`400`) — the full observable
behavior.

## Backing stores

| Collection | Seed fixture |
|------------|-------------|
| `charges`, `customers`, `connect_accounts` | `fixtures/*.jsonl` |
| `payment_intents`, `payment_methods`, `refunds`, `tokens`, `events`, `test_clocks`, `disputes`, `balance_transactions`, `application_fees` | — |
| `products`, `prices`, `subscriptions`, `usage_records`, `invoice_items`, `invoices`, `credit_notes`, `coupons`, `promotion_codes`, `tax_rates` | — |
| `checkout_sessions`, `setup_intents`, `webhook_endpoints`, `files`, `file_links` | — |
| `persons`, `external_accounts`, `transfer_reversals`, `transfers`, `payouts` | — |

IDs use provider-style prefixes (`ch_`, `pi_`, `in_`, `sub_`, `dp_`, `txn_`,
`cs_`, `seti_`, `fee_`, `po_`, `trr_`, …) via a KV-backed sequence counter.
Per-account Connect balances (`bal_*`), the test-clock offset (`tc_offset`),
and idempotency replays (`idem_*`) live in the KV store.

## Layout

```
adapter.yaml                    Manifest: endpoints, resources, rules, identity
scripts/                        29 handler scripts + lib.star (shared helpers)
fixtures/                       Seed data (charges, customers, connect_accounts)
templates/ schemas/             Example response + charge JSON Schema
```

## Usage

```yaml
services:
  stripe:
    adapter: ./adapters/stripe-style
    config:
      webhook_url: http://localhost:9090/webhook   # optional
```

Then `stunt up` and make requests to the served address.
