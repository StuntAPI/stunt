# Shared library for braintree-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# Braintree auth: Bearer token OR public key + private key (basic auth style).
# We check presence of either.

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

# _require_auth validates the auth (Bearer or basic). Returns None if
# authorized, or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token != "":
        return None
    # Check for basic auth (public_key:private_key).
    headers = req.get("headers")
    if headers != None:
        auth = headers.get("Authorization", "")
        if auth != None and auth.startswith("Basic "):
            return None
    return _bt_err(401, "AUTHENTICATION", "Authentication failed: missing or invalid credentials")

# _bt_err returns a Braintree-style error response (REST).
def _bt_err(status, code, message):
    return respond(status, {
        "error": {
            "code": code,
            "message": message,
        },
    })

# _bt_graphql_error returns a Braintree GraphQL error envelope.
def _bt_graphql_error(message):
    return respond(200, {
        "data": None,
        "errors": [{"message": message}],
    })

# _txn_id generates a Braintree transaction ID.
def _txn_id():
    n = store_kv_incr("braintree", "txn_seq")
    return _alpha_id("t", n)

# _customer_id generates a Braintree customer ID.
def _customer_id():
    n = store_kv_incr("braintree", "customer_seq")
    return "customer_" + str(n)

# _payment_method_token generates a Braintree payment method token.
def _payment_method_token():
    n = store_kv_incr("braintree", "pm_seq")
    return _alpha_id("pm", n)

# _alpha_id generates an alphanumeric ID with a prefix.
def _alpha_id(prefix, n):
    alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
    s = ""
    val = n
    if val == 0:
        s = "0"
    while val > 0:
        s = alphabet[val % 36] + s
        val = val // 36
    # Pad to 6 chars.
    while len(s) < 6:
        s = alphabet[0] + s
    return prefix + s

# _txn_public returns the Braintree-shaped transaction object.
def _txn_public(doc):
    return {
        "id": doc.get("id", ""),
        "status": doc.get("status", "authorized"),
        "type": doc.get("type", "sale"),
        "amount": doc.get("amount", "0.00"),
        "currency": doc.get("currency", "USD"),
        "customer": doc.get("customer", {}),
        "creditCard": doc.get("creditCard", {
            "last4": "1111",
            "cardType": "Visa",
            "expirationDate": "03/2030",
        }),
        "createdAt": doc.get("createdAt", "2024-06-15T12:30:00.000Z"),
    }

# _customer_public returns the Braintree-shaped customer object.
def _customer_public(doc):
    return {
        "id": doc.get("id", ""),
        "firstName": doc.get("firstName", ""),
        "lastName": doc.get("lastName", ""),
        "email": doc.get("email", ""),
        "createdAt": doc.get("createdAt", "2024-06-15T12:30:00.000Z"),
    }

# _client_token generates a synthetic client token for the Braintree Drop-in.
def _client_token():
    n = store_kv_incr("braintree", "ct_seq")
    return "production_cb_" + _alpha_id("ct", n + 10000)

# _contains checks if a string contains a substring (Starlark has no `in` for strings).
def _contains(haystack, needle):
    if haystack == None or needle == None:
        return False
    if len(needle) == 0:
        return True
    for i in range(len(haystack) - len(needle) + 1):
        if haystack[i:i + len(needle)] == needle:
            return True
    return False
