# Transfers handlers — Stripe Connect (platform → connected account).
#
# Transfers move funds from the platform to a connected account and are
# stored in the transfers collection; each reversal is a first-class
# transfer_reversal doc (trr_*) in the transfer_reversals collection.
# Money movement mirrors the ledger: creating a transfer credits the
# destination account and debits the platform (type transfer); a reversal
# moves the funds back (type transfer_reversal, both sides). Partial
# reversals accumulate on transfer.amount_reversed; the total may never
# exceed the transfer amount (real 400s for over-reversal and for reversing
# an already fully reversed transfer). Emits transfer.created and
# transfer.reversed (the real event fires for partial reversals too).
# Shared helpers (_require_auth, _next_id, _not_found, _num, _usd, _now,
# _bt_record, _get_balance, _set_balance, _signed_emit, _list_page,
# _newest_first, _idempotent_lookup, _idempotent_remember) are in lib.star.

# _apply_transfer_filters maps the real Stripe transfer-list query params
# (destination, created exact/range) to query_select clauses, applied before
# paging like the real API.
def _apply_transfer_filters(req, docs):
    f = []
    dest = _get_query(req, "destination")
    if dest != "":
        f.append(["destination", "=", dest])
    _created_filters(req, f)
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# _reversals_for returns the reversal docs of one transfer.
def _reversals_for(transfer_id):
    docs = store_collection("transfer_reversals").list()
    return query_select(docs, [["transfer", "=", transfer_id]])

# _reversal_view renders the transfer_reversal object
# (docs.stripe.com/api/transfer_reversals/object).
def _reversal_view(doc):
    return {
        "id": doc["id"],
        "object": "transfer_reversal",
        "amount": _num(doc.get("amount", 0)),
        "balance_transaction": doc.get("balance_transaction", None),
        "created": _num(doc.get("created", 0)),
        "currency": doc.get("currency", "usd"),
        "destination_payment_refund": None,
        "metadata": doc.get("metadata", {}),
        "source_refund": None,
        "transfer": doc.get("transfer", None),
    }

# _transfer_view renders the transfer object, with the embedded reversals
# list rebuilt from the stored reversal docs.
def _transfer_view(doc):
    revs = _newest_first(_reversals_for(doc["id"]))
    return {
        "id": doc["id"],
        "object": "transfer",
        "amount": _num(doc.get("amount", 0)),
        "amount_reversed": _num(doc.get("amount_reversed", 0)),
        "balance_transaction": doc.get("balance_transaction", None),
        "created": _num(doc.get("created", 0)),
        "currency": doc.get("currency", "usd"),
        "description": doc.get("description", None),
        "destination": doc.get("destination", None),
        "livemode": False,
        "metadata": doc.get("metadata", {}),
        "reversals": {
            "object": "list",
            "data": [_reversal_view(r) for r in revs],
            "has_more": False,
            "total_count": len(revs),
            "url": "/v1/transfers/" + doc["id"] + "/reversals",
        },
        "reversed": doc.get("reversed", False) == True,
        "source_transaction": doc.get("source_transaction", None),
        "source_type": "card",
        "transfer_group": doc.get("transfer_group", None),
    }

# POST /v1/transfers — create a transfer to a connected account.
def on_create_transfer(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    amount = _num(body.get("amount", 0))
    currency = body.get("currency", "usd")
    destination = body.get("destination", None)

    if destination == None or destination == "":
        return respond(400, {"error": {"message": "Must provide destination.", "param": "destination", "type": "invalid_request_error"}})
    if amount <= 0:
        return respond(400, {"error": {"code": "parameter_invalid_integer", "message": "Invalid positive integer: " + str(body.get("amount", 0)), "param": "amount", "type": "invalid_request_error"}})

    # Verify destination account exists. Real Stripe rejects an unknown
    # destination with a resource-missing "No such account" error; the
    # adapter-wide convention (and existing behavior) is a 404 envelope.
    if store_collection("connect_accounts").get(destination) == None:
        return _not_found("account", destination)

    transfer_id = _next_id("tr")
    doc = {
        "id": transfer_id,
        "object": "transfer",
        "amount": amount,
        "currency": currency,
        "destination": destination,
        "description": body.get("description", None),
        "metadata": body.get("metadata", {}),
        "source_transaction": body.get("source_transaction", None),
        "transfer_group": body.get("transfer_group", None),
        "reversed": False,
        "amount_reversed": 0,
        "balance_transaction": None,
        "created": _now(),
    }

    c = store_collection("transfers")
    c.insert(doc)

    # Mirror the existing accounting with ledger rows: credit the connected
    # account (+amount, type transfer) and debit the platform ledger
    # (-amount). transfer.balance_transaction is the platform-side txn, like
    # real Stripe.
    _bt_record(destination, "transfer", amount, 0, currency, transfer_id, body.get("description", None))
    plat_bt = _bt_record("", "transfer", -amount, 0, currency, transfer_id, body.get("description", None))
    doc["balance_transaction"] = plat_bt["id"]
    c.update(transfer_id, doc)

    # Emit webhook event (fire-and-forget).
    _signed_emit("transfer.created", _transfer_view(doc))

    return respond(201, _transfer_view(doc))

# GET /v1/transfers/{id} — retrieve a single transfer.
def on_retrieve_transfer(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("transfers").get(id)
    if doc == None:
        return _not_found("transfer", id)
    return respond(200, _transfer_view(doc))

# GET /v1/transfers — list all transfers (optionally ?destination=).
def on_list_transfers(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("transfers").list()

    # Real transfer-list params (destination, created exact/range), applied
    # before paging. transfer_group is not stored, so it is not honored.
    docs = _apply_transfer_filters(req, docs)
    docs = _newest_first(docs)

    page, has_more, err2 = _list_page(req, docs, "transfer")
    if err2 != None:
        return err2
    return respond(200, {"object": "list", "data": [_transfer_view(d) for d in page], "has_more": has_more, "url": "/v1/transfers"})

# POST /v1/transfers/{id}/reversals — reverse a transfer, fully (amount
# omitted) or partially. Returns the transfer_reversal object, like the real
# API; the transfer's amount_reversed/reversed fields accumulate across
# partials.
def on_reverse_transfer(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "transfer_reversals")
    if cached != None:
        return respond(cached["status"], _reversal_view(cached["doc"]))

    id = req["params"]["id"]
    c = store_collection("transfers")
    doc = c.get(id)
    if doc == None:
        return _not_found("transfer", id)

    body = req["body"]
    if body == None:
        body = {}

    base = _num(doc.get("amount", 0))
    already = _num(doc.get("amount_reversed", 0))
    remaining = base - already
    if remaining <= 0:
        return respond(400, {"error": {"message": "This transfer is already fully reversed.", "param": "amount", "type": "invalid_request_error"}})

    amount = _num(body.get("amount", 0))
    if amount == 0:
        amount = remaining
    if amount > remaining or amount <= 0:
        return respond(400, {"error": {"message": "Transfer reversal amount (" + _usd(amount) + ") is greater than unreversed amount on transfer (" + _usd(remaining) + ")", "param": "amount", "type": "invalid_request_error"}})

    trr_id = _next_id("trr")
    # Debit the connected account's balance (ledger row: transfer_reversal,
    # -amount) and credit the platform ledger back. The reversal's
    # balance_transaction is the platform-side txn the API caller sees.
    dest = doc.get("destination", None)
    if dest != None and dest != "":
        _bt_record(dest, "transfer_reversal", -amount, 0, doc.get("currency", "usd"), trr_id, "Transfer reversal")
        if _get_balance(dest) < 0:
            _set_balance(dest, 0)
    plat_bt = _bt_record("", "transfer_reversal", amount, 0, doc.get("currency", "usd"), trr_id, "Transfer reversal")

    trr = {
        "id": trr_id,
        "object": "transfer_reversal",
        "amount": amount,
        "balance_transaction": plat_bt["id"],
        "created": _now(),
        "currency": doc.get("currency", "usd"),
        "metadata": body.get("metadata", {}),
        "transfer": id,
    }
    store_collection("transfer_reversals").insert(trr)

    doc["amount_reversed"] = already + amount
    if doc["amount_reversed"] >= base:
        doc["reversed"] = True
    c.update(id, doc)

    # Emit webhook event after every state change is persisted.
    _signed_emit("transfer.reversed", _transfer_view(doc))

    _idempotent_remember(req, "transfer_reversals", 200, trr_id)
    return respond(200, _reversal_view(trr))

# GET /v1/transfers/{id}/reversals — list a transfer's reversals (newest
# first, cursor pagination).
def on_list_transfer_reversals(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    if store_collection("transfers").get(id) == None:
        return _not_found("transfer", id)

    docs = _newest_first(_reversals_for(id))
    page, has_more, err2 = _list_page(req, docs, "transfer_reversal")
    if err2 != None:
        return err2
    return respond(200, {"object": "list", "data": [_reversal_view(d) for d in page], "has_more": has_more, "url": "/v1/transfers/" + id + "/reversals"})

# GET /v1/transfers/{id}/reversals/{tr_id} — retrieve one reversal.
def on_retrieve_transfer_reversal(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    tr_id = req["params"]["tr_id"]
    if store_collection("transfers").get(id) == None:
        return _not_found("transfer", id)
    doc = store_collection("transfer_reversals").get(tr_id)
    if doc == None or doc.get("transfer", None) != id:
        return _not_found("transfer_reversal", tr_id)
    return respond(200, _reversal_view(doc))
