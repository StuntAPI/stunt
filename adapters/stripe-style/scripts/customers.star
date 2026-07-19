# Customer handlers — Starlark stateful logic backed by store_collection.
# Shared helpers (_bearer_token, _require_auth, _next_id) are in lib.star.

# POST /v1/customers — create a customer.
def on_create_customer(req):
    err = _require_auth(req)
    if err != None:
        return err

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
def on_retrieve_customer(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("customers")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "No such customer: " + id, "type": "invalid_request_error"}})
    return respond(200, doc)

# GET /v1/customers — list all customers.
def on_list_customers(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("customers")
    docs = c.list()
    return respond(200, {"object": "list", "data": docs, "has_more": False, "url": "/v1/customers"})

# POST /v1/customers/{id} — update a customer (merge fields from body).
def on_update_customer(req):
    err = _require_auth(req)
    if err != None:
        return err

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
def on_delete_customer(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("customers")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "No such customer: " + id, "type": "invalid_request_error"}})

    c.delete(id)
    return respond(200, {"id": id, "object": "customer", "deleted": True})
