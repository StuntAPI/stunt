# Balance handler — returns a synthetic account balance.
# No state needed; the values are fixed synthetic placeholders.

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

# GET /v1/balance — return the account balance.
def on_get(req):
    err = _require_auth(req)
    if err != None:
        return err

    return respond(200, {
        "object": "balance",
        "available": [
            {"amount": 100000, "currency": "usd"},
        ],
        "pending": [
            {"amount": 50000, "currency": "usd"},
        ],
        "instant_available": [
            {"amount": 25000, "currency": "usd"},
        ],
        "livemode": False,
    })
