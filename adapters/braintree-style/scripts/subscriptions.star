# Subscription + plan handlers — Braintree recurring billing.
#
# POST /merchants/{merchantId}/plans                      → { plan: {...} }
# GET  /merchants/{merchantId}/plans                      → { plans: [...] }
# GET  /merchants/{merchantId}/plans/{id}                 → { plan: {...} }
# POST /merchants/{merchantId}/subscriptions              → { subscription: {...} }
# GET  /merchants/{merchantId}/subscriptions/{id}         → { subscription: {...} }
# POST /merchants/{merchantId}/subscriptions/{id}/cancel  → { subscription: {...} }
#
# Lifecycle (derive-on-read, compressed like the transaction machine):
#   Active -> Expired   (after numberOfBillingCycles billing cycles complete;
#                        one simulated cycle = _CYCLE_SECONDS)
#   Active -> Canceled  (POST .../cancel, immediate, terminal)
# Each completed billing cycle fires one signed
# subscription_charged_successfully notification; completing the final cycle
# also fires subscription_expired, and a cancel fires subscription_cancelled
# (Braintree's British spelling).

# One simulated billing cycle (seconds). Real Braintree bills monthly.
_CYCLE_SECONDS = 2

# _plan_public returns the Braintree-shaped plan object.
def _plan_public(doc):
    return {
        "id": doc.get("id", ""),
        "name": doc.get("name", ""),
        "price": doc.get("price", "0.00"),
        "currency": doc.get("currency", "USD"),
        "billing_frequency": doc.get("billing_frequency", "monthly"),
        "number_of_billing_cycles": _to_int(doc.get("number_of_billing_cycles", 2)),
        "created_at": doc.get("created_at", ""),
    }

# _sub_public returns the Braintree-shaped subscription object. Status values
# are the real ones: Active, Canceled, Expired (Past Due is not simulated).
def _sub_public(doc):
    total = _to_int(doc.get("_total_cycles", 0))
    done = _to_int(doc.get("_billed_cycles", 0))
    remaining = total - done
    if remaining < 0:
        remaining = 0
    return {
        "id": doc.get("id", ""),
        "status": doc.get("status", "Active"),
        "plan_id": doc.get("plan_id", ""),
        "price": doc.get("price", "0.00"),
        "currency": doc.get("currency", "USD"),
        "payment_method_token": doc.get("payment_method_token", ""),
        "billing_cycles_completed": done,
        "billing_cycles_remaining": remaining,
        "created_at": doc.get("created_at", ""),
    }

# on_create_plan creates a billing plan.
def on_create_plan(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}
    planb = body.get("plan", None)
    if planb == None or type(planb) != "dict":
        planb = body

    price = planb.get("price", planb.get("amount", "0.00"))
    p = _to_float(price)
    if p <= 0:
        return _bt_err(422, _ERR_AMOUNT, "Amount must be greater than zero")

    plan_id = planb.get("id", "")
    if plan_id == None or plan_id == "":
        plan_id = "plan_" + str(store_kv_incr("braintree", "plan_seq"))

    doc = {
        "id": plan_id,
        "name": planb.get("name", ""),
        "price": _fmt_amount(p),
        "currency": planb.get("currency", "USD"),
        "billing_frequency": planb.get("billing_frequency", "monthly"),
        "number_of_billing_cycles": _to_int(planb.get("number_of_billing_cycles", 2)),
        "created_at": clock.now_rfc3339(),
    }
    store_collection("plans").insert(doc)
    return respond(200, {"plan": _plan_public(doc)})

# on_list_plans lists all billing plans.
def on_list_plans(req):
    err = _require_auth(req)
    if err != None:
        return err

    plans = []
    for doc in store_collection("plans").list():
        plans.append(_plan_public(doc))
    return respond(200, {"plans": plans, "total_count": len(plans)})

# on_get_plan retrieves a billing plan by ID.
def on_get_plan(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc = store_collection("plans").get(req["params"].get("id", ""))
    if doc == None:
        return _bt_err(404, "NOT_FOUND", "Plan not found")
    return respond(200, {"plan": _plan_public(doc)})

# on_create_subscription subscribes a payment method to a plan.
def on_create_subscription(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}
    sub = body.get("subscription", None)
    if sub == None or type(sub) != "dict":
        sub = body

    plan_id = sub.get("plan_id", sub.get("planId", ""))
    if plan_id == None:
        plan_id = ""
    plan = store_collection("plans").get(plan_id)
    if plan == None:
        return _bt_err(404, "NOT_FOUND", "Plan not found")

    pm = sub.get("payment_method_token", sub.get("paymentMethodToken", ""))
    if pm == None or pm == "":
        pm = _payment_method_token()

    cycles = _to_int(sub.get("number_of_billing_cycles", plan.get("number_of_billing_cycles", 2)))
    if cycles <= 0:
        cycles = _to_int(plan.get("number_of_billing_cycles", 2))
    if cycles <= 0:
        cycles = 2

    now = clock.now_unix()
    doc = {
        "id": "sub_" + str(store_kv_incr("braintree", "sub_seq")),
        "status": "Active",
        "plan_id": plan_id,
        "price": plan.get("price", "0.00"),
        "currency": plan.get("currency", "USD"),
        "payment_method_token": pm,
        "created_at": clock.now_rfc3339(),
        "_created_unix": now,
        "_seconds_per_cycle": _CYCLE_SECONDS,
        "_total_cycles": cycles,
        "_billed_cycles": 0,
    }
    store_collection("subscriptions").insert(doc)
    return respond(200, {"subscription": _sub_public(doc)})

# _advance_subscription derives the subscription's billing progress from the
# clock: each completed cycle is persisted and fires one signed
# subscription_charged_successfully notification; completing the final cycle
# transitions to Expired and also fires subscription_expired. Returns the
# (possibly updated) doc.
def _advance_subscription(doc):
    if doc.get("status", "Active") != "Active":
        return doc

    c = store_collection("subscriptions")
    now = clock.now_unix()
    created = _to_int(doc.get("_created_unix", now))
    spc = _to_int(doc.get("_seconds_per_cycle", _CYCLE_SECONDS))
    total = _to_int(doc.get("_total_cycles", 2))
    done = _to_int(doc.get("_billed_cycles", 0))
    if spc <= 0:
        return doc

    while done < total and now >= created + spc * (done + 1):
        done = done + 1
        doc["_billed_cycles"] = done
        if done >= total:
            doc["status"] = "Expired"
        c.update(doc["id"], doc)
        _emit_if_subscribed("subscription_charged_successfully", {"subscription": _sub_public(doc)})
        if done >= total:
            _emit_if_subscribed("subscription_expired", {"subscription": _sub_public(doc)})
    return doc

# on_get_subscription retrieves a subscription, advancing its lifecycle
# first.
def on_get_subscription(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc = store_collection("subscriptions").get(req["params"].get("id", ""))
    if doc == None:
        return _bt_err(404, "NOT_FOUND", "Subscription not found")
    return respond(200, {"subscription": _sub_public(_advance_subscription(doc))})

# on_cancel_subscription cancels an Active subscription (immediate, terminal).
def on_cancel_subscription(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc = store_collection("subscriptions").get(req["params"].get("id", ""))
    if doc == None:
        return _bt_err(404, "NOT_FOUND", "Subscription not found")

    doc = _advance_subscription(doc)
    if doc.get("status", "") != "Active":
        return _bt_err(422, _ERR_CANCEL_STATE, "Subscription can only be canceled when status is Active")

    doc["status"] = "Canceled"
    doc["canceled_at"] = clock.now_rfc3339()
    store_collection("subscriptions").update(doc["id"], doc)
    _emit_if_subscribed("subscription_cancelled", {"subscription": _sub_public(doc)})
    return respond(200, {"subscription": _sub_public(doc)})
