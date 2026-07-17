# Charges handlers — Starlark stateful logic backed by store_collection.
#
# Each handler receives `req` with keys: method, path, headers, body, params.
# Returns respond(status, body, headers).

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
# KV store as a sequence counter. Produces ids like "ch_1", "ch_2", ...
def _next_id(prefix):
    # Atomic increment via store_kv_incr (race-free under concurrent requests).
    return prefix + "_" + str(store_kv_incr("stripe", prefix + "_seq"))

# POST /v1/charges — create a charge (status starts as "pending").
def on_create(req):
    err = _require_auth(req)
    if err != None:
        return err

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
    events_emit("charge.created", doc)

    return respond(201, doc)

# GET /v1/charges/{id} — retrieve a single charge.
def on_retrieve(req):
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
def on_list(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("charges")
    docs = c.list()
    return respond(200, {"object": "list", "data": docs, "has_more": False, "url": "/v1/charges"})

# POST /v1/charges/{id}/capture — capture a pending charge (set status succeeded).
def on_capture(req):
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
    events_emit("charge.updated", doc)

    return respond(200, doc)

# POST /v1/charges/{id}/refund — refund a charge (set status refunded).
def on_refund(req):
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
    events_emit("charge.refunded", doc)

    return respond(200, doc)
