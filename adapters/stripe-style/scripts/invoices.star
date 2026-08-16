# Invoice handlers — the billing document lifecycle (draft -> open -> paid /
# void / uncollectible) plus the upcoming-invoice preview.
#
# Subscription-created invoice DOCS are written by the subscriptions domain
# via lib._subscription_invoice; this file owns every invoice ENDPOINT and
# renders every invoice (manual or subscription) through lib._invoice_public.
#
# Manual invoices (POST /v1/invoices) start as drafts with either explicit
# `items` lines or zero lines. Finalize moves draft -> open (finalized_at),
# pay charges the resolved payment method through the shared test-card
# behavior (decline -> 402 card_error, invoice stays open,
# invoice.payment_failed; success -> paid + real charge + balance transaction
# + charge.succeeded + invoice.paid + invoice.payment_succeeded).
#
# Shared helpers (_require_auth, _next_id, _now, _num, _to_int, _not_found,
# _list_page, _newest_first, _created_filters, _created_check,
# _idempotent_lookup, _idempotent_remember, _invoice_public,
# _card_number_for, _card_outcome, _card_decline_error, _sca_charge_error,
# _charge_settle_hooks, _create_refund, _apply_charge_refund, _signed_emit)
# are in lib.star.

_INV_COLLECTION = "invoices"

# fails to parse arrives as an EMPTY dict via req.body, so req.raw_body is
# the source of truth.
# _inv_err builds the real Stripe error envelope with status 400.
def _inv_err(msg, param):
    e = {"type": "invalid_request_error", "message": msg}
    if param != None:
        e["param"] = param
    return respond(400, {"error": e})

# _inv_missing is the real Stripe 400 for a missing required param.
def _inv_missing(param):
    return _inv_err("Missing required param: " + param + ".", param)

# _inv_state_err is the 400 for a lifecycle call on an invoice whose status
# does not allow it (phrased like the adapter's PaymentIntent errors).
# verb is the present-tense action, participle its past form ("pay"/"paid").
def _inv_state_err(verb, participle, status, allowed):
    return _inv_err("You cannot " + verb + " this invoice because it has a status of " + status + ". Only invoices with one of the following statuses may be " + participle + ": " + allowed + ".", None)

# _inv_get loads an invoice doc or None.
def _inv_get(id):
    return store_collection(_INV_COLLECTION).get(id)

# _inv_save persists an invoice doc.
def _inv_save(doc):
    store_collection(_INV_COLLECTION).update(doc["id"], doc)

# ============================================================================
# TAX + DISCOUNT MATH (TAX CONTRACT: tax cents per rate over an amount is
# int(amount * percentage / 100.0 + 0.5); exclusive rates add to the total,
# inclusive rates are shown in `tax` but NOT added)
# ============================================================================

# _inv_tax_cents computes the tax over `amount` for a list of tax-rate ids.
# Returns [tax_cents, inclusive] where inclusive is True only when every
# rate is inclusive (mixed lists behave exclusive, funds-wise).
def _inv_tax_cents(amount, rate_ids):
    if rate_ids == None:
        return 0, False
    tax = 0
    n = 0
    inclusive_n = 0
    for i in range(len(rate_ids)):
        rid = rate_ids[i]
        if rid == None:
            continue
        doc = store_collection("tax_rates").get(rid)
        if doc == None:
            continue
        if doc.get("deleted", False) == True:
            continue
        pct = doc.get("percentage", 0)
        if pct == None:
            pct = 0
        tax = tax + int(amount * pct / 100.0 + 0.5)
        n = n + 1
        if doc.get("inclusive", False) == True:
            inclusive_n = inclusive_n + 1
    if n > 0 and inclusive_n == n:
        return tax, True
    return tax, False

# _inv_coupon_for resolves the coupon dict behind a discount: the invoice
# contract stores {coupon fields + promotion_code} flattened, subscription
# docs may store {"coupon": <id or expanded dict>, ...}. Returns None when
# no coupon can be resolved.
def _inv_coupon_for(discount):
    if discount == None or type(discount) != "dict":
        return None
    if discount.get("percent_off", None) != None or discount.get("amount_off", None) != None:
        return discount
    c = discount.get("coupon", None)
    if c == None:
        return None
    if type(c) == "dict":
        return c
    return store_collection("coupons").get(c)

# _inv_discount_amount computes the discount cents for one invoice over its
# subtotal: percent -> int(subtotal*pct/100 + 0.5), amount_off capped at
# subtotal (COUPON CONTRACT).
def _inv_discount_amount(subtotal, discount):
    c = _inv_coupon_for(discount)
    if c == None:
        return 0
    pct = c.get("percent_off", None)
    if pct != None:
        amt = int(subtotal * pct / 100.0 + 0.5)
        if amt > subtotal:
            amt = subtotal
        return amt
    off = _num(c.get("amount_off", 0))
    if off > subtotal:
        return subtotal
    if off < 0:
        return 0
    return off

# _inv_recompute rebuilds subtotal/tax/discount-free totals for an invoice
# from its lines + default_tax_rates (draft edits). Line amounts are
# per-unit (subtotal = sum(amount * quantity)), matching
# lib._subscription_invoice.
def _inv_recompute(doc):
    subtotal = 0
    for i in range(len(doc["lines"])):
        ln = doc["lines"][i]
        subtotal = subtotal + _num(ln.get("amount", 0)) * _num(ln.get("quantity", 1))
    tax, inclusive = _inv_tax_cents(subtotal, doc.get("default_tax_rates", []))
    total = subtotal + tax
    if inclusive:
        total = subtotal
    if total < 0:
        total = 0
    doc["subtotal"] = subtotal
    doc["tax"] = tax
    doc["total"] = total
    doc["amount_due"] = total
    doc["amount_remaining"] = total - _num(doc.get("amount_paid", 0))
    return doc

# _inv_currency picks the invoice currency: explicit param, else the first
# priced line's currency, else usd.
def _inv_currency(lines, wanted):
    if wanted != None and wanted != "":
        return wanted
    for i in range(len(lines)):
        price = lines[i].get("price", None)
        if price != None and price.get("currency", None) != None:
            return price["currency"]
    return "usd"

# _inv_price_doc resolves a price id to its stored doc (None when unknown).
def _inv_price_doc(price_id):
    if price_id == None or type(price_id) != "string":
        return None
    p = store_collection("prices").get(price_id)
    if p == None:
        return None
    out = {}
    for k in p:
        if k.startswith("_"):
            continue
        out[k] = p[k]
    return out

# _inv_lines_from_items builds contract line dicts from POST /v1/invoices
# `items` entries: each {unit_amount|amount, currency, description,
# quantity, price, price_data}.
def _inv_lines_from_items(items):
    lines = []
    for i in range(len(items)):
        it = items[i]
        if it == None or type(it) != "dict":
            continue
        qty = _num(it.get("quantity", 1))
        if qty < 1:
            qty = 1
        unit = _num(it.get("unit_amount", 0))
        if it.get("unit_amount", None) == None:
            unit = _num(it.get("amount", 0))
        price = None
        if it.get("price_data", None) != None and type(it["price_data"]) == "dict":
            price = dict(it["price_data"])
            price["object"] = "price"
            if price.get("id", None) == None:
                price["id"] = None
            if unit == 0:
                unit = _num(price.get("unit_amount", 0))
        elif it.get("price", None) != None:
            price = _inv_price_doc(it["price"])
            if price != None and unit == 0:
                unit = _num(price.get("unit_amount", 0))
        desc = it.get("description", None)
        if desc == None:
            desc = "Line item"
        now = _now()
        lines.append({
            "id": _next_id("il"),
            "object": "line_item",
            "type": "invoice_item",
            "description": desc,
            "amount": unit,
            "quantity": qty,
            "period": {"start": now, "end": now},
            "price": price,
            "proration": False,
            "tax_rates": [],
        })
    return lines

# _inv_new builds a draft invoice doc (INVOICE DOC CONTRACT).
def _inv_new(customer, lines, currency, collection_method, auto_advance, due_date, subscription, description, metadata, tax_rates, billing_reason):
    now = _now()
    doc = {
        "id": _next_id("in"),
        "object": "invoice",
        "customer": customer,
        "subscription": subscription,
        "status": "draft",
        "collection_method": collection_method,
        "currency": _inv_currency(lines, currency),
        "lines": lines,
        "subtotal": 0,
        "discount": None,
        "default_tax_rates": tax_rates,
        "tax": 0,
        "total": 0,
        "amount_due": 0,
        "amount_paid": 0,
        "amount_remaining": 0,
        "starting_balance": 0,
        "charge": None,
        "payment_intent": None,
        "status_transitions": {"finalized_at": None, "paid_at": None, "voided_at": None},
        "billing_reason": billing_reason,
        "due_date": due_date,
        "created": now,
        "auto_advance": auto_advance,
        "attempted": False,
        "metadata": metadata,
        "paid": None,
        "description": description,
        "_advance_scheduled": False,
    }
    return _inv_recompute(doc)

# ============================================================================
# PAYMENT-METHOD RESOLUTION (pay + subscriptions auto-charge use the same
# precedence: explicit param > invoice default > customer default)
# ============================================================================

# _inv_default_pm resolves the customer's default card-ish payment method:
# the customer doc's default_payment_method / invoice_settings.default_
# payment_method, else the newest card payment method attached to the
# customer. Returns None when the customer has none.
def _inv_default_pm(customer_id):
    if customer_id == None:
        return None
    cus = store_collection("customers").get(customer_id)
    if cus != None:
        pm = cus.get("default_payment_method", None)
        if pm == None:
            settings = cus.get("invoice_settings", None)
            if settings != None and type(settings) == "dict":
                pm = settings.get("default_payment_method", None)
        if pm != None and pm != "":
            return pm
    docs = store_collection("payment_methods").list()
    newest = None
    for i in range(len(docs)):
        pm = docs[i]
        if pm.get("customer", None) != customer_id:
            continue
        if pm.get("type", "card") != "card":
            continue
        if newest == None or _num(pm.get("created", 0)) > _num(newest.get("created", 0)):
            newest = pm
    if newest == None:
        return None
    return newest.get("id", None)

# _inv_resolve_pm picks the payment method for a pay call: explicit body
# param, else the subscription's default, else the customer default.
def _inv_resolve_pm(doc, body):
    pm = body.get("payment_method", None)
    if pm != None and pm != "":
        return pm
    if doc.get("subscription", None) != None:
        sub = store_collection("subscriptions").get(doc["subscription"])
        if sub != None:
            spm = sub.get("default_payment_method", None)
            if spm != None and spm != "":
                return spm
    return _inv_default_pm(doc.get("customer", None))

# ============================================================================
# ENDPOINTS
# ============================================================================

# POST /v1/invoices — create a draft invoice.
#
# With `items` -> a manual invoice carrying those lines; without -> a draft
# with zero lines (pending invoice items are NOT auto-included; they flow
# into subscription invoices). `subscription` scopes the invoice to a
# subscription's customer.
def on_create_invoice(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, _INV_COLLECTION)
    if cached != None:
        return respond(cached["status"], _invoice_public(cached["doc"]))

    if _bad_body(req):
        return _inv_err("Invalid request body: could not parse as JSON.", None)
    body = req["body"]
    if body == None:
        body = {}

    subscription = body.get("subscription", None)
    customer = body.get("customer", None)
    if subscription != None and subscription != "":
        sub = store_collection("subscriptions").get(subscription)
        if sub == None:
            return _not_found("subscription", subscription)
        if customer == None or customer == "":
            customer = sub.get("customer", None)
    if customer == None or customer == "":
        return _inv_missing("customer")
    cus = store_collection("customers").get(customer)
    if cus == None:
        return _not_found("customer", customer)

    collection_method = body.get("collection_method", "charge_automatically")
    if collection_method not in ["charge_automatically", "send_invoice"]:
        return _inv_err("Invalid collection_method: must be one of charge_automatically or send_invoice.", "collection_method")

    due_date = body.get("due_date", None)
    if due_date != None:
        due_date = _num(due_date)
        if due_date <= 0:
            due_date = None

    tax_rates = body.get("default_tax_rates", [])
    if tax_rates == None or type(tax_rates) != "list":
        tax_rates = []

    items = body.get("items", None)
    if items == None:
        items = body.get("line_items", None)
    lines = []
    if items != None and type(items) == "list":
        lines = _inv_lines_from_items(items)

    metadata = body.get("metadata", {})
    if metadata == None or type(metadata) != "dict":
        metadata = {}

    auto_advance = body.get("auto_advance", False)
    if auto_advance == None:
        auto_advance = False

    doc = _inv_new(
        customer,
        lines,
        body.get("currency", None),
        collection_method,
        auto_advance == True,
        due_date,
        subscription,
        body.get("description", None),
        metadata,
        tax_rates,
        "manual",
    )

    store_collection(_INV_COLLECTION).insert(doc)
    _signed_emit("invoice.created", _invoice_public(doc))
    _idempotent_remember(req, _INV_COLLECTION, 201, doc["id"])
    return respond(201, _invoice_public(doc))

# GET /v1/invoices/{id} — retrieve an invoice.
def on_retrieve_invoice(req):
    err = _require_auth(req)
    if err != None:
        return err
    doc = _inv_get(req["params"]["id"])
    if doc == None:
        return _not_found("invoice", req["params"]["id"])
    return respond(200, _invoice_public(doc))

# _inv_apply_filters maps the real Stripe invoice-list query params
# (customer, subscription, status, collection_method, due_date exact/range,
# created exact/range) to query_select clauses.
def _inv_apply_filters(req, docs):
    f = []
    cust = _get_query(req, "customer")
    if cust != "":
        f.append(["customer", "=", cust])
    sub = _get_query(req, "subscription")
    if sub != "":
        f.append(["subscription", "=", sub])
    status = _get_query(req, "status")
    if status != "":
        f.append(["status", "=", status])
    cm = _get_query(req, "collection_method")
    if cm != "":
        f.append(["collection_method", "=", cm])
    for key in ["due_date", "due_date[gt]", "due_date[gte]", "due_date[lt]", "due_date[lte]"]:
        v = _get_query(req, key)
        if v == "":
            continue
        n = _to_int(v)
        if n <= 0:
            continue
        op = "="
        if key.endswith("[gt]"):
            op = ">"
        elif key.endswith("[gte]"):
            op = ">="
        elif key.endswith("[lt]"):
            op = "<"
        elif key.endswith("[lte]"):
            op = "<="
        f.append(["due_date", op, n])
    _created_filters(req, f)
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# GET /v1/invoices — list invoices.
def on_list_invoices(req):
    err = _require_auth(req)
    if err != None:
        return err
    bad = _created_check(req)
    if bad != None:
        return bad
    docs = store_collection(_INV_COLLECTION).list()
    docs = _inv_apply_filters(req, docs)
    docs = _newest_first(docs)
    page, has_more, e = _list_page(req, docs, "invoice")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_invoice_public(d) for d in page], "has_more": has_more, "url": "/v1/invoices"})

# GET /v1/invoices/{id}/lines — page over the invoice's line items.
def on_list_invoice_lines(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = _inv_get(id)
    if doc == None:
        return _not_found("invoice", id)
    lines = doc.get("lines", [])
    page, has_more, e = _list_page(req, lines, "line_item")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": page, "has_more": has_more, "url": "/v1/invoices/" + id + "/lines"})

# _INV_DRAFT_ONLY lists the update params that real Stripe restricts to
# drafts ("Once an invoice is finalized, monetary values, as well as
# collection_method, become uneditable"; description stays editable).
_INV_DRAFT_ONLY = ["collection_method", "currency", "default_tax_rates", "days_until_due", "subscription", "due_date"]

# POST /v1/invoices/{id} — update an invoice. Drafts are fully editable;
# finalized invoices accept only metadata/auto_advance-style fields, and a
# draft-only param on a finalized invoice is a real 400.
def on_update_invoice(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = _inv_get(id)
    if doc == None:
        return _not_found("invoice", id)

    if _bad_body(req):
        return _inv_err("Invalid request body: could not parse as JSON.", None)
    body = req["body"]
    if body == None:
        body = {}

    status = doc.get("status", "draft")
    if status != "draft":
        for i in range(len(_INV_DRAFT_ONLY)):
            key = _INV_DRAFT_ONLY[i]
            if body.get(key, None) != None:
                return _inv_err("You cannot update the " + key + " of an invoice with a status of " + status + ". Only draft invoices may update this field.", key)

    if body.get("metadata", None) != None and type(body["metadata"]) == "dict":
        meta = doc.get("metadata", {})
        if meta == None or type(meta) != "dict":
            meta = {}
        for k in body["metadata"]:
            meta[k] = body["metadata"][k]
        doc["metadata"] = meta

    if body.get("description", None) != None:
        doc["description"] = body["description"]
    if body.get("due_date", None) != None:
        dd = _num(body["due_date"])
        if dd > 0:
            doc["due_date"] = dd
    if body.get("collection_method", None) != None:
        cm = body["collection_method"]
        if cm in ["charge_automatically", "send_invoice"]:
            doc["collection_method"] = cm
    if body.get("auto_advance", None) != None:
        doc["auto_advance"] = body["auto_advance"] == True
    if body.get("default_tax_rates", None) != None and type(body["default_tax_rates"]) == "list":
        doc["default_tax_rates"] = body["default_tax_rates"]
        doc = _inv_recompute(doc)

    _inv_save(doc)
    _signed_emit("invoice.updated", _invoice_public(doc))
    return respond(200, _invoice_public(doc))

# DELETE /v1/invoices/{id} — delete a DRAFT one-off invoice. Subscription
# invoices and non-drafts cannot be deleted (they must be voided).
def on_delete_invoice(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = _inv_get(id)
    if doc == None:
        return _not_found("invoice", id)
    if doc.get("subscription", None) != None:
        return _inv_err("You can't delete invoices created by subscriptions.", None)
    if doc.get("status", "draft") != "draft":
        return _inv_state_err("delete", "deleted", doc["status"], "draft")
    pub = _invoice_public(doc)
    store_collection(_INV_COLLECTION).delete(id)
    _signed_emit("invoice.deleted", pub)
    return respond(200, {"id": id, "object": "invoice", "deleted": True})

# POST /v1/invoices/{id}/finalize — draft -> open.
def on_finalize_invoice(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = _inv_get(id)
    if doc == None:
        return _not_found("invoice", id)
    if doc.get("status", "draft") != "draft":
        return _inv_state_err("finalize", "finalized", doc["status"], "draft")

    doc["status"] = "open"
    st = doc.get("status_transitions", {})
    if st == None:
        st = {}
    st["finalized_at"] = _now()
    doc["status_transitions"] = st
    doc["auto_advance"] = False
    _inv_save(doc)
    _signed_emit("invoice.finalized", _invoice_public(doc))
    return respond(200, _invoice_public(doc))

# POST /v1/invoices/{id}/void — open -> void.
def on_void_invoice(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = _inv_get(id)
    if doc == None:
        return _not_found("invoice", id)
    if doc.get("status", "") != "open":
        return _inv_state_err("void", "voided", doc.get("status", ""), "open")

    doc["status"] = "void"
    st = doc.get("status_transitions", {})
    if st == None:
        st = {}
    st["voided_at"] = _now()
    doc["status_transitions"] = st
    _inv_save(doc)
    _signed_emit("invoice.voided", _invoice_public(doc))
    return respond(200, _invoice_public(doc))

# POST /v1/invoices/{id}/mark_uncollectible — open -> uncollectible.
def on_mark_uncollectible_invoice(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = _inv_get(id)
    if doc == None:
        return _not_found("invoice", id)
    if doc.get("status", "") != "open":
        return _inv_state_err("mark", "marked uncollectible", doc.get("status", ""), "open")

    doc["status"] = "uncollectible"
    _inv_save(doc)
    _signed_emit("invoice.marked_uncollectible", _invoice_public(doc))
    return respond(200, _invoice_public(doc))

# POST /v1/invoices/{id}/send — email the customer a finalized invoice
# (mocked: status stays open, invoice.sent fires).
def on_send_invoice(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = _inv_get(id)
    if doc == None:
        return _not_found("invoice", id)
    if doc.get("status", "") != "open":
        return _inv_state_err("send", "sent", doc.get("status", ""), "open")

    doc["attempted"] = True
    _inv_save(doc)
    _signed_emit("invoice.sent", _invoice_public(doc))
    return respond(200, _invoice_public(doc))

# _inv_mark_paid applies the paid transitions shared by the paid_out_of_band
# and successful-charge paths, persists, then emits invoice.paid +
# invoice.payment_succeeded.
def _inv_mark_paid(doc):
    now = _now()
    st = doc.get("status_transitions", {})
    if st == None:
        st = {}
    st["paid_at"] = now
    doc["status_transitions"] = st
    doc["status"] = "paid"
    doc["paid"] = True
    doc["attempted"] = True
    doc["amount_paid"] = _num(doc.get("total", 0))
    doc["amount_remaining"] = 0
    _inv_save(doc)
    pub = _invoice_public(doc)
    _signed_emit("invoice.paid", pub)
    _signed_emit("invoice.payment_succeeded", pub)
    return doc

# _inv_failed_charge records the failed charge behind a declined invoice
# payment (like charges.star: the failed charge object still exists).
def _inv_failed_charge(doc):
    ch = {
        "id": _next_id("ch"),
        "object": "charge",
        "amount": _num(doc.get("total", 0)),
        "currency": doc.get("currency", "usd"),
        "customer": doc.get("customer", None),
        "description": doc.get("description", None),
        "status": "failed",
        "captured": False,
        "refunded": False,
        "invoice": doc["id"],
        "created": _now(),
    }
    store_collection("charges").insert(ch)
    return ch

# POST /v1/invoices/{id}/pay — attempt payment on an open invoice.
#
# paid_out_of_band marks it paid with no charge. Otherwise the resolved
# payment method runs through the shared test-card behavior: decline cards
# -> 402 card_error with the invoice left open + invoice.payment_failed;
# SCA cards -> 402 authentication_required (off-session charges cannot run
# 3DS); any other card -> paid + a real captured charge + its balance
# transaction + charge.succeeded + invoice.paid + invoice.payment_succeeded.
def on_pay_invoice(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, _INV_COLLECTION)
    if cached != None:
        return respond(cached["status"], _invoice_public(cached["doc"]))

    if _bad_body(req):
        return _inv_err("Invalid request body: could not parse as JSON.", None)
    body = req["body"]
    if body == None:
        body = {}

    id = req["params"]["id"]
    doc = _inv_get(id)
    if doc == None:
        return _not_found("invoice", id)
    if doc.get("status", "") != "open":
        return _inv_state_err("pay", "paid", doc.get("status", ""), "open")

    if _num(doc.get("amount_due", 0)) <= 0:
        doc = _inv_mark_paid(doc)
        _idempotent_remember(req, _INV_COLLECTION, 200, id)
        return respond(200, _invoice_public(doc))

    if body.get("paid_out_of_band", False) == True:
        doc = _inv_mark_paid(doc)
        _idempotent_remember(req, _INV_COLLECTION, 200, id)
        return respond(200, _invoice_public(doc))

    pm = _inv_resolve_pm(doc, body)
    if pm == None:
        return _inv_err("This customer has no attached payment source or default payment method.", "payment_method")

    number = _card_number_for(pm)
    oc = _card_outcome(number)

    if oc != None and oc["kind"] == "decline":
        ch = _inv_failed_charge(doc)
        doc["attempted"] = True
        _inv_save(doc)
        _signed_emit("charge.failed", ch)
        _signed_emit("invoice.payment_failed", _invoice_public(doc))
        return _card_decline_error(oc, "charge", ch["id"])

    if oc != None:
        # SCA test card: an off-session invoice charge cannot authenticate.
        ch = _inv_failed_charge(doc)
        doc["attempted"] = True
        _inv_save(doc)
        _signed_emit("charge.failed", ch)
        _signed_emit("invoice.payment_failed", _invoice_public(doc))
        return _sca_charge_error(ch["id"])

    ch = {
        "id": _next_id("ch"),
        "object": "charge",
        "amount": _num(doc.get("total", 0)),
        "currency": doc.get("currency", "usd"),
        "customer": doc.get("customer", None),
        "description": doc.get("description", None),
        "status": "succeeded",
        "captured": True,
        "refunded": False,
        "balance_transaction": None,
        "dispute": None,
        "invoice": doc["id"],
        "payment_intent": None,
        "created": _now(),
    }
    store_collection("charges").insert(ch)
    doc["charge"] = ch["id"]
    doc = _inv_mark_paid(doc)
    _charge_settle_hooks(ch, body, number)
    _signed_emit("charge.succeeded", ch)
    _idempotent_remember(req, _INV_COLLECTION, 200, id)
    return respond(200, _invoice_public(doc))

# ============================================================================
# UPCOMING INVOICE PREVIEW (GET /v1/invoices/upcoming — literal route, must
# be declared BEFORE /v1/invoices/{id})
# ============================================================================

# _inv_interval_step maps a price's recurring interval to a billing step:
# [whole_months, extra_seconds] (exactly one is non-zero). Month/year steps
# use calendar math; week/day steps use fixed offsets.
def _inv_interval_step(price):
    rec = price.get("recurring", None)
    interval = "month"
    if rec != None and type(rec) == "dict":
        iv = rec.get("interval", None)
        if iv != None and iv != "":
            interval = iv
    if interval == "year":
        return 12, 0
    if interval == "week":
        return 0, 7 * 24 * 3600
    if interval == "day":
        return 0, 24 * 3600
    return 1, 0

# _inv_period_end advances a period end by one billing interval (the same
# calendar math the subscriptions lifecycle uses, duplicated locally).
def _inv_period_end(end, price):
    months, seconds = _inv_interval_step(price)
    if months > 0:
        return _add_months(end, months)
    return end + seconds

# _inv_sub_lines builds the subscription renewal lines for the NEXT period:
# per-unit amounts x quantity (INVOICE line contract).
def _inv_sub_lines(sub):
    items = sub.get("items", [])
    if items == None:
        items = []
    lines = []
    start = _num(sub.get("current_period_end", 0))
    if start <= 0:
        start = _now()
    for i in range(len(items)):
        it = items[i]
        if it == None or type(it) != "dict":
            continue
        price = it.get("price", None)
        if price == None or type(price) != "dict":
            continue
        unit = _num(price.get("unit_amount", 0))
        qty = _num(it.get("quantity", 1))
        if qty < 1:
            qty = 1
        desc = "Subscription " + str(price.get("id", ""))
        prod = price.get("product", None)
        if prod != None and type(prod) == "dict":
            desc = prod.get("name", desc)
        lines.append({
            "id": _next_id("il"),
            "object": "line_item",
            "type": "subscription",
            "description": desc,
            "amount": unit,
            "quantity": qty,
            "period": {"start": start, "end": _inv_period_end(start, price)},
            "price": price,
            "proration": False,
            "tax_rates": [],
        })
    return lines

# _inv_pending_items returns the customer's pending invoice items (no
# invoice yet) — they flow into the next invoice.
def _inv_pending_items(customer_id):
    docs = store_collection("invoice_items").list()
    return query_select(docs, [["customer", "=", customer_id], ["invoice", "=", None]])

# _inv_item_lines turns pending invoice items into preview lines.
def _inv_item_lines(items):
    lines = []
    now = _now()
    for i in range(len(items)):
        ii = items[i]
        qty = _num(ii.get("quantity", 1))
        if qty < 1:
            qty = 1
        lines.append({
            "id": _next_id("il"),
            "object": "line_item",
            "type": "invoice_item",
            "description": ii.get("description", None),
            "amount": _num(ii.get("unit_amount", 0)),
            "quantity": qty,
            "period": {"start": _num(ii.get("created", now)), "end": now},
            "price": ii.get("price", None),
            "proration": False,
            "tax_rates": [],
        })
    return lines

# _inv_preview_doc assembles the invoice-SHAPED preview object (never
# stored; ids are ephemeral).
def _inv_preview_doc(customer, subscription, sub_lines, item_lines, collection_method, tax_rates, discount):
    if subscription == "":
        subscription = None
    lines = []
    lines.extend(sub_lines)
    lines.extend(item_lines)
    subtotal = 0
    for i in range(len(lines)):
        subtotal = subtotal + _num(lines[i].get("amount", 0)) * _num(lines[i].get("quantity", 1))
    discount_amt = _inv_discount_amount(subtotal, discount)
    tax, inclusive = _inv_tax_cents(subtotal - discount_amt, tax_rates)
    total = subtotal - discount_amt + tax
    if inclusive:
        total = subtotal - discount_amt
    if total < 0:
        total = 0
    currency = None
    for i in range(len(lines)):
        price = lines[i].get("price", None)
        if price != None and price.get("currency", None) != None:
            currency = price["currency"]
            break
    if currency == None:
        currency = "usd"
    return {
        "id": None,
        "object": "invoice",
        "customer": customer,
        "subscription": subscription,
        "status": "open",
        "collection_method": collection_method,
        "currency": currency,
        # same list envelope as _invoice_public — upcoming previews are
        # invoice-shaped responses too
        "lines": {
            "object": "list",
            "data": lines,
            "has_more": False,
            "total_count": len(lines),
            "url": "/v1/invoices/upcoming",
        },
        "subtotal": subtotal,
        "discount": discount,
        "tax": tax,
        "total": total,
        "amount_due": total,
        "amount_paid": 0,
        "amount_remaining": total,
        "starting_balance": 0,
        "charge": None,
        "payment_intent": None,
        "status_transitions": {"finalized_at": None, "paid_at": None, "voided_at": None},
        "billing_reason": "subscription_cycle",
        "due_date": None,
        "created": _now(),
        "auto_advance": True,
        "attempted": False,
        "metadata": {},
        "paid": None,
    }

# _inv_upcoming_none is the real Stripe 404 when a customer has no upcoming
# invoice.
def _inv_upcoming_none(customer):
    return respond(404, {"error": {"code": "invoice_upcoming_none", "message": "No upcoming invoice for customer: " + customer, "param": "customer", "type": "invalid_request_error"}})

# GET /v1/invoices/upcoming — preview the customer's next invoice:
# subscription renewal lines for the next period + pending invoice items +
# tax per the TAX CONTRACT + the subscription discount. Nothing persists.
def on_upcoming_invoice(req):
    err = _require_auth(req)
    if err != None:
        return err

    customer = _get_query(req, "customer")
    subscription = _get_query(req, "subscription")

    sub = None
    if subscription != "":
        sub = store_collection("subscriptions").get(subscription)
        if sub == None:
            return _not_found("subscription", subscription)
        if customer == "":
            customer = sub.get("customer", "")
    if customer == "":
        return _inv_missing("customer")
    if store_collection("customers").get(customer) == None:
        return _not_found("customer", customer)

    if sub == None:
        # Preview the customer's next invoice: their newest active
        # subscription, else pending invoice items alone.
        subs = query_select(store_collection("subscriptions").list(), [["customer", "=", customer]])
        for i in range(len(subs)):
            s = subs[i]
            st = s.get("status", "")
            if st in ["trialing", "active", "past_due", "incomplete"]:
                if sub == None or _num(s.get("created", 0)) > _num(sub.get("created", 0)):
                    sub = s
        subscription = ""
        if sub != None:
            subscription = sub.get("id", "")

    sub_lines = []
    collection_method = "charge_automatically"
    tax_rates = []
    discount = None
    if sub != None:
        sub_lines = _inv_sub_lines(sub)
        collection_method = sub.get("collection_method", "charge_automatically")
        tax_rates = sub.get("default_tax_rates", [])
        if tax_rates == None:
            tax_rates = []
        discount = sub.get("discount", None)
        subscription = sub.get("id", "")

    item_lines = _inv_item_lines(_inv_pending_items(customer))
    if sub == None and len(item_lines) == 0:
        return _inv_upcoming_none(customer)

    return respond(200, _inv_preview_doc(customer, subscription, sub_lines, item_lines, collection_method, tax_rates, discount))
