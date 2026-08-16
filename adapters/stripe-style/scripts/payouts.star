# Payouts handlers — Stripe Connect (connected account → bank).
#
# Payouts move funds from a connected account's balance to their bank and
# are stored in the payouts collection. Status runs a derive-on-read state
# machine keyed off _now() (test-clock aware): pending -> in_transit at
# +10s (emits payout.updated) -> paid at +60s (emits payout.paid); every
# transition is persisted BEFORE its emission and fires exactly once.
# arrival_date is computed at creation from the method (standard +4 days,
# instant +60 seconds). Creation records the negative payout balance
# transaction (no fee); canceling returns the funds with a positive payout
# ledger row linked from failure_balance_transaction, like the real API's
# cancel response. Emits payout.created / payout.updated / payout.paid /
# payout.canceled.
# Shared helpers (_require_auth, _next_id, _not_found, _num, _now,
# _stripe_account, _get_balance, _set_balance, _bt_record, _signed_emit,
# _list_page, _newest_first, _created_filters, _created_check, _get_query)
# are in lib.star.

_PO_IN_TRANSIT_SECS = 10  # pending -> in_transit
_PO_PAID_SECS = 60        # in_transit -> paid
_PO_STANDARD_DAYS = 4     # standard arrival_date horizon (days)
_PO_INSTANT_SECS = 60     # instant arrival_date horizon (seconds)

# _apply_payout_filters maps the real Stripe payout-list query params
# (destination, status, arrival_date exact/range, created exact/range) to
# query_select clauses, applied before paging like the real API.
# arrival_date/created are stored as ints, so the string params are converted.
def _apply_payout_filters(req, docs):
    f = []

    # Real Stripe scopes payouts to the Stripe-Account header when present.
    acct = _stripe_account(req)
    if acct != None:
        f.append(["_account", "=", acct])

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

# _payout_view strips the internal keys (_account scoping + lifecycle
# bookkeeping) and renders the additive real-object fields with defaults.
def _payout_view(p):
    return {
        "id": p["id"],
        "object": "payout",
        "amount": _num(p.get("amount", 0)),
        "arrival_date": _num(p.get("arrival_date", 0)),
        "automatic": False,
        "balance_transaction": p.get("balance_transaction", None),
        "created": _num(p.get("created", 0)),
        "currency": p.get("currency", "usd"),
        "description": p.get("description", None),
        "destination": p.get("destination", None),
        "failure_balance_transaction": p.get("failure_balance_transaction", None),
        "failure_code": None,
        "failure_message": None,
        "livemode": False,
        "metadata": p.get("metadata", {}),
        "method": p.get("method", "standard"),
        "original_payout": None,
        "reconciliation_status": "not_applicable",
        "reversed_by": None,
        "source_type": "bank_account",
        "statement_descriptor": p.get("statement_descriptor", None),
        "status": p.get("status", "pending"),
        "type": "bank_account",
    }

# _payout_advance derives the payout status from the clock, persisting each
# transition BEFORE emitting it, exactly once (terminal statuses short-
# circuit). Both transitions can happen in one read if the window elapsed.
def _payout_advance(doc):
    status = doc.get("status", "pending")
    if status == "paid" or status == "canceled" or status == "failed":
        return doc
    now = _now()
    created = _num(doc.get("created", 0))
    c = store_collection("payouts")
    if now >= created + _PO_PAID_SECS:
        if status != "in_transit":
            doc["status"] = "in_transit"
            c.update(doc["id"], doc)
            _signed_emit("payout.updated", _payout_view(doc))
        doc["status"] = "paid"
        c.update(doc["id"], doc)
        _signed_emit("payout.paid", _payout_view(doc))
        return doc
    if now >= created + _PO_IN_TRANSIT_SECS:
        doc["status"] = "in_transit"
        c.update(doc["id"], doc)
        _signed_emit("payout.updated", _payout_view(doc))
    return doc

# _payout_default_destination resolves the implicit payout destination: the
# account's default external bank account for the payout currency (the first
# one attached when none was flagged). None when the account has none.
def _payout_default_destination(acct, currency):
    if acct == None or acct == "":
        return None
    docs = store_collection("external_accounts").list()
    eas = query_select(docs, [["account", "=", acct], ["currency", "=", currency]])
    for i in range(len(eas)):
        if eas[i].get("default_for_currency", False) == True:
            return eas[i]["id"]
    if len(eas) > 0:
        return eas[0]["id"]
    return None

# POST /v1/payouts — create a payout from a connected account's balance.
def on_create_payout(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "payouts")
    if cached != None:
        return respond(cached["status"], _payout_view(cached["doc"]))

    body = req["body"]
    if body == None:
        body = {}

    amount = _num(body.get("amount", 0))
    currency = body.get("currency", None)
    if currency == None or currency == "":
        return respond(400, {"error": {"message": "Missing required param: currency.", "param": "currency", "type": "invalid_request_error"}})
    if amount <= 0:
        return respond(400, {"error": {"code": "parameter_invalid_integer", "message": "Invalid positive integer: " + str(body.get("amount", 0)), "param": "amount", "type": "invalid_request_error"}})

    method = body.get("method", "standard")
    acct = _stripe_account(req)
    destination = body.get("destination", None)
    if destination == None or destination == "":
        destination = _payout_default_destination(acct, currency)

    payout_id = _next_id("po")
    # Standard payouts arrive in ~4 days; instant payouts in a minute.
    # Test-clock aware via _now().
    arrival = _now() + _PO_STANDARD_DAYS * 24 * 3600
    if method == "instant":
        arrival = _now() + _PO_INSTANT_SECS
    doc = {
        "id": payout_id,
        "object": "payout",
        "amount": amount,
        "currency": currency,
        "method": method,
        "destination": destination,
        "description": body.get("description", None),
        "statement_descriptor": body.get("statement_descriptor", None),
        "metadata": body.get("metadata", {}),
        "status": "pending",
        "arrival_date": arrival,
        "created": _now(),
        "balance_transaction": None,
        "failure_balance_transaction": None,
    }

    # Track which account this payout belongs to (for list filtering) and
    # debit its balance, mirrored by a payout ledger row (-amount, no fee).
    if acct != None:
        doc["_account"] = acct
        po_bt = _bt_record(acct, "payout", -amount, 0, currency, payout_id, "Payout to bank")
        doc["balance_transaction"] = po_bt["id"]
        # Preserve the historical no-negative-balance clamp.
        if _get_balance(acct) < 0:
            _set_balance(acct, 0)

    store_collection("payouts").insert(doc)

    # Emit webhook event (fire-and-forget).
    _signed_emit("payout.created", _payout_view(doc))

    _idempotent_remember(req, "payouts", 201, doc["id"])
    return respond(201, _payout_view(doc))

# GET /v1/payouts — list all payouts (optionally ?destination=/?status=).
def on_list_payouts(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("payouts").list()

    # Derive every payout's status first so the status filter matches the
    # same state the retrieve endpoint would report.
    docs = [_payout_advance(d) for d in docs]

    # Real payout-list params (destination, status, arrival_date, created),
    # applied before paging.
    docs = _apply_payout_filters(req, docs)
    docs = _newest_first(docs)
    docs = [_payout_view(p) for p in docs]

    page, has_more, err2 = _list_page(req, docs, "payout")
    if err2 != None:
        return err2
    return respond(200, {"object": "list", "data": page, "has_more": has_more, "url": "/v1/payouts"})

# GET /v1/payouts/{id} — retrieve a payout (derives its status first).
def on_retrieve_payout(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("payouts").get(id)
    if doc == None:
        return _not_found("payout", id)
    return respond(200, _payout_view(_payout_advance(doc)))

# POST /v1/payouts/{id} — update a payout (metadata + description, the real
# API's updatable params).
def on_update_payout(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("payouts").get(id)
    if doc == None:
        return _not_found("payout", id)

    body = req["body"]
    if body != None:
        md = body.get("metadata", None)
        if md != None and type(md) == "dict":
            doc["metadata"] = md
        d = body.get("description", None)
        if d != None:
            doc["description"] = d

    store_collection("payouts").update(id, doc)
    _signed_emit("payout.updated", _payout_view(doc))
    return respond(200, _payout_view(doc))

# POST /v1/payouts/{id}/cancel — cancel a pending or in-transit payout and
# return the funds to the account's available balance (a positive payout
# ledger row, linked from failure_balance_transaction like the real cancel
# response). Terminal payouts (paid) get the real 400.
def on_cancel_payout(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("payouts").get(id)
    if doc == None:
        return _not_found("payout", id)

    # Derive first: a payout whose window elapsed is already paid and can no
    # longer be canceled.
    doc = _payout_advance(doc)
    status = doc.get("status", "pending")
    if status == "paid" or status == "canceled" or status == "failed":
        return respond(400, {"error": {"message": "This payout can no longer be canceled.", "param": "status", "type": "invalid_request_error"}})

    doc["status"] = "canceled"
    acct = doc.get("_account", None)
    if acct != None and acct != "":
        bt = _bt_record(acct, "payout", _num(doc.get("amount", 0)), 0, doc.get("currency", "usd"), id, "Payout canceled: funds returned")
        doc["failure_balance_transaction"] = bt["id"]

    store_collection("payouts").update(id, doc)
    _signed_emit("payout.canceled", _payout_view(doc))
    return respond(200, _payout_view(doc))
