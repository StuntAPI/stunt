# Payouts handlers — Stripe Connect (connected account → bank).
#
# Payouts move funds from a connected account's balance to their bank.
# Stored in the payouts collection. Emits payout.created.
# Shared helpers (_require_auth, _next_id, _stripe_account, _get_balance,
# _set_balance) are in lib.star.

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
    events_emit("payout.created", doc)

    return respond(201, doc)

# GET /v1/payouts — list all payouts (optionally ?destination=).
def on_list_payouts(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("payouts")
    docs = c.list()

    # Optional destination filter.
    query = req.get("query")
    if query != None:
        dest_id = query.get("destination", "")
        if dest_id != "":
            docs = [d for d in docs if d.get("destination") == dest_id]

    return respond(200, {"object": "list", "data": docs, "has_more": False, "url": "/v1/payouts"})
