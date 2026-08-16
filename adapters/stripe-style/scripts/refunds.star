# Refunds handlers — first-class /v1/refunds resource with its own async
# lifecycle: every refund starts "pending", then derives its terminal state
# from the clock on read (+3s): "succeeded", or "failed" when created with the
# simulator-only simulate_fail flag. The transition is persisted and the
# refund.updated webhook fires exactly once.
#
# The over-refund guard sums every still-active refund (pending, succeeded) of
# the target payment_intent/charge and rejects amounts beyond the unrefunded
# balance with the real Stripe 400. failed AND canceled refunds free the
# balance again.
#
# Refunding an UNCAPTURED charge releases the authorization instead of moving
# money (docs.stripe.com/refunds: a PaymentIntent in requires_capture "can't
# be refunded directly. You must cancel the PaymentIntent" — for the legacy
# Charges API the equivalent is a refund that voids the auth): the refund is
# born terminal "succeeded" with NO balance transaction (no funds ever moved)
# and the charge flips refunded.
# Shared helpers (_require_auth, _not_found, _list_page, _signed_emit,
# _refund_public, _create_refund, _advance_refund, _refunds_for,
# _refunded_total, _over_refund_error, _apply_charge_refund, _receipt_number,
# _bt_record, _num) are in lib.star.

# _ref_public renders the lib public shape plus the failure fields the real
# Refund object carries once a refund has failed or been canceled (failure_
# reason / failure_balance_transaction — docs.stripe.com/api/refunds/cancel).
# Purely additive over lib._refund_public, which this file cannot edit.
def _ref_public(doc):
    out = _refund_public(doc)
    if doc.get("failure_reason", None) != None:
        out["failure_reason"] = doc["failure_reason"]
    if doc.get("failure_balance_transaction", None) != None:
        out["failure_balance_transaction"] = doc["failure_balance_transaction"]
    return out

# fails to parse arrives as an EMPTY dict via req.body, so req.raw_body is the
# source of truth.
# _ref_apply_charge_recompute recomputes a charge's refund bookkeeping from the
# still-active refunds (used after a cancel rolls one back). Mirrors lib's
# _apply_charge_refund flags: fully refunded -> refunded True + status
# "refunded", otherwise refunded False with the status left alone.
def _ref_apply_charge_recompute(ch, active):
    base = _num(ch.get("amount", 0))
    ch["amount_refunded"] = active
    if active >= base and base > 0:
        ch["refunded"] = True
        ch["status"] = "refunded"
    else:
        ch["refunded"] = False
        if ch.get("status", "") == "refunded":
            ch["status"] = "succeeded"
    return ch

# _ref_release_refund creates the terminal refund that documents an
# authorization release on an uncaptured charge. No balance transaction is
# recorded (real Stripe moves no funds when voiding an auth — unlike lib's
# _create_refund, which books the -amount row for captured-charge refunds).
def _ref_release_refund(charge_id, amount, currency, reason):
    doc = {
        "id": _next_id("re"),
        "object": "refund",
        "amount": amount,
        "balance_transaction": None,
        "receipt_number": _receipt_number(),
        "currency": currency,
        "payment_intent": None,
        "charge": charge_id,
        "reason": reason,
        "status": "succeeded",
        "created": _now(),
        "_stage": 2,
        "_done_at": _now(),
        "_fail_mode": "",
    }
    store_collection("refunds").insert(doc)
    _signed_emit("refund.created", _ref_public(doc))
    return doc

# _apply_refund_filters maps the real Stripe refund-list query params
# (charge, payment_intent, created exact/range) to query_select clauses,
# applied before paging like the real API.
def _apply_refund_filters(req, docs):
    f = []
    ch = _get_query(req, "charge")
    if ch != "":
        f.append(["charge", "=", ch])
    pi = _get_query(req, "payment_intent")
    if pi != "":
        f.append(["payment_intent", "=", pi])
    _created_filters(req, f)
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# POST /v1/refunds — refund a payment_intent or charge. amount omitted → the
# full remaining unrefunded balance.
def on_create_refund(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "refunds")
    if cached != None:
        return respond(cached["status"], _ref_public(cached["doc"]))

    if _bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})
    body = req["body"]
    if body == None:
        body = {}

    pi_id = body.get("payment_intent", None)
    charge_id = body.get("charge", None)
    if pi_id == None and charge_id == None:
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Must provide either payment_intent or charge."}})

    amount = _num(body.get("amount", 0))
    sf = body.get("simulate_fail", False)
    fail_mode = sf != None and sf
    reason = body.get("reason", "requested_by_customer")

    if pi_id != None:
        pis = store_collection("payment_intents")
        pi = pis.get(pi_id)
        if pi == None:
            return _not_found("payment_intent", pi_id)
        # Real Stripe (docs.stripe.com/refunds): a PaymentIntent held at
        # requires_capture cannot be refunded — the uncaptured charge can't be
        # refunded directly; the PaymentIntent must be canceled instead.
        if pi.get("status", "") == "requires_capture":
            return respond(400, {"error": {"code": "payment_intent_unexpected_state", "type": "invalid_request_error", "message": "This PaymentIntent could not be refunded because it has a status of requires_capture. You can cancel it instead with the PaymentIntents API.", "param": "payment_intent"}})
        base = _num(pi.get("amount", 0))
        remaining = base - _refunded_total(_refunds_for("payment_intent", pi_id))
        if amount == 0:
            amount = remaining
        if amount > remaining or amount <= 0:
            return _over_refund_error(amount, remaining)
        doc = _create_refund(pi_id, None, amount, pi.get("currency", "usd"), reason, fail_mode)
    else:
        chs = store_collection("charges")
        ch = chs.get(charge_id)
        if ch == None:
            return _not_found("charge", charge_id)
        base = _num(ch.get("amount", 0))
        already = _refunded_total(_refunds_for("charge", charge_id))
        remaining = base - already
        if amount == 0:
            amount = remaining
        if amount > remaining or amount <= 0:
            return _over_refund_error(amount, remaining)

        if ch.get("captured", True) != True:
            # Uncaptured charge: release the authorization — the refund is born
            # terminal with no ledger row, and the charge flips refunded.
            doc = _ref_release_refund(charge_id, amount, ch.get("currency", "usd"), reason)
            _apply_charge_refund(ch, already, amount)
            chs.update(charge_id, ch)
            _signed_emit("charge.refunded", ch)
            _idempotent_remember(req, "refunds", 201, doc["id"])
            return respond(201, _ref_public(doc))

        doc = _create_refund(None, charge_id, amount, ch.get("currency", "usd"), reason, fail_mode)

        _apply_charge_refund(ch, already, amount)
        chs.update(charge_id, ch)
        _signed_emit("charge.refunded", ch)

    _idempotent_remember(req, "refunds", 201, doc["id"])
    return respond(201, _ref_public(doc))

# GET /v1/refunds/{id} — retrieve a refund (derives its async status first,
# so polls agree with the webhook timeline).
def on_retrieve_refund(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("refunds").get(id)
    if doc == None:
        return _not_found("refund", id)
    return respond(200, _ref_public(_advance_refund(doc)))

# GET /v1/refunds — list refunds (optional ?payment_intent= / ?charge=).
def on_list_refunds(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("refunds").list()
    for i in range(len(docs)):
        docs[i] = _advance_refund(docs[i])
    docs = _apply_refund_filters(req, docs)
    docs = _newest_first(docs)

    page, has_more, e = _list_page(req, docs, "refund")
    if e != None:
        return e
    out = []
    for i in range(len(page)):
        out.append(_ref_public(page[i]))
    return respond(200, {"object": "list", "data": out, "has_more": has_more, "url": "/v1/refunds"})

# POST /v1/refunds/{id}/cancel — cancel a refund that has not settled yet.
#
# Real Stripe (docs.stripe.com/api/refunds/cancel) only cancels refunds still
# awaiting settlement; everything else is a 400 invalid_request_error. A
# canceled refund is terminal: cancellation is a kind of refund failure, so
# the refund object carries failure_reason (merchant_request, per the real
# cancel response) plus failure_balance_transaction — the ledger row that
# returns the reserved funds to the platform balance (lib._create_refund
# booked the -amount row at creation). The charge's refund bookkeeping is
# recomputed from the remaining active refunds, so a canceled refund frees the
# unrefunded balance again. refund.updated fires exactly once.
def on_cancel_refund(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "refunds")
    if cached != None:
        return respond(cached["status"], _ref_public(cached["doc"]))

    if _bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})

    id = req["params"]["id"]
    rs = store_collection("refunds")
    doc = rs.get(id)
    if doc == None:
        return _not_found("refund", id)

    # The cancel decision uses the STORED status: a refund still pending in the
    # store is cancellable (real Stripe's pending window is days; the
    # simulator derives success after 3 seconds, which would otherwise make
    # cancel untestable and race-prone).
    if doc.get("status", "") != "pending":
        return respond(400, {"error": {"type": "invalid_request_error", "message": "This refund cannot be canceled because its status is " + str(doc.get("status", "")) + ". Only pending refunds can be canceled."}})

    # Persist the terminal canceled state BEFORE emitting or touching money.
    doc["status"] = "canceled"
    doc["failure_reason"] = "merchant_request"
    doc["_stage"] = 2
    doc["_done_at"] = _now()
    doc["_fail_mode"] = ""
    rs.update(id, doc)

    # Return the reserved funds to the platform ledger.
    if doc.get("balance_transaction", None) != None:
        fbt = _bt_record("", "refund_failure", _num(doc.get("amount", 0)), 0, doc.get("currency", "usd"), id, "Canceled refund")
        doc["failure_balance_transaction"] = fbt["id"]
        rs.update(id, doc)

    # Roll the charge's refund bookkeeping back to the still-active refunds.
    ch_id = doc.get("charge", None)
    if ch_id != None:
        chs = store_collection("charges")
        ch = chs.get(ch_id)
        if ch != None:
            _ref_apply_charge_recompute(ch, _refunded_total(_refunds_for("charge", ch_id)))
            chs.update(ch_id, ch)

    _signed_emit("refund.updated", _ref_public(doc))
    _idempotent_remember(req, "refunds", 200, id)
    return respond(200, _ref_public(doc))
