# Shared library for printify-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        return ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _require_auth validates that a non-empty bearer key is present.
# Returns None if authorized, or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return respond(401, {
            "status": 401,
            "message": "Missing Bearer token in Authorization header.",
        })
    return None

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _pad4 pads a sequence number to 4 digits with leading zeros.
def _pad4(n):
    if n < 10:
        return "000" + str(n)
    if n < 100:
        return "00" + str(n)
    if n < 1000:
        return "0" + str(n)
    return str(n)

# _strip_json removes a trailing ".json" suffix from a path param value.
# Route params like {product_id} capture the full path segment including
# any .json suffix, so this normalizes it back to the bare id.
def _strip_json(s):
    if s == None:
        return ""
    suffix = ".json"
    slen = len(suffix)
    if len(s) >= slen and s[-slen:] == suffix:
        return s[:-slen]
    return s

# _product_id generates a Printify-style hex product id (24 chars).
def _product_id(seq):
    return "5e5d3c8c000000000000" + _pad4(seq)

# _order_id returns a numeric string order id.
def _order_id(seq):
    return str(100000 + seq)

# _synth_total returns a synthetic order total (whole USD units) derived from
# the line-item quantities, so consumers that read total_price get a real
# number rather than a missing field. Minimum 20.
def _synth_total(line_items):
    total = 0
    for it in line_items:
        q = it.get("quantity", 1)
        if type(q) != "int":
            q = 1
        total += q * 20
    if total == 0:
        total = 20
    return total

# _new_order builds a synthetic Printify-style order document from a create
# request body. Accepts either the top-level "shipping_address" key (the real
# /v1/shops/{shop_id}/orders.json shape) or "address_to", and always includes
# total_price + currency so downstream parsers have the fields they read.
def _new_order(body):
    seq = store_kv_incr("printify", "order_seq")
    oid = _order_id(seq)
    ts = 1700000000 + seq
    line_items = body.get("line_items", [])
    addr = body.get("shipping_address", body.get("address_to", {}))
    return {
        "id": oid,
        "status": "pending",
        "total_price": _synth_total(line_items),
        "currency": "USD",
        "shipping_method": body.get("shipping_method", 1),
        "line_items": line_items,
        "address_to": addr,
        "shipping_address": addr,
        "created_at": ts,
        "updated_at": ts,
        "is_test": True,
    }
