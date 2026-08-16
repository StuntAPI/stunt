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
        "email": body.get("email", None),
        "description": description,
        "created": 1700000000,
    }

    c = store_collection("customers")
    c.insert(doc)
    return respond(201, doc)

# GET /v1/customers/{id} — retrieve a single customer. Deleted customers
# remain retrievable: real Stripe keeps the object and returns it with
# "deleted": true (only list endpoints hide deleted customers).
def on_retrieve_customer(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("customers")
    doc = c.get(id)
    if doc == None:
        return _not_found("customer", id)
    return respond(200, doc)

# GET /v1/customers — list all customers.
def on_list_customers(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    c = store_collection("customers")
    docs = c.list()
    docs = _apply_customer_filters(req, docs)
    docs = _newest_first(docs)
    page, has_more, err = _list_page(req, docs, "customer")
    if err != None:
        return err
    return respond(200, {"object": "list", "data": page, "has_more": has_more, "url": "/v1/customers"})

# _apply_customer_filters maps the real Stripe customer-list query params
# (email exact match, created exact/range) to query_select clauses, applied
# before paging like the real API. Customers created without an email do not
# match an `email` filter (missing field matches only !=).
#
# Deleted customers are ALWAYS excluded: real Stripe list endpoints never
# return deleted customers (there is no include-deleted parameter). The
# ["deleted", "!=", True] clause keeps docs without a `deleted` key (missing
# field matches !=) and drops the deleted ones, so the seed + live customers
# flow through untouched.
def _apply_customer_filters(req, docs):
    f = [["deleted", "!=", True]]
    email = _get_query(req, "email")
    if email != "":
        f.append(["email", "=", email])
    _created_filters(req, f)
    return query_select(docs, f)

# POST /v1/customers/{id} — update a customer (merge fields from body).
# Deleted customers cannot be updated: Stripe reports a resource_missing
# invalid_request_error for mutations on a deleted customer.
def on_update_customer(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("customers")
    doc = c.get(id)
    if doc == None or doc.get("deleted", False) == True:
        return _not_found("customer", id)

    body = req["body"]
    if body != None:
        for k in body:
            doc[k] = body[k]

    c.update(id, doc)
    return respond(200, doc)

# DELETE /v1/customers/{id} — delete a customer (SOFT delete, like real
# Stripe): the stored object is kept and flagged deleted:true rather than
# removed. The customer stays retrievable by id (with the flag), disappears
# from list endpoints, cannot be updated, and a customer.deleted event is
# emitted (recorded in the events collection + signed webhook delivery).
def on_delete_customer(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("customers")
    doc = c.get(id)
    if doc == None:
        return _not_found("customer", id)

    doc["deleted"] = True
    c.update(id, doc)
    _signed_emit("customer.deleted", doc)
    return respond(200, {"id": id, "object": "customer", "deleted": True})
