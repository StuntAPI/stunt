# Refund handlers — create refund, list refunds.
#
# Refund semantics (mirrors Square's Payment Refunds API):
#   - only COMPLETED payments are refundable (400 INVALID_REQUEST otherwise)
#   - refund amount must not exceed the payment's net (amount minus prior
#     COMPLETED refunds); partial refunds accumulate
#   - payment.refunded_amount tracks the running refunded total
#
# POST /v2/refunds → { refund: { id, status:"COMPLETED", ... } }
# GET  /v2/refunds → { refunds: [...], cursor } (cursor-paginated)

def on_create_refund(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    # Idempotency check.
    cached = _check_idempotency(req, "refunds")
    if cached != None:
        return respond(200, {"refund": _refund_public(cached)})

    body = req["body"]
    if body == None:
        body = {}

    payment_id = body.get("payment_id") or ""
    amount_money = body.get("amount_money", None)
    location_id = body.get("location_id", "")
    reason = body.get("reason", "")

    # Verify payment exists.
    pc = store_collection("payments")
    payment = pc.get(payment_id)
    if payment == None:
        return _sq_err_field(404, "INVALID_REQUEST_ERROR", "NOT_FOUND", "Payment " + payment_id + " not found", "payment_id")

    # Only COMPLETED payments are refundable.
    if payment.get("status", "") != "COMPLETED":
        return _sq_err_field(400, "INVALID_REQUEST_ERROR", "INVALID_REQUEST", "Only COMPLETED payments can be refunded", "payment_id")

    paid_amount = payment.get("amount_money", None)
    paid = _amount_of(paid_amount, 0)
    currency = _currency_of(paid_amount)
    already = _sum_refunded(payment_id)
    remaining = paid - already

    # An omitted/amount-less amount_money refunds the remaining balance.
    if amount_money == None or type(amount_money) != "dict" or amount_money.get("amount", None) == None:
        amount_money = _money(remaining, currency)

    # A refund's currency must match the payment's; a foreign-currency
    # amount would silently corrupt the refunded-amount bookkeeping.
    if _currency_of(amount_money) != currency:
        return _sq_err_field(400, "INVALID_REQUEST_ERROR", "INVALID_REQUEST", "Refund currency must match the payment currency", "amount_money.currency")

    amount = _amount_of(amount_money, 0)
    if amount <= 0:
        return _sq_err_field(400, "INVALID_REQUEST_ERROR", "INVALID_REQUEST", "Refund amount must be greater than 0", "amount_money.amount")
    if amount > remaining:
        return _sq_err_field(400, "INVALID_REQUEST_ERROR", "INVALID_REQUEST", "Refund amount exceeds the unrefunded balance of the payment", "amount_money.amount")

    refund_id = _refund_id()
    now = clock.now_rfc3339()

    doc = {
        "id": refund_id,
        "status": "COMPLETED",
        "payment_id": payment_id,
        "amount_money": amount_money,
        "location_id": location_id,
        "created_at": now,
        "reason": reason,
    }

    rc = store_collection("refunds")
    rc.insert(doc)

    # Track the accumulated refund total on the payment.
    payment["refunded_amount"] = _money(already + amount, currency)
    payment["updated_at"] = now
    pc.update(payment_id, payment)

    _store_idempotency(req, "refunds", refund_id)

    # Emit webhook events.
    _signed_emit("refund.created", {
        "type": "refund.created",
        "data": {
            "object": {
                "refund": _refund_public(doc),
            },
        },
    })
    _signed_emit("payment.updated", {
        "type": "payment.updated",
        "data": {
            "object": {
                "payment": _payment_public(payment),
            },
        },
    })

    return respond(200, {"refund": _refund_public(doc)})

# on_list_payment_refunds lists refunds (ListPaymentRefunds). Supports the
# real query params status and location_id via query_select; cursor
# pagination via limit/cursor.
def on_list_payment_refunds(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    items = []
    for doc in store_collection("refunds").list():
        items.append(_refund_public(doc))

    f = []
    status = _get_query(req, "status", "")
    if status != "":
        f.append(["status", "=", status])
    location_id = _get_query(req, "location_id", "")
    if location_id != "":
        f.append(["location_id", "=", location_id])

    order_dir = "asc"
    sort_order = _get_query(req, "sort_order", "")
    if sort_order == "DESC":
        order_dir = "desc"

    items = query_select(items, f if len(f) > 0 else None, "created_at", order_dir, None, None, None)

    page, next_cursor = _list_page(req, items)
    if page == None:
        return _sq_err(400, "INVALID_REQUEST_ERROR", "INVALID_CURSOR", "The cursor is invalid.")
    return respond(200, {
        "refunds": page,
        "cursor": _sq_cursor(next_cursor),
    })
