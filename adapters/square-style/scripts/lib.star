# Shared library for square-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# Square requires:
#   1. Authorization: Bearer <access_token>
#   2. Square-Version: <dated version>
# We check presence of both.

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

# _square_version extracts the Square-Version header.
def _square_version(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    return headers.get("Square-Version", "")

# _require_auth validates the bearer token AND Square-Version header.
# Returns None if authorized, or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return _sq_err(401, "UNAUTHORIZED", "ACCESS_TOKEN_EXPIRED", "Missing or invalid Authorization header")
    return None

# _require_version validates the Square-Version header is present.
# Must be called after _require_auth.
def _require_version(req):
    version = _square_version(req)
    if version == "":
        return _sq_err(400, "BAD_REQUEST", "MISSING_REQUIRED_HEADER", "Square-Version header is required")
    return None

# _sq_err returns a Square-style error response.
# Shape: { errors: [{ code, category, detail, field }] }
def _sq_err(status, category, code, detail):
    return respond(status, {
        "errors": [
            {
                "category": category,
                "code": code,
                "detail": detail,
                "field": "",
            },
        ],
    })

# _sq_err_field returns a Square-style error with a field attribute.
def _sq_err_field(status, category, code, detail, field):
    return respond(status, {
        "errors": [
            {
                "category": category,
                "code": code,
                "detail": detail,
                "field": field,
            },
        ],
    })

# _safe_int attempts to convert a value to int, returning fallback on failure.
# Starlark doesn't have try/except, so we check if it's already an int or a
# numeric string before calling int().
def _safe_int(val, fallback):
    if type(val) == "int":
        return val
    if type(val) == "string":
        # Check if the string is numeric.
        if _is_numeric(val):
            return int(val)
    return fallback

# _is_numeric checks if a string represents a positive integer.
def _is_numeric(s):
    if s == "" or s == None:
        return False
    for c in range(len(s)):
        ch = s[c]
        if ch != "0" and ch != "1" and ch != "2" and ch != "3" and ch != "4" and ch != "5" and ch != "6" and ch != "7" and ch != "8" and ch != "9":
            return False
    return True

# _payment_id generates a Square payment ID.
def _payment_id():
    n = store_kv_incr("square", "payment_seq")
    return "p_" + str(1000000000 + n)

# _refund_id generates a Square refund ID.
def _refund_id():
    n = store_kv_incr("square", "refund_seq")
    return "r_" + str(2000000000 + n)

# _order_id generates a Square order ID.
def _order_id():
    n = store_kv_incr("square", "order_seq")
    return "ord_" + str(3000000000 + n)

# _check_idempotency returns a cached response for an idempotency_key, or None.
def _check_idempotency(req, collection_name):
    body = req.get("body")
    if body == None:
        return None
    key = body.get("idempotency_key", "")
    if key == None or key == "":
        return None
    cached_id = store_kv_get("square", "idem_" + collection_name + "_" + key)
    if cached_id != None and cached_id != "":
        c = store_collection(collection_name)
        return c.get(cached_id)
    return None

# _store_idempotency caches a resource ID for an idempotency_key.
def _store_idempotency(req, collection_name, resource_id):
    body = req.get("body")
    if body == None:
        return
    key = body.get("idempotency_key", "")
    if key == None or key == "":
        return
    store_kv_set("square", "idem_" + collection_name + "_" + key, resource_id)

# _payment_public returns the Square-shaped payment object.
def _payment_public(doc):
    return {
        "id": doc["id"],
        "status": doc.get("status", "APPROVED"),
        "source_id": doc.get("source_id", ""),
        "amount_money": doc.get("amount_money", {}),
        "location_id": doc.get("location_id", ""),
        "receipt_url": doc.get("receipt_url", ""),
        "created_at": doc.get("created_at", "2024-01-01T00:00:00Z"),
        "order_id": doc.get("order_id", ""),
        "card_details": doc.get("card_details", {
            "card": {
                "card_brand": "VISA",
                "last_4": "1111",
                "exp_month": 3,
                "exp_year": 2030,
                "card_type": "CREDIT",
                "prepaid_type": "NOT_PREPAID",
                "bin": "411111",
            },
            "entry_method": "KEYED",
            "cvv_status": "ACCEPTED",
            "avs_status": "PASS",
            "status": "CAPTURED",
        }),
    }

# _refund_public returns the Square-shaped refund object.
def _refund_public(doc):
    return {
        "id": doc["id"],
        "status": doc.get("status", "PENDING"),
        "payment_id": doc.get("payment_id", ""),
        "amount_money": doc.get("amount_money", {}),
        "location_id": doc.get("location_id", ""),
        "created_at": doc.get("created_at", "2024-01-01T00:00:00Z"),
        "reason": doc.get("reason", ""),
    }

# _order_public returns the Square-shaped order object.
def _order_public(doc):
    return {
        "id": doc["id"],
        "location_id": doc.get("location_id", ""),
        "state": doc.get("state", "OPEN"),
        "line_items": doc.get("line_items", []),
        "total_money": doc.get("total_money", {}),
        "created_at": doc.get("created_at", "2024-01-01T00:00:00Z"),
    }
