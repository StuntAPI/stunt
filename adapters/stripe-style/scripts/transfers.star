# Transfers handlers — Stripe Connect (platform → connected account).
#
# Transfers move funds from the platform to a connected account. Stored in
# the transfers collection. Emits transfer.created and transfer.reversed.
# Shared helpers (_require_auth, _next_id, _not_found, _get_balance,
# _set_balance) are in lib.star.

# POST /v1/transfers — create a transfer to a connected account.
def on_create_transfer(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    amount = body.get("amount", 0)
    currency = body.get("currency", "usd")
    destination = body.get("destination", None)

    if destination == None:
        return respond(400, {"error": {"message": "Must provide destination.", "type": "invalid_request_error"}})

    # Verify destination account exists.
    accts = store_collection("connect_accounts")
    acct = accts.get(destination)
    if acct == None:
        return _not_found("account", destination)

    transfer_id = _next_id("tr")
    doc = {
        "id": transfer_id,
        "object": "transfer",
        "amount": amount,
        "currency": currency,
        "destination": destination,
        "description": body.get("description", None),
        "reversed": False,
        "amount_reversed": 0,
        "reversals": {"object": "list", "data": [], "has_more": False, "url": "/v1/transfers/" + transfer_id + "/reversals"},
        "created": 1700000000,
    }

    c = store_collection("transfers")
    c.insert(doc)

    # Credit the connected account's balance.
    bal = _get_balance(destination)
    _set_balance(destination, bal + amount)

    # Emit webhook event (fire-and-forget).
    events_emit("transfer.created", doc)

    return respond(201, doc)

# GET /v1/transfers/{id} — retrieve a single transfer.
def on_retrieve_transfer(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("transfers")
    doc = c.get(id)
    if doc == None:
        return _not_found("transfer", id)
    return respond(200, doc)

# GET /v1/transfers — list all transfers (optionally ?destination=).
def on_list_transfers(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("transfers")
    docs = c.list()

    # Optional destination filter.
    query = req.get("query")
    if query != None:
        dest_id = query.get("destination", "")
        if dest_id != "":
            docs = [d for d in docs if d.get("destination") == dest_id]

    return respond(200, {"object": "list", "data": docs, "has_more": False, "url": "/v1/transfers"})

# POST /v1/transfers/{id}/reversals — reverse (part of) a transfer.
def on_reverse_transfer(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("transfers")
    doc = c.get(id)
    if doc == None:
        return _not_found("transfer", id)

    body = req["body"]
    if body == None:
        body = {}

    amount = body.get("amount", doc.get("amount", 0))
    doc["reversed"] = True
    doc["amount_reversed"] = amount
    c.update(id, doc)

    # Debit the connected account's balance.
    dest = doc.get("destination")
    if dest != None:
        bal = _get_balance(dest)
        new_bal = bal - amount
        if new_bal < 0:
            new_bal = 0
        _set_balance(dest, new_bal)

    # Emit webhook event (fire-and-forget).
    events_emit("transfer.reversed", doc)

    return respond(200, doc)
