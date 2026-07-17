# Customer handlers — Starlark stateful logic backed by store_collection.

# --- auth helper (duplicated in each script; Starlark load() is unavailable) ---

# _bearer_token extracts the bearer token from the Authorization header, or
# None if absent.
def _bearer_token(req):
    headers = req.get("headers")
    if headers == None:
        return None
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return None

# _require_auth validates the bearer token.
#
# Returns None if authorized, or an error-response dict to return from the
# handler if not.
#
# Dev bypass: tokens starting with "sk_test" are accepted WITHOUT
# identity_validate, for frictionless local testing.
def _require_auth(req):
    token = _bearer_token(req)
    if token == None:
        return respond(401, {"error": {"type": "authentication_error", "message": "Missing Authorization header. Provide 'Authorization: Bearer <token>'."}})

    # Dev bypass: sk_test tokens skip real validation.
    if token.startswith("sk_test"):
        return None

    # Real validation via the identity issuer.
    claims = identity_validate(token)
    if claims == None:
        return respond(401, {"error": {"type": "authentication_error", "message": "Invalid API Key provided."}})
    return None

# _next_id returns a monotonically-increasing provider-style ID using the
# KV store as a sequence counter. Produces ids like "cus_1", "cus_2", ...
def _next_id(prefix):
    # Atomic increment via store_kv_incr (race-free under concurrent requests).
    return prefix + "_" + str(store_kv_incr("stripe", prefix + "_seq"))

# POST /v1/customers — create a customer.
def on_create(req):
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
def on_retrieve(req):
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
def on_list(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("customers")
    docs = c.list()
    return respond(200, {"object": "list", "data": docs, "has_more": False, "url": "/v1/customers"})

# POST /v1/customers/{id} — update a customer (merge fields from body).
def on_update(req):
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
def on_delete(req):
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
