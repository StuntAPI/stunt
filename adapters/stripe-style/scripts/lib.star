# Shared library for stripe-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

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

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
def _to_int(s):
    if s == None:
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return n
    return n

# _stripe_account extracts the Stripe-Account header used by Stripe Connect
# to scope requests to a connected account. Returns None if absent.
def _stripe_account(req):
    headers = req.get("headers")
    if headers == None:
        return None
    acct = headers.get("Stripe-Account", "")
    if acct == None or acct == "":
        return None
    return acct

# _get_balance returns the available balance (in cents) for a connected
# account, tracked via the KV store. Defaults to 0 for new accounts.
def _get_balance(acct_id):
    val = store_kv_get("stripe", "bal_" + acct_id)
    return _to_int(val)

# _set_balance sets the available balance (in cents) for a connected account.
def _set_balance(acct_id, amount):
    store_kv_set("stripe", "bal_" + acct_id, str(amount))

# _not_found returns a standard Stripe-style 404 error response.
def _not_found(resource, id):
    return respond(404, {"error": {"message": "No such " + resource + ": " + id, "type": "invalid_request_error"}})
