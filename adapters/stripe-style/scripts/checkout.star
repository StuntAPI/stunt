# Checkout Sessions handlers — Stripe's hosted payment page
# (docs.stripe.com/api/checkout/sessions).
#
# Modes: payment | subscription | setup. The session doc follows the real
# checkout.session shape (amount_subtotal/amount_total computed from line
# items, currency, payment_status unpaid|paid|no_payment_required, status
# open|complete|expired, url).
#
# COMPLETION TRIGGER: real Stripe completes a session inside the hosted UI
# (the customer types their card there). This mock exposes that moment over
# HTTP: every open session carries url "/c/pay/{id}" (relative; the test
# client GETs <engine base> + path) and GET /c/pay/{id} — NO auth, hosted-page
# semantics — completes it:
#   payment mode      -> PaymentIntent (succeeded) + captured Charge + balance
#                        transaction (existing shapes from
#                        payment_intents.star/charges.star), then 302 to
#                        success_url with {CHECKOUT_SESSION_ID} substituted.
#   subscription mode -> Subscription doc created DIRECTLY in active state per
#                        the shared SUBSCRIPTION DOC CONTRACT, with its first
#                        invoice paid (lib._subscription_invoice + paid
#                        transition), plus the backing PI/charge.
#   setup mode        -> SetupIntent created and succeeded.
# checkout.session.completed and the underlying events (payment_intent.*,
# charge.created, customer.subscription.created, invoice.paid,
# setup_intent.succeeded, ...) are emitted via _signed_emit.
#
# ?payment_method=<tok_*|pm_*> on the pay URL drives the card outcome through
# the lib test-card behavior: a decline card fails the completion exactly like
# a card decline (402 card_error envelope; session stays open with
# payment_status unpaid; payment_intent.payment_failed /
# setup_intent.setup_failed / checkout.session.async_payment_failed emitted —
# the delayed-notification style Stripe uses when a hosted payment later
# fails). SCA cards succeed here: the hosted page runs 3DS for the customer,
# so by redirect time authentication is done (mock simplification, documented
# here rather than in the Stripe docs).
#
# Nonexistent session on the pay URL -> 404 (resource_missing semantics).
# Expired -> 200 hosted "Checkout Session expired" page (the docs: customers
# loading an expired session "see a message saying the Checkout Session is
# expired" — no redirect, no side effects). A complete session re-redirects to
# success_url WITHOUT re-running any side effect (completion is idempotent).
#
# Shared helpers (_require_auth, _next_id, _now, _signed_emit, _num, _to_int,
# _not_found, _list_page, _newest_first, _created_filters, _created_check,
# _get_query, _add_months, _card_number_for, _card_outcome,
# _card_decline_error, _charge_settle_hooks, _subscription_invoice) are in
# lib.star.

# _CK_SESSION_TTL is the default expiry horizon: 24 hours from creation (real
# Stripe default, docs.stripe.com/api/checkout/sessions/create -> expires_at).
_CK_SESSION_TTL = 24 * 3600

# _ck_public renders a stored session, stripping internal "_"-prefixed keys.
def _ck_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    return out

# _ck_get loads a session (no liveness filter: expired/complete sessions stay
# retrievable like real Stripe).
def _ck_get(id):
    return store_collection("checkout_sessions").get(id)

# _ck_advance derives the open -> expired transition from the clock
# (derive-on-read, like refunds): a session whose expires_at has passed
# flips to status expired exactly once — the state change is persisted
# BEFORE checkout.session.expired is emitted, and the status guard makes the
# transition one-shot.
def _ck_advance(doc):
    if doc.get("status", "") != "open":
        return doc
    if _now() < _num(doc.get("expires_at", 0)):
        return doc
    doc["status"] = "expired"
    doc["url"] = None
    store_collection("checkout_sessions").update(doc["id"], doc)
    _signed_emit("checkout.session.expired", _ck_public(doc))
    return doc

# _ck_product_id mints a product reference for inline product_data prices
# (name passthrough), mirroring how Checkout materializes a Product for
# price_data.product_data.
def _ck_product_for(pd):
    if pd == None:
        return None
    if type(pd) == "string":
        return pd
    if type(pd) == "dict":
        pid = pd.get("id", None)
        if pid != None:
            return pid
        return _next_id("prod")
    return _next_id("prod")

# _ck_price_from_data builds the real price object shape for a
# line_items[N].price_data payload {currency, unit_amount, product_data |
# product, recurring {interval}}.
def _ck_price_from_data(pdata, mode):
    interval = None
    rec = pdata.get("recurring", None)
    if rec != None and type(rec) == "dict":
        interval = rec.get("interval", None)
    ptype = "one_time"
    if mode == "subscription":
        ptype = "recurring"
    recurring = None
    if ptype == "recurring":
        recurring = {"aggregate_usage": None, "interval": interval, "interval_count": 1, "trial_period_days": None, "usage_type": "licensed"}
    unit_amount = _num(pdata.get("unit_amount", 0))
    return {
        "id": _next_id("price"),
        "object": "price",
        "active": True,
        "billing_scheme": "per_unit",
        "created": _now(),
        "currency": pdata.get("currency", "usd"),
        "custom_unit_amount": None,
        "livemode": False,
        "lookup_key": None,
        "metadata": {},
        "nickname": None,
        "product": _ck_product_for(pdata.get("product_data", pdata.get("product", None))),
        "recurring": recurring,
        "tax_behavior": "unspecified",
        "tiers_mode": None,
        "transform_quantity": None,
        "type": ptype,
        "unit_amount": unit_amount,
        "unit_amount_decimal": str(unit_amount),
    }

# _ck_price_public strips any internal keys a stored price doc may carry
# (prices.star owns the prices collection; stored docs may hold "_" fields).
def _ck_price_public(p):
    if p == None:
        return None
    out = {}
    for k in p:
        if k.startswith("_"):
            continue
        out[k] = p[k]
    return out

# _ck_resolve_price resolves one line item's price: an existing price id
# (looked up in the prices collection -> 400 resource_missing when unknown)
# or an inline price_data payload (materialized into the real price shape).
def _ck_resolve_price(line, mode):
    pid = line.get("price", None)
    if pid != None and type(pid) == "string":
        stored = store_collection("prices").get(pid)
        if stored == None:
            return None, respond(400, {"error": {"code": "resource_missing", "message": "No such price: '" + pid + "'", "param": "price", "type": "invalid_request_error"}})
        return _ck_price_public(stored), None
    pdata = line.get("price_data", None)
    if pdata != None and type(pdata) == "dict":
        return _ck_price_from_data(pdata, mode), None
    return None, respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: line_items[0][price].", "param": "line_items[0][price]"}})

# _ck_description names a line item from its price's product data / id.
def _ck_description(price):
    if price == None:
        return ""
    return "Subscription" if price.get("type", "") == "recurring" else "One-time purchase"

# _ck_lines resolves the request line_items into stored item dicts (the real
# session line-item shape, docs.stripe.com/api/checkout/sessions/line_items:
# id li_*, object item, amount_subtotal/amount_total, currency, description,
# price, quantity). Returns (lines, currency, subtotal, error).
def _ck_lines(body, mode):
    raw = body.get("line_items", None)
    if raw == None or type(raw) != "list" or len(raw) == 0:
        return None, None, 0, respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: line_items[0].", "param": "line_items"}})
    lines = []
    currency = None
    subtotal = 0
    for i in range(len(raw)):
        line = raw[i]
        if line == None or type(line) != "dict":
            return None, None, 0, respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: line_items[0][price].", "param": "line_items"}})
        price, err = _ck_resolve_price(line, mode)
        if err != None:
            return None, None, 0, err
        qty = _num(line.get("quantity", 1))
        if qty < 1:
            qty = 1
        unit = _num(price.get("unit_amount", 0))
        amt = unit * qty
        cur = price.get("currency", None)
        if cur != None and cur != "":
            currency = cur
        subtotal = subtotal + amt
        lines.append({
            "id": _next_id("li"),
            "object": "item",
            "amount_discount": 0,
            "amount_subtotal": amt,
            "amount_tax": 0,
            "amount_total": amt,
            "currency": cur,
            "description": _ck_description(price),
            "price": price,
            "quantity": qty,
        })
    return lines, currency, subtotal, None

# POST /v1/checkout/sessions — create a session (mode payment|subscription|
# setup, line_items [{price, quantity} | price_data {...}], success_url
# required for hosted mode, customer|customer_email, payment_method_types,
# subscription_data passthrough subset, metadata, expires_at default
# _now() + 24h).
def on_create_checkout_session(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "checkout_sessions")
    if cached != None:
        return respond(cached["status"], _ck_public(cached["doc"]))

    if _bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})

    body = req["body"]
    if body == None:
        body = {}

    mode = body.get("mode", None)
    if mode == None or mode == "":
        mode = "payment"
    if mode not in ["payment", "subscription", "setup"]:
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid mode: must be one of payment, setup, or subscription", "param": "mode"}})

    success_url = body.get("success_url", None)
    if success_url == None or success_url == "":
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: success_url.", "param": "success_url"}})

    now = _now()
    lines = []
    currency = None
    subtotal = 0
    if mode != "setup":
        lines, currency, subtotal, lerr = _ck_lines(body, mode)
        if lerr != None:
            return lerr
    if currency == None:
        currency = body.get("currency", None)
        if currency == None and mode != "setup":
            currency = "usd"

    expires_at = _num(body.get("expires_at", 0))
    if expires_at <= 0:
        expires_at = now + _CK_SESSION_TTL

    payment_status = "unpaid"
    if mode == "setup":
        payment_status = "no_payment_required"

    pm_types = body.get("payment_method_types", None)
    if pm_types == None or type(pm_types) != "list" or len(pm_types) == 0:
        pm_types = ["card"]

    sid = _next_id("cs")
    doc = {
        "id": sid,
        "object": "checkout.session",
        "amount_subtotal": subtotal,
        "amount_total": subtotal,
        "currency": currency,
        "customer": body.get("customer", None),
        "customer_email": body.get("customer_email", None),
        "mode": mode,
        "status": "open",
        "payment_status": payment_status,
        "payment_intent": None,
        "subscription": None,
        "setup_intent": None,
        "payment_method_types": pm_types,
        "success_url": success_url,
        "cancel_url": body.get("cancel_url", None),
        "url": "/c/pay/" + sid,
        "expires_at": expires_at,
        "created": now,
        "livemode": False,
        "metadata": body.get("metadata", {}),
        "client_reference_id": body.get("client_reference_id", None),
        "total_details": {"amount_discount": 0, "amount_shipping": 0, "amount_tax": 0},
        "_lines": lines,
        "_subscription_data": body.get("subscription_data", {}),
        "_paid_pm": None,
    }
    if mode == "setup":
        doc["amount_subtotal"] = None
        doc["amount_total"] = None
    store_collection("checkout_sessions").insert(doc)
    _idempotent_remember(req, "checkout_sessions", 201, sid)
    return respond(201, _ck_public(doc))

# fails to parse arrives as an EMPTY dict via req.body, so req.raw_body is
# the source of truth.
# GET /v1/checkout/sessions — list sessions (filters customer, status,
# payment_intent, subscription, created; newest first; cursor pagination).
def on_list_checkout_sessions(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("checkout_sessions").list()
    f = []
    cust = _get_query(req, "customer")
    if cust != "":
        f.append(["customer", "=", cust])
    status = _get_query(req, "status")
    if status != "":
        f.append(["status", "=", status])
    pi = _get_query(req, "payment_intent")
    if pi != "":
        f.append(["payment_intent", "=", pi])
    sub = _get_query(req, "subscription")
    if sub != "":
        f.append(["subscription", "=", sub])
    _created_filters(req, f)
    if len(f) > 0:
        docs = query_select(docs, f)

    advanced = []
    for i in range(len(docs)):
        advanced.append(_ck_advance(docs[i]))
    advanced = _newest_first(advanced)

    page, has_more, perr = _list_page(req, advanced, "checkout_session")
    if perr != None:
        return perr
    return respond(200, {"object": "list", "data": [_ck_public(d) for d in page], "has_more": has_more, "url": "/v1/checkout/sessions"})

# GET /v1/checkout/sessions/{id} — retrieve a session (derive-on-read expiry).
def on_retrieve_checkout_session(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = _ck_get(id)
    if doc == None:
        return _not_found("checkout_session", id)
    doc = _ck_advance(doc)
    return respond(200, _ck_public(doc))

# GET /v1/checkout/sessions/{id}/line_items — the session line-item shape
# (object "item", NOT the price shape), paginated.
def on_list_checkout_session_line_items(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = _ck_get(id)
    if doc == None:
        return _not_found("checkout_session", id)
    doc = _ck_advance(doc)

    lines = doc.get("_lines", [])
    if lines == None:
        lines = []
    page, has_more, perr = _list_page(req, lines, "item")
    if perr != None:
        return perr
    return respond(200, {"object": "list", "data": page, "has_more": has_more, "url": "/v1/checkout/sessions/" + id + "/line_items"})

# POST /v1/checkout/sessions/{id}/expire — open -> expired +
# checkout.session.expired (only an open session is expireable, per
# docs.stripe.com/api/checkout/sessions/expire).
def on_expire_checkout_session(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "checkout_sessions")
    if cached != None:
        return respond(cached["status"], _ck_public(cached["doc"]))

    id = req["params"]["id"]
    doc = _ck_get(id)
    if doc == None:
        return _not_found("checkout_session", id)
    doc = _ck_advance(doc)
    if doc.get("status", "") != "open":
        return respond(400, {"error": {"type": "invalid_request_error", "message": "You cannot expire this Checkout Session because it has a status of " + doc.get("status", "") + ". Only a Checkout Session with one of the following statuses may be expired: open."}})
    doc["status"] = "expired"
    doc["url"] = None
    store_collection("checkout_sessions").update(id, doc)
    _signed_emit("checkout.session.expired", _ck_public(doc))
    _idempotent_remember(req, "checkout_sessions", 200, id)
    return respond(200, _ck_public(doc))

# ============================================================================
# HOSTED-PAGE COMPLETION (GET /c/pay/{id}, no auth)
# ============================================================================

# _ck_redirect_url substitutes {CHECKOUT_SESSION_ID} in the success_url (the
# real Checkout templating parameter).
def _ck_redirect_url(doc):
    url = doc.get("success_url", "")
    if url == None:
        url = ""
    return url.replace("{CHECKOUT_SESSION_ID}", doc["id"])

# _ck_pi_doc mints the PaymentIntent doc in the payment_intents.star shape.
def _ck_pi_doc(doc, amount, status, pm, description):
    return {
        "id": _next_id("pi"),
        "object": "payment_intent",
        "amount": amount,
        "amount_capturable": 0,
        "amount_received": 0,
        "currency": doc.get("currency", "usd"),
        "status": status,
        "capture_method": "automatic",
        "payment_method": pm,
        "customer": doc.get("customer", None),
        "description": description,
        "last_payment_error": None,
        "next_action": None,
        "metadata": doc.get("metadata", {}),
        "created": _now(),
    }

# _ck_charge_for mints the captured Charge doc behind a successful
# PaymentIntent (charges.star shape) and records its balance transaction via
# the shared settlement hook. Persist-first, then charge.created.
def _ck_charge_for(pi_doc, number, description):
    ch = {
        "id": _next_id("ch"),
        "object": "charge",
        "amount": _num(pi_doc.get("amount", 0)),
        "currency": pi_doc.get("currency", "usd"),
        "customer": pi_doc.get("customer", None),
        "description": description,
        "status": "succeeded",
        "captured": True,
        "refunded": False,
        "balance_transaction": None,
        "dispute": None,
        "payment_intent": pi_doc["id"],
        "created": _now(),
    }
    store_collection("charges").insert(ch)
    pi_doc["latest_charge"] = ch["id"]
    store_collection("payment_intents").update(pi_doc["id"], pi_doc)
    _signed_emit("charge.created", ch)
    _charge_settle_hooks(ch, None, number)
    return ch

# _ck_fail_payment completes a session's payment with a declined card: the PI
# is persisted in requires_payment_method with last_payment_error (like PI
# confirm in payment_intents.star), payment_intent.payment_failed fires, the
# session stays open with payment_status unpaid, and the hosted page answers
# with the real 402 card_error envelope. The async-failure notification
# (checkout.session.async_payment_failed) is emitted as well — the style
# Stripe uses when a hosted payment ultimately fails.
def _ck_fail_payment(doc, pm, oc):
    pi = _ck_pi_doc(doc, _num(doc.get("amount_total", 0)), "requires_payment_method", pm, None)
    pi["last_payment_error"] = {
        "charge": None,
        "code": oc["code"],
        "decline_code": oc["decline_code"],
        "doc_url": "https://stripe.com/docs/error-codes/card-declined",
        "message": oc["message"],
        "payment_method": pm,
        "type": "card_error",
    }
    store_collection("payment_intents").insert(pi)
    _signed_emit("payment_intent.created", pi)
    _signed_emit("payment_intent.payment_failed", pi)
    _signed_emit("checkout.session.async_payment_failed", _ck_public(doc))
    return _card_decline_error(oc, "payment_intent", pi["id"])

# _ck_period_end computes the subscription's first current_period_end from
# the recurring interval.
def _ck_period_end(now, interval):
    if interval == "year":
        return _add_months(now, 12)
    if interval == "week":
        return now + 7 * 24 * 3600
    if interval == "day":
        return now + 24 * 3600
    return _add_months(now, 1)

# _ck_complete_subscription builds the Subscription doc directly in active
# state (SUBSCRIPTION DOC CONTRACT) with its first invoice paid, plus the
# backing PaymentIntent + Charge. Persist-first emission throughout; each
# transition fires exactly once. Returns the subscription doc.
def _ck_complete_subscription(doc, pm):
    now = _now()
    lines = doc.get("_lines", [])
    customer = doc.get("customer", None)
    if customer == None:
        # Checkout creates a Customer during the flow when none was supplied.
        customer = _next_id("cus")
        email = doc.get("customer_email", None)
        cust = {"id": customer, "object": "customer", "name": None, "email": email, "description": None, "created": now}
        store_collection("customers").insert(cust)
        _signed_emit("customer.created", cust)

    sub_data = doc.get("_subscription_data", {})
    if sub_data == None:
        sub_data = {}
    sub_meta = sub_data.get("metadata", {})
    if sub_meta == None:
        sub_meta = {}

    # Items: embedded subscription_item docs with resolved price objects.
    items = []
    line_dicts = []
    interval = "month"
    for i in range(len(lines)):
        ln = lines[i]
        price = ln.get("price", None)
        if price == None:
            continue
        qty = _num(ln.get("quantity", 1))
        rec = price.get("recurring", None)
        if rec != None and type(rec) == "dict" and rec.get("interval", None) != None:
            interval = rec["interval"]
        items.append({
            "id": _next_id("si"),
            "object": "subscription_item",
            "price": price,
            "quantity": qty,
            "subscription": None,
            "tax_rates": [],
        })
        line_dicts.append({
            "type": "subscription",
            "description": ln.get("description", ""),
            "amount": _num(price.get("unit_amount", 0)),
            "quantity": qty,
            "period": {"start": now, "end": now},
            "price": price,
        })

    period_end = _ck_period_end(now, interval)
    for i in range(len(line_dicts)):
        line_dicts[i]["period"] = {"start": now, "end": period_end}
    sub = {
        "id": _next_id("sub"),
        "object": "subscription",
        "customer": customer,
        "status": "active",
        "items": items,
        "current_period_start": now,
        "current_period_end": period_end,
        "cancel_at_period_end": False,
        "canceled_at": None,
        "ended_at": None,
        "collection_method": "charge_automatically",
        "default_payment_method": pm,
        "latest_invoice": None,
        "discount": None,
        "default_tax_rates": [],
        "start_date": now,
        "trial_end": None,
        "billing_cycle_anchor": now,
        "metadata": sub_meta,
        "test_clock": None,
        "currency": doc.get("currency", "usd"),
        "_period_no": 1,
    }
    for i in range(len(sub["items"])):
        sub["items"][i]["subscription"] = sub["id"]
    store_collection("subscriptions").insert(sub)
    _signed_emit("customer.subscription.created", sub)

    # First invoice (open) -> paid, with the backing PI + charge linked.
    inv = _subscription_invoice(sub, line_dicts, 0, 0, False)
    total = _num(inv.get("total", 0))
    pi = _ck_pi_doc(doc, total, "succeeded", pm, "Subscription creation invoice")
    pi["customer"] = customer
    pi["invoice"] = inv["id"]
    pi["amount_received"] = total
    store_collection("payment_intents").insert(pi)
    _signed_emit("payment_intent.created", pi)
    ch = _ck_charge_for(pi, "", "Subscription creation invoice")

    inv["status"] = "paid"
    inv["paid"] = True
    inv["attempted"] = True
    inv["amount_paid"] = total
    inv["amount_remaining"] = 0
    st = inv.get("status_transitions", {})
    st["paid_at"] = _now()
    inv["status_transitions"] = st
    inv["charge"] = ch["id"]
    inv["payment_intent"] = pi["id"]
    store_collection("invoices").update(inv["id"], inv)

    sub["latest_invoice"] = inv["id"]
    store_collection("subscriptions").update(sub["id"], sub)

    _signed_emit("invoice.paid", _invoice_public(inv))
    _signed_emit("invoice.payment_succeeded", _invoice_public(inv))
    _signed_emit("payment_intent.succeeded", pi)
    return sub

# _ck_complete_setup finishes a setup-mode session: SetupIntent created and
# succeeded (setup_intents.star shape).
def _ck_complete_setup(doc):
    seti = {
        "id": _next_id("seti"),
        "object": "setup_intent",
        "cancellation_reason": None,
        "client_secret": None,
        "created": _now(),
        "customer": doc.get("customer", None),
        "description": None,
        "last_setup_error": None,
        "latest_attempt": None,
        "livemode": False,
        "metadata": doc.get("metadata", {}),
        "next_action": None,
        "payment_method": None,
        "payment_method_types": doc.get("payment_method_types", ["card"]),
        "status": "requires_confirmation",
        "usage": "off_session",
    }
    seti["client_secret"] = seti["id"] + "_secret_" + str(_now())
    store_collection("setup_intents").insert(seti)
    _signed_emit("setup_intent.created", seti)
    seti["status"] = "succeeded"
    store_collection("setup_intents").update(seti["id"], seti)
    _signed_emit("setup_intent.succeeded", seti)
    return seti

# _ck_fail_setup is the setup-mode decline path: the SetupIntent persists in
# requires_payment_method with last_setup_error, setup_intent.setup_failed
# fires, and the session stays open.
def _ck_fail_setup(doc, pm, oc):
    seti = {
        "id": _next_id("seti"),
        "object": "setup_intent",
        "cancellation_reason": None,
        "client_secret": None,
        "created": _now(),
        "customer": doc.get("customer", None),
        "description": None,
        "last_setup_error": {
            "code": oc["code"],
            "decline_code": oc["decline_code"],
            "doc_url": "https://stripe.com/docs/error-codes/card-declined",
            "message": oc["message"],
            "payment_method": pm,
            "type": "card_error",
        },
        "latest_attempt": None,
        "livemode": False,
        "metadata": doc.get("metadata", {}),
        "next_action": None,
        "payment_method": pm,
        "payment_method_types": doc.get("payment_method_types", ["card"]),
        "status": "requires_payment_method",
        "usage": "off_session",
    }
    seti["client_secret"] = seti["id"] + "_secret_" + str(_now())
    store_collection("setup_intents").insert(seti)
    _signed_emit("setup_intent.created", seti)
    _signed_emit("setup_intent.setup_failed", seti)
    _signed_emit("checkout.session.async_payment_failed", _ck_public(doc))
    e = {
        "code": oc["code"],
        "decline_code": oc["decline_code"],
        "doc_url": "https://stripe.com/docs/error-codes/card-declined",
        "message": oc["message"],
        "setup_intent": seti["id"],
        "type": "card_error",
    }
    return respond(402, {"error": e})

# GET /c/pay/{id} — the hosted Checkout page stand-in (NO auth). Completing a
# session runs the mode's side effects (PI/charge, subscription + paid
# invoice, SetupIntent), persists every state change BEFORE emitting, then
# 302-redirects to success_url with {CHECKOUT_SESSION_ID} substituted.
# Optional ?payment_method=<tok_*|pm_*> drives the test-card behavior.
def on_pay_checkout_session(req):
    id = req["params"]["id"]
    doc = _ck_get(id)
    if doc == None:
        return _not_found("checkout_session", id)
    doc = _ck_advance(doc)

    status = doc.get("status", "")
    if status == "expired":
        return respond(200, "<html><body><h1>Checkout Session expired</h1><p>This Checkout Session has expired and can no longer be completed.</p></body></html>", {"Content-Type": "text/html; charset=utf-8"})
    if status == "complete":
        # Re-visiting a completed session re-redirects WITHOUT re-running any
        # side effect (completion is one-shot).
        return respond(302, "Found", {"Location": _ck_redirect_url(doc)})

    pm = _get_query(req, "payment_method")
    if pm == "":
        pm = None
    number = ""
    if pm != None:
        number = _card_number_for(pm)
    oc = None
    if number != "":
        oc = _card_outcome(number)
    if oc != None and oc["kind"] == "decline":
        doc["_paid_pm"] = pm
        store_collection("checkout_sessions").update(id, doc)
        if doc.get("mode", "") == "setup":
            return _ck_fail_setup(doc, pm, oc)
        return _ck_fail_payment(doc, pm, oc)

    mode = doc.get("mode", "payment")
    if mode == "subscription":
        sub = _ck_complete_subscription(doc, pm)
        doc["subscription"] = sub["id"]
    elif mode == "setup":
        seti = _ck_complete_setup(doc)
        doc["setup_intent"] = seti["id"]
    else:
        pi = _ck_pi_doc(doc, _num(doc.get("amount_total", 0)), "requires_payment_method", pm, None)
        pi["status"] = "succeeded"
        pi["amount_received"] = _num(doc.get("amount_total", 0))
        store_collection("payment_intents").insert(pi)
        _signed_emit("payment_intent.created", pi)
        _ck_charge_for(pi, number, None)
        _signed_emit("payment_intent.succeeded", pi)
        doc["payment_intent"] = pi["id"]

    doc["status"] = "complete"
    # setup mode never moves money: the session stays no_payment_required
    # (real Stripe's enum reserves "paid" for funds received).
    if doc.get("mode", "payment") != "setup":
        doc["payment_status"] = "paid"
    doc["url"] = None
    doc["_paid_pm"] = pm
    store_collection("checkout_sessions").update(id, doc)
    _signed_emit("checkout.session.completed", _ck_public(doc))
    return respond(302, "Found", {"Location": _ck_redirect_url(doc)})
