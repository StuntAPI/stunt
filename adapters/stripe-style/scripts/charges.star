# Charges handlers — Starlark stateful logic backed by store_collection.
#
# Each handler receives `req` with keys: method, path, headers, body, params.
# Returns respond(status, body, headers).

# _next_id returns a monotonically-increasing provider-style ID using the
# KV store as a sequence counter. Produces ids like "ch_1", "ch_2", ...
def _next_id(prefix):
    seq_str = store_kv_get("stripe", prefix + "_seq")
    if seq_str == None:
        seq = 1
    else:
        seq = int(seq_str) + 1
    store_kv_set("stripe", prefix + "_seq", str(seq))
    return prefix + "_" + str(seq)

# POST /v1/charges — create a charge (status starts as "pending").
def on_create(req):
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

    # Note: a real Stripe API would emit a "charge.created" webhook event
    # here. The events primitive is not wired into Starlark builtins yet,
    # so event emission is stubbed/skipped in this adapter.

    return respond(201, doc)

# GET /v1/charges/{id} — retrieve a single charge.
def on_retrieve(req):
    id = req["params"]["id"]
    c = store_collection("charges")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "No such charge: " + id, "type": "invalid_request_error"}})
    return respond(200, doc)

# GET /v1/charges — list all charges.
def on_list(req):
    c = store_collection("charges")
    docs = c.list()
    return respond(200, {"object": "list", "data": docs, "has_more": False, "url": "/v1/charges"})

# POST /v1/charges/{id}/capture — capture a pending charge (set status succeeded).
def on_capture(req):
    id = req["params"]["id"]
    c = store_collection("charges")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "No such charge: " + id, "type": "invalid_request_error"}})

    doc["status"] = "succeeded"
    doc["captured"] = True
    c.update(id, doc)
    return respond(200, doc)

# POST /v1/charges/{id}/refund — refund a charge (set status refunded).
def on_refund(req):
    id = req["params"]["id"]
    c = store_collection("charges")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "No such charge: " + id, "type": "invalid_request_error"}})

    doc["status"] = "refunded"
    doc["refunded"] = True
    doc["amount_refunded"] = doc.get("amount", 0)
    c.update(id, doc)
    return respond(200, doc)
