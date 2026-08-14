# Payouts handlers — Stripe Connect (connected account → bank).
#
# Payouts move funds from a connected account's balance to their bank.
# Stored in the payouts collection. Emits payout.created.
# Shared helpers (_require_auth, _next_id, _stripe_account, _get_balance,
# _set_balance) are in lib.star.

# _apply_payout_filters maps the real Stripe payout-list query params
# (destination, status, arrival_date exact/range, created exact/range) to
# query_select clauses, applied before paging like the real API.
# arrival_date/created are stored as ints, so the string params are converted.
def _apply_payout_filters(req, docs):
    f = []
    dest = _get_query(req, "destination")
    if dest != "":
        f.append(["destination", "=", dest])
    status = _get_query(req, "status")
    if status != "":
        f.append(["status", "=", status])

    v = _to_int(_get_query(req, "arrival_date"))
    if v > 0:
        f.append(["arrival_date", "=", v])
    v = _to_int(_get_query(req, "arrival_date[gt]"))
    if v > 0:
        f.append(["arrival_date", ">", v])
    v = _to_int(_get_query(req, "arrival_date[gte]"))
    if v > 0:
        f.append(["arrival_date", ">=", v])
    v = _to_int(_get_query(req, "arrival_date[lt]"))
    if v > 0:
        f.append(["arrival_date", "<", v])
    v = _to_int(_get_query(req, "arrival_date[lte]"))
    if v > 0:
        f.append(["arrival_date", "<=", v])

    _created_filters(req, f)
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# POST /v1/payouts — create a payout from a connected account's balance.
def on_create_payout(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    amount = body.get("amount", 0)
    currency = body.get("currency", "usd")
    method = body.get("method", "standard")
    destination = body.get("destination", None)

    payout_id = _next_id("po")
    doc = {
        "id": payout_id,
        "object": "payout",
        "amount": amount,
        "currency": currency,
        "method": method,
        "destination": destination,
        "status": "pending",
        "arrival_date": 1700432000,
        "created": 1700000000,
    }

    # Track which account this payout belongs to (for list filtering).
    acct = _stripe_account(req)
    if acct != None:
        doc["_account"] = acct
        # Debit the connected account's balance.
        bal = _get_balance(acct)
        new_bal = bal - amount
        if new_bal < 0:
            new_bal = 0
        _set_balance(acct, new_bal)

    c = store_collection("payouts")
    c.insert(doc)

    # Emit webhook event (fire-and-forget).
    _signed_emit("payout.created", doc)

    return respond(201, doc)

# GET /v1/payouts — list all payouts (optionally ?destination=).
def on_list_payouts(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("payouts")
    docs = c.list()

    # Real payout-list params (destination, status, arrival_date, created),
    # applied before paging.
    docs = _apply_payout_filters(req, docs)

    page, has_more, err = _list_page(req, docs, "payout")
    if err != None:
        return err
    return respond(200, {"object": "list", "data": page, "has_more": has_more, "url": "/v1/payouts"})
