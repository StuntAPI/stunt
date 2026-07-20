# Shared library for paypal-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# _bearer extracts the Bearer token from the Authorization header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _require_auth validates the bearer token. Returns None if authorized, or
# an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return _pp_err_simple(401, "AUTHENTICATION_FAILURE", "Authentication failed due to invalid authentication credentials.")
    c = store_collection("access_tokens")
    doc = c.get(token)
    if doc == None:
        return _pp_err_simple(401, "AUTHENTICATION_FAILURE", "Access token does not exist.")
    return None

# _pp_err returns a PayPal-style error response.
def _pp_err(status, name, message, debug_id):
    return respond(status, {
        "name": name,
        "details": [{"issue": "ERROR", "description": message}],
        "message": message,
        "debug_id": debug_id,
    })

# _pp_err_simple returns an error with a synthetic debug_id.
def _pp_err_simple(status, name, message):
    n = store_kv_incr("paypal", "debug_seq")
    return respond(status, {
        "name": name,
        "details": [{"issue": "ERROR", "description": message}],
        "message": message,
        "debug_id": "debug-" + str(n),
    })

# _links returns a standard PayPal links array for an order.
def _order_links(order_id, status):
    links = [
        {"href": "https://api.stunt.test/v2/checkout/orders/" + order_id, "rel": "self", "method": "GET"},
    ]
    if status == "CREATED":
        links.append({"href": "https://www.stunt.test/checkoutnow?token=" + order_id, "rel": "approve", "method": "GET"})
        links.append({"href": "https://api.stunt.test/v2/checkout/orders/" + order_id + "/capture", "rel": "capture", "method": "POST"})
    if status == "APPROVED":
        links.append({"href": "https://api.stunt.test/v2/checkout/orders/" + order_id + "/capture", "rel": "capture", "method": "POST"})
        links.append({"href": "https://api.stunt.test/v2/checkout/orders/" + order_id + "/authorize", "rel": "authorize", "method": "POST"})
    return links

# _order_public returns the PayPal-shaped order object.
def _order_public(doc):
    status = doc.get("status", "CREATED")
    result = {
        "id": doc["id"],
        "status": status,
        "intent": doc.get("intent", "CAPTURE"),
        "purchase_units": doc.get("purchase_units", []),
        "create_time": doc.get("create_time", ""),
        "links": _order_links(doc["id"], status),
    }
    return result

# _order_id generates an order ID from the sequence counter.
def _order_id():
    n = store_kv_incr("paypal", "order_seq")
    return "ORDERID-" + str(n)

# _capture_id generates a capture ID from the sequence counter.
def _capture_id():
    n = store_kv_incr("paypal", "capture_seq")
    return "CAPTUREID-" + str(n)

# _refund_id generates a refund ID from the sequence counter.
def _refund_id():
    n = store_kv_incr("paypal", "refund_seq")
    return "REFUNDID-" + str(n)

# _request_id returns or stores the PayPal-Request-Id header for idempotency.
def _get_request_id(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    rid = headers.get("PayPal-Request-Id", "")
    if rid == None:
        rid = ""
    return rid

# _check_idempotency returns a cached response for a PayPal-Request-Id, or None.
def _check_idempotency(req, path):
    rid = _get_request_id(req)
    if rid == "":
        return None
    cached = store_kv_get("paypal", "idem_" + path + "_" + rid)
    if cached != None and cached != "":
        # Return the cached order by ID.
        oc = store_collection("orders")
        return oc.get(cached)
    return None

# _store_idempotency caches an order ID for a PayPal-Request-Id.
def _store_idempotency(req, path, order_id):
    rid = _get_request_id(req)
    if rid == "":
        return
    store_kv_set("paypal", "idem_" + path + "_" + rid, order_id)
