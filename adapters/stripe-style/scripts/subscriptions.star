# Subscription handlers — Stripe Billing subscriptions
# (docs.stripe.com/api/subscriptions) plus the lifecycle engine that turns a
# subscription's items into invoices on the clock.
#
# MODEL
#   The subscription doc carries its items EMBEDDED (SUBSCRIPTION DOC
#   CONTRACT); /v1/subscription_items endpoints project from them. Metered
#   usage lives in the usage_records collection keyed by subscription_item id
#   (scripts/subscription_items.star).
#
#   Lifecycle is DERIVED ON READ: every subscription read (single or list)
#   first runs _advance_subscription, which — while _now() has passed a
#   billing boundary —
#     * converts a finished trial (status trialing -> active/past_due, first
#       invoice covering [trial_end, trial_end + interval)),
#     * cancels at current_period_end when cancel_at_period_end is set
#       (status canceled + customer.subscription.deleted), or
#     * renews (period += interval, new invoice via lib._subscription_invoice,
#       auto-charged per the card-behavior rules: decline card -> past_due +
#       open invoice + invoice.payment_failed + charge.failed; no payment
#       method -> past_due + open invoice; success -> paid invoice +
#       charge.succeeded + balance transaction).
#   State is persisted BEFORE any event fires; each transition emits once.
#   past_due subscriptions freeze (dunning/retries are not simulated).
#
#   Documented simplifications vs real Stripe: proration_behavior is accepted
#   but always treated as "none"; a failed first payment at creation yields
#   status past_due (real Stripe: incomplete under the default
#   payment_behavior); metered lines are billed strictly in arrears (skipped
#   when zero usage was reported); coupon duration "repeating" ends after
#   duration_in_months invoices (one per period).
#
# Shared helpers (_require_auth, _next_id, _not_found, _get_query,
# _created_filters, _created_check, _newest_first, _list_page,
# _idempotent_lookup, _idempotent_remember, _now, _num, _to_int, _usd,
# _card_number_for, _card_outcome, _charge_settle_hooks, _subscription_invoice,
# _invoice_public, _add_months, _signed_emit) are in lib.star.

# _sub_bad_body reports a malformed JSON body authoritatively: a body that
# fails to parse arrives as an EMPTY dict via req.body, so req.raw_body is
# the source of truth.
def _sub_bad_body(req):
    raw = req.get("raw_body", "")
    if raw == None or raw == "":
        return False
    return json_safe_decode(raw) == None

def _sub_missing(param):
    return respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: " + param + ".", "param": param}})

# _sub_last_item_error is the real Stripe 400 for deleting the last item.
def _sub_last_item_error():
    return respond(400, {"error": {"type": "invalid_request_error", "message": "Could not delete the last subscription item on a subscription. Cancel the subscription instead using the cancel API.", "param": "subscription"}})

# _sub_no_pm_error is the real Stripe 400 for creating a charge_automatically
# subscription when neither the subscription nor the customer has a payment
# method (webhook receivers see this verbatim in the wild).
def _sub_no_pm_error():
    return respond(400, {"error": {"type": "invalid_request_error", "message": "This customer has no attached payment source or default payment method. Please consider adding a default payment method."}})

def _sub_get(id):
    return store_collection("subscriptions").get(id)

# _sub_public renders the stored subscription doc (docs.stripe.com/api/
# subscriptions/object), stripping internal "_" keys. Items are embedded on
# the doc in public shape already.
def _sub_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    return out

# _sub_interval returns (interval, interval_count) of the first item's price.
_SUB_WEEK = 7 * 24 * 3600
_SUB_DAY = 24 * 3600

def _sub_interval(doc):
    items = doc.get("items", [])
    if items == None or len(items) == 0:
        return "month", 1
    price = items[0].get("price", None)
    if price == None:
        return "month", 1
    rec = price.get("recurring", None)
    if rec == None:
        return "month", 1
    count = _num(rec.get("interval_count", 1))
    if count < 1:
        count = 1
    return rec.get("interval", "month"), count

# _sub_add_period shifts ts forward one billing period.
def _sub_add_period(ts, interval, count):
    if interval == "day":
        return ts + _SUB_DAY * count
    if interval == "week":
        return ts + _SUB_WEEK * count
    if interval == "year":
        return _add_months(ts, 12 * count)
    return _add_months(ts, count)

# --- usage aggregation (usage_records collection, written by
#     subscription_items.star) ---

def _sub_usage_records(item_id):
    docs = store_collection("usage_records").list()
    return query_select(docs, [["subscription_item", "=", item_id]])

# _sub_usage_quantity aggregates the usage records of one metered item over
# [m_start, m_end): summed quantities (Stripe's default aggregate and the
# simulator's fallback), or the quantity of the last record ever when the
# price sets aggregate_usage=last_ever.
def _sub_usage_quantity(item_id, m_start, m_end, aggregate):
    recs = _sub_usage_records(item_id)
    if len(recs) == 0:
        return 0
    if aggregate == "last_ever":
        best = None
        best_ts = -1
        for i in range(len(recs)):
            ts = _num(recs[i].get("timestamp", 0))
            if ts >= best_ts:
                best_ts = ts
                best = recs[i]
        if best == None:
            return 0
        return _num(best.get("quantity", 0))
    total = 0
    for i in range(len(recs)):
        ts = _num(recs[i].get("timestamp", 0))
        if ts >= m_start and ts < m_end:
            total = total + _num(recs[i].get("quantity", 0))
    return total

# _sub_unit_label renders the "/ month" style suffix of a line description.
def _sub_unit_label(interval, count):
    if count > 1:
        return " / " + str(count) + " " + interval + "s"
    return " / " + interval

# _sub_lines builds the lib._subscription_invoice line dicts for one cycle:
#   licensed items -> one line each, period [p_start, p_end), upfront;
#   metered items  -> one line each for the usage reported in [m_start,
#                     p_start) (billed in arrears; skipped at zero usage).
def _sub_lines(doc, p_start, p_end, m_start):
    lines = []
    items = doc.get("items", [])
    if items == None:
        return lines
    for i in range(len(items)):
        item = items[i]
        price = item.get("price", None)
        if price == None:
            continue
        amount = _num(price.get("unit_amount", 0))
        interval, count = "month", 1
        usage_type = "licensed"
        aggregate = None
        rec = price.get("recurring", None)
        if rec != None:
            interval = rec.get("interval", "month")
            count = _num(rec.get("interval_count", 1))
            usage_type = rec.get("usage_type", "licensed")
            aggregate = rec.get("aggregate_usage", None)
        prod = store_collection("products").get(price.get("product", ""))
        name = price.get("id", "")
        if prod != None and prod.get("name", None) != None:
            name = prod["name"]
        if usage_type == "metered":
            qty = _sub_usage_quantity(item.get("id", ""), m_start, p_start, aggregate)
            if qty <= 0:
                continue
            lines.append({
                "type": "subscription",
                "description": "Usage-based " + name + " (at " + _usd(amount) + _sub_unit_label(interval, count) + ")",
                "amount": amount,
                "quantity": qty,
                "period": {"start": m_start, "end": p_start},
                "price": price,
            })
        else:
            qty = _num(item.get("quantity", 1))
            if qty < 1:
                qty = 1
            lines.append({
                "type": "subscription",
                "description": str(qty) + " × " + name + " (at " + _usd(amount) + _sub_unit_label(interval, count) + ")",
                "amount": amount,
                "quantity": qty,
                "period": {"start": p_start, "end": p_end},
                "price": price,
            })
    return lines

def _sub_subtotal(lines):
    subtotal = 0
    for i in range(len(lines)):
        subtotal = subtotal + _num(lines[i].get("amount", 0)) * _num(lines[i].get("quantity", 1))
    return subtotal

# _sub_discount_amt computes the discount cents for one invoice over
# `subtotal` (COUPON CONTRACT): percent_off -> int(subtotal*pct/100 + 0.5);
# amount_off capped at the subtotal.
def _sub_discount_amt(doc, subtotal):
    d = doc.get("discount", None)
    if d == None:
        return 0
    pct = d.get("percent_off", None)
    if pct != None and _num(pct) > 0:
        return int(subtotal * _num(pct) / 100.0 + 0.5)
    amt = _num(d.get("amount_off", 0))
    if amt > subtotal:
        return subtotal
    return amt

# _sub_tax computes (tax_cents, inclusive) over the post-discount base from
# the subscription's default_tax_rates (TAX RATE CONTRACT): cents per rate =
# int(base * percentage / 100.0 + 0.5); exclusive rates add to the total,
# inclusive rates are shown only. With any exclusive rate present the
# invoice is treated as tax-exclusive overall (mixed sets collapse — the
# shared lib helper takes a single inclusive flag).
def _sub_tax(doc, base):
    rates = doc.get("default_tax_rates", [])
    if rates == None or len(rates) == 0:
        return 0, True
    tax = 0
    inclusive = True
    for i in range(len(rates)):
        r = store_collection("tax_rates").get(rates[i])
        if r == None:
            continue
        pct = r.get("percentage", 0)
        tax = tax + int(base * _num(pct) / 100.0 + 0.5)
        if r.get("inclusive", False) != True:
            inclusive = False
    return tax, inclusive

# _sub_pm_id resolves the payment method that pays this subscription's
# invoices: the subscription's default_payment_method first, then the
# customer's invoice_settings.default_payment_method / default_source.
def _sub_pm_id(doc):
    pm = doc.get("default_payment_method", None)
    if pm != None and pm != "":
        return pm
    cust = store_collection("customers").get(doc.get("customer", ""))
    if cust == None:
        return None
    ins = cust.get("invoice_settings", None)
    if ins != None and type(ins) == "dict":
        cpm = ins.get("default_payment_method", None)
        if cpm != None and cpm != "":
            return cpm
    dsrc = cust.get("default_source", None)
    if dsrc != None and dsrc != "":
        return dsrc
    return None

def _sub_pm_exists(pm_id):
    if pm_id == None or pm_id == "":
        return False
    if store_collection("payment_methods").get(pm_id) != None:
        return True
    if store_collection("tokens").get(pm_id) != None:
        return True
    return False

# _sub_mark_paid mutates an invoice doc to its paid state.
def _sub_mark_paid(inv, now):
    inv["status"] = "paid"
    inv["paid"] = True
    inv["attempted"] = True
    inv["amount_paid"] = _num(inv.get("total", 0))
    inv["amount_remaining"] = 0
    st = inv.get("status_transitions", None)
    if st == None:
        st = {}
    st["paid_at"] = now
    inv["status_transitions"] = st
    return inv

# _sub_charge creates the charge object for a subscription invoice payment
# and records its balance transaction via the shared settlement hooks (fee +
# dispute test-card behavior included). Persisted here. Returns the charge.
def _sub_charge(doc, inv, number, outcome):
    now = _now()
    ch = {
        "id": _next_id("ch"),
        "object": "charge",
        "amount": _num(inv.get("total", 0)),
        "currency": inv.get("currency", "usd"),
        "customer": doc.get("customer", None),
        "description": None,
        "invoice": inv.get("id", None),
        "subscription": doc.get("id", None),
        "refunded": False,
        "created": now,
    }
    if outcome != None:
        # Decline (or an SCA card, which cannot complete off-session): the
        # charge object is still recorded with status failed, like real
        # Stripe, and the subscription moves to past_due.
        ch["status"] = "failed"
        ch["captured"] = False
        ch["failure_code"] = outcome.get("decline_code", "card_declined")
        ch["failure_message"] = outcome.get("message", "Your card was declined.")
        store_collection("charges").insert(ch)
        return ch
    ch["status"] = "succeeded"
    ch["captured"] = True
    ch["balance_transaction"] = None
    ch["dispute"] = None
    store_collection("charges").insert(ch)
    _charge_settle_hooks(ch, {}, number)
    return ch

# _sub_attempt_invoice attempts payment of an OPEN invoice per the contract:
#   total <= 0                          -> paid, no charge
#   collection_method send_invoice      -> left open with a due_date
#   no resolvable payment method        -> past_due, invoice stays open,
#                                          invoice.payment_failed
#   decline/SCA card                    -> failed charge + past_due + open
#                                          invoice + charge.failed +
#                                          invoice.payment_failed
#   otherwise                           -> paid + charge.succeeded +
#                                          invoice.paid/payment_succeeded
# Every mutation is persisted BEFORE its events fire. `announce` controls the
# customer.subscription.updated emission (renewals announce; the creation
# path emits customer.subscription.created instead).
def _sub_attempt_invoice(doc, inv, announce):
    subs = store_collection("subscriptions")
    invs = store_collection("invoices")
    now = _now()
    total = _num(inv.get("total", 0))
    doc["latest_invoice"] = inv.get("id", None)

    if total <= 0:
        _sub_mark_paid(inv, now)
        invs.update(inv["id"], inv)
        if doc.get("status", "") == "trialing":
            doc["status"] = "active"
        subs.update(doc["id"], doc)
        pub = _invoice_public(inv)
        _signed_emit("invoice.paid", pub)
        _signed_emit("invoice.payment_succeeded", pub)
        if announce:
            _signed_emit("customer.subscription.updated", _sub_public(doc))
        return

    if doc.get("collection_method", "charge_automatically") == "send_invoice":
        days = _num(doc.get("days_until_due", 0))
        if days <= 0:
            days = 30
        inv["due_date"] = now + days * _SUB_DAY
        invs.update(inv["id"], inv)
        if doc.get("status", "") == "trialing":
            doc["status"] = "active"
        subs.update(doc["id"], doc)
        if announce:
            _signed_emit("customer.subscription.updated", _sub_public(doc))
        return

    pm = _sub_pm_id(doc)
    if pm == None:
        inv["attempted"] = True
        invs.update(inv["id"], inv)
        doc["status"] = "past_due"
        subs.update(doc["id"], doc)
        _signed_emit("invoice.payment_failed", _invoice_public(inv))
        if announce:
            _signed_emit("customer.subscription.updated", _sub_public(doc))
        return

    number = _card_number_for(pm)
    outcome = _card_outcome(number)
    if outcome != None and outcome.get("kind", "") == "sca_sdk":
        outcome = {"kind": "decline", "decline_code": "authentication_required", "message": "This payment requires authentication to complete."}
    if outcome != None and outcome.get("kind", "") == "sca_redirect":
        outcome = {"kind": "decline", "decline_code": "authentication_required", "message": "This payment requires authentication to complete."}
    if outcome != None:
        ch = _sub_charge(doc, inv, number, outcome)
        inv["attempted"] = True
        invs.update(inv["id"], inv)
        doc["status"] = "past_due"
        subs.update(doc["id"], doc)
        _signed_emit("charge.failed", ch)
        _signed_emit("invoice.payment_failed", _invoice_public(inv))
        if announce:
            _signed_emit("customer.subscription.updated", _sub_public(doc))
        return

    ch = _sub_charge(doc, inv, number, None)
    _sub_mark_paid(inv, now)
    inv["charge"] = ch["id"]
    invs.update(inv["id"], inv)
    if doc.get("status", "") == "trialing":
        doc["status"] = "active"
    elif doc.get("status", "") == "past_due":
        doc["status"] = "active"
    subs.update(doc["id"], doc)
    _signed_emit("charge.succeeded", ch)
    pub = _invoice_public(inv)
    _signed_emit("invoice.paid", pub)
    _signed_emit("invoice.payment_succeeded", pub)
    if announce:
        _signed_emit("customer.subscription.updated", _sub_public(doc))

# _sub_drop_discount removes a spent discount before invoice n is built
# (COUPON CONTRACT): duration "once" discounts only the first invoice;
# "repeating" lasts duration_in_months invoices.
def _sub_drop_discount(doc, n):
    d = doc.get("discount", None)
    if d == None:
        return
    duration = d.get("duration", "once")
    if duration == "once" and n > 1:
        doc["discount"] = None
    elif duration == "repeating":
        months = _num(d.get("duration_in_months", 0))
        if months > 0 and n > months:
            doc["discount"] = None

# _sub_issue_invoice builds, persists and pays the invoice for one billing
# cycle (lib._subscription_invoice emits invoice.created), then attempts
# payment. n is the invoice ordinal (1 = first invoice). announce controls
# the customer.subscription.updated emission (creation announces
# customer.subscription.created instead).
def _sub_issue_invoice(doc, p_start, p_end, m_start, n, announce):
    lines = _sub_lines(doc, p_start, p_end, m_start)
    subtotal = _sub_subtotal(lines)
    disc = _sub_discount_amt(doc, subtotal)
    tax, inclusive = _sub_tax(doc, subtotal - disc)
    inv = _subscription_invoice(doc, lines, disc, tax, inclusive)
    if n <= 1 and doc.get("collection_method", "charge_automatically") == "send_invoice":
        # send_invoice subscriptions start with a DRAFT first invoice that is
        # emailed to the customer later; renewals finalize to open + due_date.
        inv["status"] = "draft"
        store_collection("invoices").update(inv["id"], inv)
        doc["latest_invoice"] = inv["id"]
        if doc.get("status", "") == "trialing":
            doc["status"] = "active"
        store_collection("subscriptions").update(doc["id"], doc)
        if announce:
            _signed_emit("customer.subscription.updated", _sub_public(doc))
        return
    _sub_attempt_invoice(doc, inv, announce)

# _sub_renew moves the subscription into the cycle that starts at p_start
# (metered usage reported in [m_start, p_start) is billed on this invoice),
# persists the period fields, then issues + attempts the invoice. Returns
# without invoicing when the subscription should end instead.
def _sub_renew(doc, p_start, m_start):
    interval, count = _sub_interval(doc)
    p_end = _sub_add_period(p_start, interval, count)
    n = _num(doc.get("_period_no", 1)) + 1
    doc["_period_no"] = n
    doc["current_period_start"] = p_start
    doc["current_period_end"] = p_end
    if doc.get("status", "") == "trialing":
        doc["status"] = "active"
    _sub_drop_discount(doc, n)
    store_collection("subscriptions").update(doc["id"], doc)
    _sub_issue_invoice(doc, p_start, p_end, m_start, n, True)

# _advance_subscription derives a subscription's state from the clock
# (derive-on-read). Call before ANY read or mutation. Idempotent: settled
# boundaries never re-fire (the period advances, the status flips, or the
# subscription cancels on the first pass).
def _advance_subscription(doc):
    if doc.get("status", "") == "canceled":
        return doc
    rounds = 0
    while rounds < 120:
        rounds = rounds + 1
        now = _now()
        status = doc.get("status", "")
        if status == "past_due":
            # Frozen: payment retries/dunning are not simulated. Updating
            # default_payment_method re-attempts the open invoice.
            break
        if status == "trialing":
            te = _num(doc.get("trial_end", 0))
            if te <= 0 or now < te:
                break
            _sub_renew(doc, te, _num(doc.get("start_date", 0)))
            continue
        cpe = _num(doc.get("current_period_end", 0))
        if cpe <= 0 or now < cpe:
            break
        if doc.get("cancel_at_period_end", False) == True:
            doc["status"] = "canceled"
            doc["canceled_at"] = cpe
            doc["ended_at"] = cpe
            doc["cancel_at_period_end"] = False
            store_collection("subscriptions").update(doc["id"], doc)
            _signed_emit("customer.subscription.deleted", _sub_public(doc))
            break
        _sub_renew(doc, cpe, _num(doc.get("current_period_start", 0)))
    return doc

# _sub_new_item builds an embedded subscription item (SUBSCRIPTION DOC
# CONTRACT) around a stored price doc.
def _sub_new_item(sub_id, price_doc, quantity, tax_rates):
    return {
        "id": _next_id("si"),
        "object": "subscription_item",
        "created": _now(),
        "price": price_doc,
        "quantity": quantity,
        "subscription": sub_id,
        "tax_rates": tax_rates,
    }

# _sub_items_from_body validates the create/update items array against the
# prices collection. Returns (items, error_response). Legacy top-level
# price+quantity is normalized into a single-item array by the caller.
def _sub_items_from_body(sub_id, raw_items):
    out = []
    for i in range(len(raw_items)):
        e = raw_items[i]
        if e == None:
            continue
        price_id = e.get("price", None)
        if price_id == None or price_id == "":
            return None, _sub_missing("items[" + str(i) + "][price]")
        price_doc = store_collection("prices").get(price_id)
        if price_doc == None:
            return None, _not_found("price", price_id)
        rec = price_doc.get("recurring", None)
        if rec == None:
            return None, respond(400, {"error": {"type": "invalid_request_error", "message": "The price specified is set to `type=one_time` but this field only accepts prices with `type=recurring`.", "param": "items[" + str(i) + "][price]"}})
        qty = _num(e.get("quantity", 1))
        if qty < 1:
            qty = 1
        tr = e.get("tax_rates", [])
        if tr == None:
            tr = []
        out.append(_sub_new_item(sub_id, price_doc, qty, tr))
    if len(out) == 0:
        return None, _sub_missing("items")
    return out, None

# _sub_discount_from_body resolves the coupon / promotion_code request
# params into the discount object stored on the subscription (COUPON
# CONTRACT: the coupon's public fields plus the promotion_code id). Reads
# the coupons/promotion_codes collections written by the billing domain.
def _sub_discount_from_body(body):
    coupon_id = body.get("coupon", None)
    pc_id = body.get("promotion_code", None)
    if coupon_id == None and pc_id == None:
        return None, None
    promo = None
    if pc_id != None and pc_id != "":
        pc = store_collection("promotion_codes").get(pc_id)
        if pc == None:
            pcs = query_select(store_collection("promotion_codes").list(), [["code", "=", pc_id]])
            if len(pcs) > 0:
                pc = pcs[0]
        if pc == None:
            return None, _not_found("promotion_code", pc_id)
        promo = pc.get("id", None)
        coupon_id = pc.get("coupon", None)
    if coupon_id == None or coupon_id == "":
        return None, _sub_missing("coupon")
    c = store_collection("coupons").get(coupon_id)
    if c == None:
        return None, _not_found("coupon", coupon_id)
    d = {}
    for k in c:
        if k.startswith("_"):
            continue
        d[k] = c[k]
    d["promotion_code"] = promo
    return d, None

# _sub_tax_rates_from_body validates the default_tax_rates array against the
# tax_rates collection.
def _sub_tax_rates_from_body(body):
    raw = body.get("default_tax_rates", None)
    if raw == None:
        return [], None
    if type(raw) != "list":
        return None, respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid array: default_tax_rates must be an array of tax rate IDs.", "param": "default_tax_rates"}})
    out = []
    for i in range(len(raw)):
        rid = raw[i]
        if store_collection("tax_rates").get(rid) == None:
            return None, _not_found("tax_rate", rid)
        out.append(rid)
    return out, None

# POST /v1/subscriptions — create a subscription (docs.stripe.com/api/
# subscriptions/create). items[{price, quantity}] (or legacy top-level
# price+quantity), default_payment_method, cancel_at_period_end, trial_end,
# collection_method, coupon / promotion_code, default_tax_rates,
# billing_cycle_anchor, days_until_due, test_clock, metadata.
#
# charge_automatically + no trial: invoice #1 is created and paid inline per
# the card rules (valid card -> paid; decline card -> past_due with the
# failed charge recorded; NO payment method at all -> the real Stripe 400
# and nothing is created). send_invoice: draft invoice #1, active
# subscription. trialing: no invoice until the trial ends.
def on_create_subscription(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "subscriptions")
    if cached != None:
        return respond(cached["status"], _sub_public(cached["doc"]))

    if _sub_bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})
    body = req["body"]
    if body == None:
        body = {}

    customer = body.get("customer", None)
    if customer == None or customer == "":
        return _sub_missing("customer")
    if store_collection("customers").get(customer) == None:
        return _not_found("customer", customer)

    raw_items = body.get("items", None)
    if raw_items == None:
        price_id = body.get("price", None)
        if price_id == None or price_id == "":
            return _sub_missing("items")
        raw_items = [{"price": price_id, "quantity": body.get("quantity", 1)}]
    if type(raw_items) != "list":
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid array: items must be an array of subscription items.", "param": "items"}})

    sub_id = _next_id("sub")
    items, ierr = _sub_items_from_body(sub_id, raw_items)
    if ierr != None:
        return ierr

    discount, derr = _sub_discount_from_body(body)
    if derr != None:
        return derr
    tax_rates, terr = _sub_tax_rates_from_body(body)
    if terr != None:
        return terr

    collection_method = body.get("collection_method", "charge_automatically")
    if collection_method != "charge_automatically" and collection_method != "send_invoice":
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid collection_method: must be one of charge_automatically or send_invoice.", "param": "collection_method"}})

    now = _now()
    anchor = body.get("billing_cycle_anchor", None)
    if anchor == None or _num(anchor) <= 0:
        anchor = now
    anchor = _num(anchor)

    trial_end = body.get("trial_end", None)
    trial_period_days = body.get("trial_period_days", None)
    if trial_end == "now":
        trial_end = None
    if trial_end != None and _num(trial_end) <= 0:
        trial_end = None
    if trial_end == None and trial_period_days != None and _num(trial_period_days) > 0:
        trial_end = anchor + _num(trial_period_days) * _SUB_DAY
    trialing = trial_end != None and _num(trial_end) > now

    interval, count = _sub_interval({"items": items})
    period_end = _sub_add_period(anchor, interval, count)

    pm = body.get("default_payment_method", None)
    if pm != None and pm != "" and not _sub_pm_exists(pm):
        return _not_found("payment_method", pm)
    if pm == None or pm == "":
        pm = None

    status = "active"
    if trialing:
        status = "trialing"
    elif collection_method == "charge_automatically" and _sub_pm_id({"customer": customer, "default_payment_method": pm}) == None:
        # Real Stripe rejects the create outright when there is nothing to
        # charge and no trial to defer to.
        return _sub_no_pm_error()

    days_until_due = body.get("days_until_due", None)

    doc = {
        "id": sub_id,
        "object": "subscription",
        "application": None,
        "automatic_tax": {"enabled": False, "liability": None},
        "billing_cycle_anchor": anchor,
        "current_period_start": anchor,
        "current_period_end": period_end,
        "cancel_at": None,
        "cancel_at_period_end": body.get("cancel_at_period_end", False) == True,
        "canceled_at": None,
        "collection_method": collection_method,
        "created": now,
        "currency": items[0].get("price", {}).get("currency", "usd"),
        "customer": customer,
        "days_until_due": days_until_due,
        "default_payment_method": pm,
        "default_source": None,
        "default_tax_rates": tax_rates,
        "description": body.get("description", None),
        "discount": discount,
        "ended_at": None,
        "items": items,
        "latest_invoice": None,
        "livemode": False,
        "metadata": body.get("metadata", {}),
        "next_pending_invoice_item_invoice": None,
        "on_behalf_of": None,
        "pause_collection": None,
        "payment_settings": {"payment_method_options": None, "payment_method_types": None, "save_default_payment_method": "off"},
        "pending_update": None,
        "schedule": None,
        "start_date": now,
        "status": status,
        "test_clock": body.get("test_clock", None),
        "trial_end": trial_end,
        "trial_start": None,
        "_period_no": 1,
    }
    if trialing:
        doc["trial_start"] = now
        doc["_period_no"] = 0
        store_collection("subscriptions").insert(doc)
        _idempotent_remember(req, "subscriptions", 201, sub_id)
        _signed_emit("customer.subscription.created", _sub_public(doc))
        return respond(201, _sub_public(doc))

    store_collection("subscriptions").insert(doc)
    _idempotent_remember(req, "subscriptions", 201, sub_id)
    _signed_emit("customer.subscription.created", _sub_public(doc))
    # First invoice for [anchor, period_end); no prior usage exists, so the
    # metered window is empty.
    _sub_issue_invoice(doc, anchor, period_end, anchor, 1, False)
    return respond(201, _sub_public(doc))

# GET /v1/subscriptions/{id} — retrieve a subscription (advanced first).
def on_retrieve_subscription(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = _sub_get(id)
    if doc == None:
        return _not_found("subscription", id)
    doc = _advance_subscription(doc)
    return respond(200, _sub_public(doc))

# _sub_filters maps the subscription-list query params (customer, status,
# created exact/range) to query_select clauses; the price filter (any item on
# the subscription using that price) is applied afterwards by hand because
# query_select cannot look inside the embedded items array.
def _sub_filters(req, docs):
    f = []
    cust = _get_query(req, "customer")
    if cust != "":
        f.append(["customer", "=", cust])
    status = _get_query(req, "status")
    if status != "":
        f.append(["status", "=", status])
    _created_filters(req, f)
    if len(f) > 0:
        docs = query_select(docs, f)
    price = _get_query(req, "price")
    if price != "":
        out = []
        for i in range(len(docs)):
            items = docs[i].get("items", [])
            if items == None:
                continue
            for j in range(len(items)):
                p = items[j].get("price", None)
                if p != None and p.get("id", "") == price:
                    out.append(docs[i])
                    break
        docs = out
    return docs

# GET /v1/subscriptions — list subscriptions. Every subscription is advanced
# first (derive-on-read), then filtered (customer, status, price, created),
# newest-first, cursor paginated.
def on_list_subscriptions(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("subscriptions").list()
    for i in range(len(docs)):
        _advance_subscription(docs[i])
    docs = _sub_filters(req, docs)
    docs = _newest_first(docs)
    page, has_more, e = _list_page(req, docs, "subscription")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_sub_public(d) for d in page], "has_more": has_more, "url": "/v1/subscriptions"})

# _sub_retry_open_invoice re-attempts the subscription's open invoice after
# the caller set a new default payment method (past_due recovery).
def _sub_retry_open_invoice(doc):
    inv_id = doc.get("latest_invoice", None)
    if inv_id == None:
        return
    inv = store_collection("invoices").get(inv_id)
    if inv == None:
        return
    if inv.get("status", "") != "open":
        return
    if _num(inv.get("amount_remaining", 0)) <= 0:
        return
    _sub_attempt_invoice(doc, inv, True)

# POST /v1/subscriptions/{id} — update a subscription: metadata,
# cancel_at_period_end, default_payment_method, collection_method,
# days_until_due, coupon/promotion_code, default_tax_rates and the items
# array (quantity changes, new items, removals via deleted=true — validated
# so a subscription never drops its last item). proration_behavior is
# accepted (always|always_invoice|create_prorations|none) and treated as
# "none": no proration lines are generated.
def on_update_subscription(req):
    err = _require_auth(req)
    if err != None:
        return err

    if _sub_bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})
    id = req["params"]["id"]
    doc = _sub_get(id)
    if doc == None:
        return _not_found("subscription", id)
    doc = _advance_subscription(doc)
    if doc.get("status", "") == "canceled":
        # Canceled subscriptions are immutable except metadata (real Stripe
        # allows metadata updates on canceled subscriptions).
        body = req["body"]
        if body != None and body.get("metadata", None) != None:
            doc["metadata"] = body["metadata"]
            store_collection("subscriptions").update(id, doc)
        return respond(200, _sub_public(doc))

    body = req["body"]
    if body == None:
        body = {}
    changed = False

    if body.get("metadata", None) != None:
        doc["metadata"] = body["metadata"]
        changed = True
    if body.get("cancel_at_period_end", None) != None:
        want = body.get("cancel_at_period_end", False) == True
        if doc.get("cancel_at_period_end", False) != want:
            doc["cancel_at_period_end"] = want
            changed = True
    if body.get("collection_method", None) != None:
        cm = body.get("collection_method", "")
        if cm != "charge_automatically" and cm != "send_invoice":
            return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid collection_method: must be one of charge_automatically or send_invoice.", "param": "collection_method"}})
        if doc.get("collection_method", "") != cm:
            doc["collection_method"] = cm
            changed = True
    if body.get("days_until_due", None) != None:
        doc["days_until_due"] = body["days_until_due"]
        changed = True
    if body.get("description", None) != None:
        doc["description"] = body["description"]
        changed = True
    if body.get("default_tax_rates", None) != None:
        tax_rates, terr = _sub_tax_rates_from_body(body)
        if terr != None:
            return terr
        doc["default_tax_rates"] = tax_rates
        changed = True
    if body.get("coupon", None) != None or body.get("promotion_code", None) != None:
        if body.get("coupon", "") == "" and body.get("promotion_code", "") == "":
            doc["discount"] = None
            changed = True
        else:
            discount, derr = _sub_discount_from_body(body)
            if derr != None:
                return derr
            doc["discount"] = discount
            changed = True

    pm_set = False
    if body.get("default_payment_method", None) != None:
        pm = body.get("default_payment_method", "")
        if pm != "" and not _sub_pm_exists(pm):
            return _not_found("payment_method", pm)
        if pm == "":
            pm = None
        if doc.get("default_payment_method", None) != pm:
            doc["default_payment_method"] = pm
            changed = True
            pm_set = True

    raw_items = body.get("items", None)
    if raw_items != None:
        if type(raw_items) != "list":
            return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid array: items must be an array of subscription items.", "param": "items"}})
        # Removals first (deleted=true), then in-place updates, then appends.
        keep = []
        removed = {}
        for i in range(len(raw_items)):
            e = raw_items[i]
            if e == None:
                continue
            eid = e.get("id", None)
            if eid != None and e.get("deleted", False) == True:
                removed[eid] = True
        existing = doc.get("items", [])
        for rid in removed:
            ok = False
            for j in range(len(existing)):
                if existing[j].get("id", "") == rid:
                    ok = True
                    break
            if not ok:
                return _not_found("subscription_item", rid)
        for i in range(len(existing)):
            it = existing[i]
            if removed.get(it.get("id", ""), None) == True:
                continue
            keep.append(it)
        if len(removed) > 0:
            changed = True
        for i in range(len(raw_items)):
            e = raw_items[i]
            if e == None:
                continue
            eid = e.get("id", None)
            if eid != None and removed.get(eid, None) == True:
                # already handled as a removal above
                continue
            if eid == None:
                price_id = e.get("price", None)
                if price_id == None or price_id == "":
                    return _sub_missing("items[" + str(i) + "][price]")
                price_doc = store_collection("prices").get(price_id)
                if price_doc == None:
                    return _not_found("price", price_id)
                if price_doc.get("recurring", None) == None:
                    return respond(400, {"error": {"type": "invalid_request_error", "message": "The price specified is set to `type=one_time` but this field only accepts prices with `type=recurring`.", "param": "items[" + str(i) + "][price]"}})
                qty = _num(e.get("quantity", 1))
                if qty < 1:
                    qty = 1
                tr = e.get("tax_rates", [])
                if tr == None:
                    tr = []
                keep.append(_sub_new_item(doc["id"], price_doc, qty, tr))
                changed = True
                continue
            found = False
            for j in range(len(keep)):
                if keep[j].get("id", "") == eid:
                    found = True
                    if e.get("quantity", None) != None:
                        q = _num(e.get("quantity", 1))
                        if q < 1:
                            q = 1
                        if _num(keep[j].get("quantity", 1)) != q:
                            keep[j]["quantity"] = q
                            changed = True
                    if e.get("price", None) != None:
                        price_doc = store_collection("prices").get(e.get("price", ""))
                        if price_doc == None:
                            return _not_found("price", e.get("price", ""))
                        if price_doc.get("recurring", None) == None:
                            return respond(400, {"error": {"type": "invalid_request_error", "message": "The price specified is set to `type=one_time` but this field only accepts prices with `type=recurring`.", "param": "items[" + str(i) + "][price]"}})
                        keep[j]["price"] = price_doc
                        changed = True
                    if e.get("tax_rates", None) != None:
                        tr = e.get("tax_rates", [])
                        if tr == None:
                            tr = []
                        keep[j]["tax_rates"] = tr
                        changed = True
                    if e.get("metadata", None) != None:
                        keep[j]["metadata"] = e["metadata"]
                        changed = True
                    break
            if not found:
                return _not_found("subscription_item", eid)
        if len(keep) == 0:
            return _sub_last_item_error()
        doc["items"] = keep

    store_collection("subscriptions").update(id, doc)
    if changed:
        _signed_emit("customer.subscription.updated", _sub_public(doc))
    if pm_set and doc.get("status", "") == "past_due":
        _sub_retry_open_invoice(doc)
    return respond(200, _sub_public(doc))

# POST /v1/subscriptions/{id}/cancel — cancel a subscription
# (docs.stripe.com/api/subscriptions/cancel). No parameters: immediate —
# status canceled, canceled_at/ended_at stamped, one
# customer.subscription.deleted event. at_period_end=true: sets
# cancel_at_period_end instead (the cancellation lands at the next billing
# boundary via _advance_subscription). invoice_now / prorate /
# cancellation_details are accepted and ignored.
def on_cancel_subscription(req):
    err = _require_auth(req)
    if err != None:
        return err

    if _sub_bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})
    id = req["params"]["id"]
    doc = _sub_get(id)
    if doc == None:
        return _not_found("subscription", id)
    doc = _advance_subscription(doc)

    body = req["body"]
    if body == None:
        body = {}

    if body.get("at_period_end", False) == True:
        if doc.get("status", "") != "canceled" and doc.get("cancel_at_period_end", False) != True:
            doc["cancel_at_period_end"] = True
            store_collection("subscriptions").update(id, doc)
            _signed_emit("customer.subscription.updated", _sub_public(doc))
        return respond(200, _sub_public(doc))

    if doc.get("status", "") != "canceled":
        now = _now()
        doc["status"] = "canceled"
        doc["canceled_at"] = now
        doc["ended_at"] = now
        doc["cancel_at_period_end"] = False
        store_collection("subscriptions").update(id, doc)
        _signed_emit("customer.subscription.deleted", _sub_public(doc))
    return respond(200, _sub_public(doc))
