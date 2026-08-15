# Payments handlers — get capture, refund, get refund.
#
#   GET  /v2/payments/captures/{id}                  -> capture object (+ refunded_amount)
#   POST /v2/payments/captures/{capture_id}/refund   -> refund object (201, PENDING)
#   GET  /v2/payments/refunds/{id}                   -> refund object (derive-on-read)
#
# REFUND BOOKKEEPING: refunds are created PENDING and derive their terminal
# state on read (PENDING -> COMPLETED after 3s, or -> FAILED with the
# simulator-only simulate_fail flag) — see lib.star. Every read of the
# capture or the refund advances the state machine first, so refunded_amount
# and the refund status always reflect the derived state. Guards: refunding
# more than the unrefunded balance -> 400 REFUND_NOT_ALLOWED; a refund
# currency that differs from the capture currency -> 400 CURRENCY_MISMATCH.

# on_get_capture retrieves a capture by ID, advancing any due refunds before
# reporting the refunded_amount bookkeeping.
def on_get_capture(req):
    err = _require_auth(req)
    if err != None:
        return err

    capture_id = req["params"]["id"]
    c = store_collection("captures")
    doc = c.get(capture_id)
    if doc == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Capture not found.")

    # Derive-on-read: settle any refund whose window has elapsed.
    refunds = _refunds_for(capture_id)
    for r in refunds:
        _advance_refund(r)
    refunded = _refunded_cents(refunds)

    amount = doc.get("amount", {})
    out = {
        "id": doc["id"],
        "status": doc.get("status", "COMPLETED"),
        "amount": amount,
        "final_capture": doc.get("final_capture", True),
        "create_time": doc.get("create_time", ""),
        "seller_protection": {"status": "ELIGIBLE", "dispute_categories": ["ITEM_NOT_RECEIVED", "UNAUTHORIZED_TRANSACTION"]},
        "links": [
            {"href": "https://api.stunt.test/v2/payments/captures/" + doc["id"], "rel": "self", "method": "GET"},
        ],
    }
    if doc.get("order_id", "") != "":
        out["links"].append({"href": "https://api.stunt.test/v2/checkout/orders/" + doc.get("order_id", ""), "rel": "up", "method": "GET"})
    if refunded > 0:
        out["refunded_amount"] = {"currency_code": amount.get("currency_code", "USD"), "value": _fmt_cents(refunded)}

    return respond(200, out)

# on_get_refund retrieves a refund by ID, deriving its async status first.
def on_get_refund(req):
    err = _require_auth(req)
    if err != None:
        return err

    refund_id = req["params"]["id"]
    rc = store_collection("refunds")
    doc = rc.get(refund_id)
    if doc == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Refund not found.")

    doc = _advance_refund(doc)
    return respond(200, _refund_public(doc))

# on_refund refunds a capture (full amount by default, partial via
# body.amount). The refund is created PENDING and settles via derive-on-read.
def on_refund(req):
    err = _require_auth(req)
    if err != None:
        return err

    capture_id = req["params"]["capture_id"]
    c = store_collection("captures")
    cap_doc = c.get(capture_id)
    if cap_doc == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Capture not found.")

    body = req["body"]
    if body == None:
        body = {}

    cap_amount = cap_doc.get("amount", {})
    cap_currency = cap_amount.get("currency_code", "USD")
    cap_cents = _cents(cap_amount.get("value", "0.00"))
    if cap_cents == None:
        cap_cents = 0

    # Refund amount defaults to the full capture amount.
    amount = body.get("amount", cap_amount)
    currency = amount.get("currency_code", cap_currency)
    value = amount.get("value", cap_amount.get("value", "0.00"))

    if currency != cap_currency:
        return _pp_err_details(400, "INVALID_REQUEST", "CURRENCY_MISMATCH", "Refund currency must match the capture currency (" + cap_currency + ").")

    req_cents = _cents(value)
    if req_cents == None:
        return _pp_err_details(400, "INVALID_REQUEST", "INVALID_PARAMETER_VALUE", "amount.value must be a valid decimal amount.")
    if req_cents <= 0:
        return _pp_err_details(400, "INVALID_REQUEST", "INVALID_PARAMETER_VALUE", "amount.value must be greater than zero.")

    # Over-refund guard: every non-FAILED refund of this capture counts
    # (PENDING included — the balance is reserved when accepted).
    already = _refunded_cents(_refunds_for(capture_id))
    remaining = cap_cents - already
    if req_cents > remaining:
        return _pp_err_details(400, "UNPROCESSABLE_ENTITY", "REFUND_NOT_ALLOWED", "The requested refund amount exceeds the remaining unrefunded amount of the capture (" + _fmt_cents(remaining) + " " + cap_currency + ").")

    sf = body.get("simulate_fail", False)
    now = clock.now_unix()
    refund_id = _refund_id()
    refund_doc = {
        "id": refund_id,
        "capture_id": capture_id,
        "amount": {"currency_code": currency, "value": value},
        "status": "PENDING",
        "create_time": clock.now_rfc3339(),
        "_stage": 0,
        "_done_at": now + 3,
        "_fail_mode": "",
    }
    if sf:
        refund_doc["_fail_mode"] = "FAILED"

    rc = store_collection("refunds")
    rc.insert(refund_doc)

    return respond(201, _refund_public(refund_doc))
