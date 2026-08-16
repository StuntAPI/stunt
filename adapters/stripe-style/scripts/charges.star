# Charges handlers — Starlark stateful logic backed by store_collection.
#
# Each handler receives `req` with keys: method, path, headers, body, params.
# Returns respond(status, body, headers).
# Shared helpers (_bearer_token, _require_auth, _next_id, _card_number_for,
# _card_outcome, _card_decline_error, _sca_charge_error, _create_refund,
# _refunds_for, _refunded_total, _over_refund_error, _apply_charge_refund)
# are in lib.star.

# _charge_card_number resolves the card number a charge request pays with:
# a card token (source=tok_*), a PaymentMethod (payment_method=pm_*), or an
# inline card dict. Returns "" when no card number is known.
def _charge_card_number(body):
    src = body.get("source", None)
    if src == None:
        src = body.get("payment_method", None)
    if src != None and type(src) == "string":
        return _card_number_for(src)
    card = body.get("card", None)
    if card != None and type(card) == "dict":
        n = card.get("number", "")
        if n == None:
            return ""
        return n
    return ""

# POST /v1/charges — create a charge.
#
# No card instrument → status "pending" (capture-later flow, the simulator's
# historical default). A card token/PaymentMethod resolves the test-card
# behavior: decline cards → 402 card_error (the failed charge object is still
# recorded, like real Stripe); SCA cards → 402 authentication_required (the
# legacy Charges API cannot run 3DS); any other card → succeeded + captured.
def on_create_charge(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "charges")
    if cached != None:
        return respond(cached["status"], cached["doc"])

    body = req["body"]
    if body == None:
        body = {}

    charge_id = _next_id("ch")
    amount = body.get("amount", 0)
    currency = body.get("currency", "usd")
    customer = body.get("customer", None)
    description = body.get("description", None)

    status = "pending"
    captured = False
    number = _charge_card_number(body)
    if number != "":
        oc = _card_outcome(number)
        if oc != None and oc["kind"] == "decline":
            doc = {
                "id": charge_id,
                "object": "charge",
                "amount": amount,
                "currency": currency,
                "customer": customer,
                "description": description,
                "status": "failed",
                "captured": False,
                "refunded": False,
                "created": _now(),
            }
            store_collection("charges").insert(doc)
            _signed_emit("charge.failed", doc)
            return _card_decline_error(oc, "charge", charge_id)
        if oc != None:
            return _sca_charge_error(charge_id)
        status = "succeeded"
        captured = True

    doc = {
        "id": charge_id,
        "object": "charge",
        "amount": amount,
        "currency": currency,
        "customer": customer,
        "description": description,
        "status": status,
        "captured": captured,
        "refunded": False,
        "balance_transaction": None,
        "dispute": None,
        "created": _now(),
    }

    c = store_collection("charges")
    c.insert(doc)

    # Emit webhook event (fire-and-forget: errors do not break charge creation).
    _signed_emit("charge.created", doc)

    # Settlement hooks (lib.star): the charge balance transaction (recorded
    # once funds move — immediately for a captured card charge, at capture
    # time otherwise), the application-fee record for Connect charges with
    # application_fee_amount, and the immediate dispute raised by the
    # documented dispute test cards.
    if captured:
        _charge_settle_hooks(doc, body, number)

    _idempotent_remember(req, "charges", 201, charge_id)
    return respond(201, doc)

# GET /v1/charges/{id} — retrieve a single charge.
def on_retrieve_charge(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("charges")
    doc = c.get(id)
    if doc == None:
        return _not_found("charge", id)
    return respond(200, doc)

# GET /v1/charges — list all charges.
def on_list_charges(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    c = store_collection("charges")
    docs = c.list()
    docs = _apply_charge_filters(req, docs)
    docs = _newest_first(docs)
    page, has_more, err = _list_page(req, docs, "charge")
    if err != None:
        return err
    return respond(200, {"object": "list", "data": page, "has_more": has_more, "url": "/v1/charges"})

# _apply_charge_filters maps the real Stripe charge-list query params
# (customer, created exact/range) to query_select clauses, applied before
# paging like the real API.
def _apply_charge_filters(req, docs):
    f = []
    cust = _get_query(req, "customer")
    if cust != "":
        f.append(["customer", "=", cust])
    _created_filters(req, f)
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# POST /v1/charges/{id}/capture — capture a pending charge (set status succeeded).
def on_capture_charge(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("charges")
    doc = c.get(id)
    if doc == None:
        return _not_found("charge", id)

    body = req["body"]
    if body == None:
        body = {}

    doc["status"] = "succeeded"
    doc["captured"] = True
    c.update(id, doc)

    # Emit webhook event (fire-and-forget).
    _signed_emit("charge.updated", doc)

    # Funds move at capture: record the charge balance transaction now (plus
    # any application_fee_amount supplied on the capture call, like real
    # Stripe). The hooks are idempotent for already-settled charges.
    _charge_settle_hooks(doc, body, "")

    return respond(200, doc)

# POST /v1/charges/{id}/refund — refund a charge (full or partial via amount).
#
# Creates a first-class refund (pending -> succeeded on its async lifecycle)
# and returns the updated charge. The over-refund guard counts every
# non-failed refund of this charge, so repeated calls cannot exceed the
# original amount. amount omitted → refund the remaining unrefunded balance.
def on_refund_charge(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("charges")
    doc = c.get(id)
    if doc == None:
        return _not_found("charge", id)

    body = req["body"]
    if body == None:
        body = {}

    base = _num(doc.get("amount", 0))
    already = _refunded_total(_refunds_for("charge", id))
    remaining = base - already
    amount = _num(body.get("amount", 0))
    if amount == 0:
        amount = remaining
    if amount > remaining or amount <= 0:
        return _over_refund_error(amount, remaining)

    sf = body.get("simulate_fail", False)
    fail_mode = sf != None and sf
    _create_refund(None, id, amount, doc.get("currency", "usd"), body.get("reason", "requested_by_customer"), fail_mode)

    _apply_charge_refund(doc, already, amount)
    c.update(id, doc)

    # Emit webhook event (fire-and-forget).
    _signed_emit("charge.refunded", doc)

    return respond(200, doc)
