# REST handlers — Braintree REST API alternative.
#
# POST /merchants/{merchantId}/transactions                    → { transaction: {...} }
# GET  /merchants/{merchantId}/transactions/{id}               → { transaction: {...} }
# POST /merchants/{merchantId}/transactions/advanced_search    → { transactions: [...] }
# POST /merchants/{merchantId}/transactions/{id}/settle        → { transaction: {...} }
# POST /merchants/{merchantId}/transactions/{id}/void          → { transaction: {...} }
# POST /merchants/{merchantId}/transactions/{id}/refund        → { transaction: {...} }
# POST /merchants/{merchantId}/payment_methods                 → { payment_method: {...} }
# POST /merchants/{merchantId}/client_token                    → { client_token: "..." }
#
# Lifecycle, state-machine semantics, and the search-criteria mapping live in
# scripts/lib.star (preloaded).

# on_create_transaction creates a REST transaction. The body may be flat
# ({amount, type, options}) or wrapped ({transaction: {...}}) — both shapes
# appear against the real gateway. Amounts must be positive. Without
# options.submit_for_settlement the sale is created authorized; with it the
# transaction auto-advances authorized -> submitted_for_settlement (+1s) ->
# settled (+3s), derived on read.
def on_create_transaction(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "transactions")
    if cached != None:
        return respond(cached["status"], {"transaction": _txn_public(_advance_transaction(cached["doc"]))})

    body = req.get("body")
    if body == None:
        body = {}

    doc, einfo = _new_transaction(_txn_body(body), False)
    if einfo != None:
        return _bt_err(einfo["status"], einfo["code"], einfo["message"])

    _idempotent_remember(req, "transactions", 200, doc["id"])
    return respond(200, {"transaction": _txn_public(doc)})

# on_get_transaction retrieves a REST transaction by ID, advancing its
# lifecycle first so single reads agree with searches.
def on_get_transaction(req):
    err = _require_auth(req)
    if err != None:
        return err

    txn_id = req["params"].get("id", "")
    if txn_id == None or txn_id == "":
        return _bt_err(400, "VALIDATION", "Transaction ID is required")

    doc = _find_transaction(txn_id)
    if doc == None:
        return _bt_err(404, "NOT_FOUND", "Transaction not found")

    return respond(200, {"transaction": _txn_public(doc)})

# on_advanced_search searches transactions with the Braintree search-criteria
# vocabulary ({"search": {"status": {"in": [...]}, "amount": {"min": ...},
# "id": ..., ...}}), mapped onto typed filter/sort/slice.
def on_advanced_search(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}
    search = body.get("search", None)
    if search == None or type(search) != "dict":
        search = {}

    c = store_collection("transactions")
    items = []
    for doc in c.list():
        items.append(_txn_public(_advance_transaction(doc)))

    f = _search_filters(search)
    if len(f) > 0:
        items = query_select(items, f, None, "", None, None, None)

    return respond(200, {"transactions": items, "total_count": len(items)})

# on_settle_transaction submits a transaction for settlement (capture).
# Legal from authorized or submitted_for_settlement; an optional amount
# performs a partial capture. The settled state derives on read 3s later.
def on_settle_transaction(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc = _find_transaction(req["params"].get("id", ""))
    if doc == None:
        return _bt_err(404, "NOT_FOUND", "Transaction not found")

    body = req.get("body")
    if body == None:
        body = {}
    amount = _txn_body(body).get("amount", None)

    doc, einfo = _apply_settle(doc, amount)
    if einfo != None:
        return _bt_err(einfo["status"], einfo["code"], einfo["message"])

    return respond(200, {"transaction": _txn_public(doc)})

# on_void_transaction voids a transaction — only legal from authorized.
def on_void_transaction(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc = _find_transaction(req["params"].get("id", ""))
    if doc == None:
        return _bt_err(404, "NOT_FOUND", "Transaction not found")

    doc, einfo = _apply_void(doc)
    if einfo != None:
        return _bt_err(einfo["status"], einfo["code"], einfo["message"])

    return respond(200, {"transaction": _txn_public(doc)})

# on_refund_transaction refunds a settled transaction. Amount defaults to the
# full unrefunded balance; the sum of refunds can never exceed the original
# amount. A nonexistent transaction 404s.
def on_refund_transaction(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc = _find_transaction(req["params"].get("id", ""))
    if doc == None:
        return _bt_err(404, "NOT_FOUND", "Transaction not found")

    body = req.get("body")
    if body == None:
        body = {}
    amount = _txn_body(body).get("amount", None)

    refund_doc, einfo = _apply_refund(doc, amount)
    if einfo != None:
        return _bt_err(einfo["status"], einfo["code"], einfo["message"])

    return respond(200, {"transaction": _txn_public(refund_doc)})

# on_create_payment_method vaults a payment method / nonce.
def on_create_payment_method(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    token = _payment_method_token()
    return respond(200, {
        "payment_method": {
            "token": token,
            "customer_id": body.get("customer_id", ""),
            "card_type": "Visa",
            "last4": "1111",
            "expiration_date": "03/2030",
        },
    })

# on_client_token generates a client token for the frontend Drop-in.
def on_client_token(req):
    err = _require_auth(req)
    if err != None:
        return err

    return respond(200, {
        "client_token": _client_token(),
    })
