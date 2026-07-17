# Customer handlers — Starlark stateful logic backed by store_collection.

# _next_id returns a monotonically-increasing provider-style ID using the
# KV store as a sequence counter. Produces ids like "cus_1", "cus_2", ...
def _next_id(prefix):
    seq_str = store_kv_get("stripe", prefix + "_seq")
    if seq_str == None:
        seq = 1
    else:
        seq = int(seq_str) + 1
    store_kv_set("stripe", prefix + "_seq", str(seq))
    return prefix + "_" + str(seq)

# POST /v1/customers — create a customer.
def on_create(req):
    body = req["body"]
    if body == None:
        body = {}

    customer_id = _next_id("cus")
    name = body.get("name", None)
    description = body.get("description", None)

    doc = {
        "id": customer_id,
        "object": "customer",
        "name": name,
        "description": description,
        "created": 1700000000,
    }

    c = store_collection("customers")
    c.insert(doc)
    return respond(201, doc)

# GET /v1/customers/{id} — retrieve a single customer.
def on_retrieve(req):
    id = req["params"]["id"]
    c = store_collection("customers")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "No such customer: " + id, "type": "invalid_request_error"}})
    return respond(200, doc)

# GET /v1/customers — list all customers.
def on_list(req):
    c = store_collection("customers")
    docs = c.list()
    return respond(200, {"object": "list", "data": docs, "has_more": False, "url": "/v1/customers"})

# POST /v1/customers/{id} — update a customer (merge fields from body).
def on_update(req):
    id = req["params"]["id"]
    c = store_collection("customers")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "No such customer: " + id, "type": "invalid_request_error"}})

    body = req["body"]
    if body != None:
        for k in body:
            doc[k] = body[k]

    c.update(id, doc)
    return respond(200, doc)

# DELETE /v1/customers/{id} — delete a customer.
def on_delete(req):
    id = req["params"]["id"]
    c = store_collection("customers")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "No such customer: " + id, "type": "invalid_request_error"}})

    c.delete(id)
    return respond(200, {"id": id, "object": "customer", "deleted": True})
