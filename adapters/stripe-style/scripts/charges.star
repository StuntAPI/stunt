# Charges handlers — Starlark stateful logic backed by store_collection.
#
# Each handler receives `req` with keys: method, path, headers, body, params.
# Returns respond(status, body, headers).
# Shared helpers (_bearer_token, _require_auth, _next_id) are in lib.star.

# POST /v1/charges — create a charge (status starts as "pending").
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

    doc = {
        "id": charge_id,
        "object": "charge",
        "amount": amount,
        "currency": currency,
        "customer": customer,
        "description": description,
        "status": "pending",
        "captured": False,
        "refunded": False,
        "created": 1700000000,
    }

    c = store_collection("charges")
    c.insert(doc)

    # Emit webhook event (fire-and-forget: errors do not break charge creation).
    _signed_emit("charge.created", doc)

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
        return respond(404, {"error": {"message": "No such charge: " + id, "type": "invalid_request_error"}})
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
        return respond(404, {"error": {"message": "No such charge: " + id, "type": "invalid_request_error"}})

    doc["status"] = "succeeded"
    doc["captured"] = True
    c.update(id, doc)

    # Emit webhook event (fire-and-forget).
    _signed_emit("charge.updated", doc)

    return respond(200, doc)

# POST /v1/charges/{id}/refund — refund a charge (set status refunded).
def on_refund_charge(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("charges")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "No such charge: " + id, "type": "invalid_request_error"}})

    doc["status"] = "refunded"
    doc["refunded"] = True
    doc["amount_refunded"] = doc.get("amount", 0)
    c.update(id, doc)

    # Emit webhook event (fire-and-forget).
    _signed_emit("charge.refunded", doc)

    return respond(200, doc)
