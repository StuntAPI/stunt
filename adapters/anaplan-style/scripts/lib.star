# Shared library for anaplan-style adapter scripts.

# _check_auth validates Anaplan auth. Accepts either Basic auth
# (email:password) or Bearer token.
def _check_auth(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:6] == "Basic ":
        return auth[6:]
    if auth[:7] == "Bearer ":
        return auth[7:]
    return None

# _require_auth returns (token, None) if auth is present, or
# (None, error_response) if missing.
def _require_auth(req):
    token = _check_auth(req)
    if token == None:
        return None, respond(401, {
            "status": "FAILURE",
            "statusMessage": "Authentication is required. Provide Basic or Bearer credentials.",
        })
    return token, None

# _to_int parses a decimal string to int.
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

# _seed populates default workspaces.
def _seed():
    if store_kv_get("anaplan", "seeded") == "yes":
        return
    store_kv_set("anaplan", "seeded", "yes")

    wc = store_collection("workspaces")
    wc.insert({
        "id": "8a819c8645a0aa8e0005c715c7ad49b9",
        "name": "Supply Chain Planning",
        "active": True,
        "size": 1048576,
    })
    wc.insert({
        "id": "8a819c8645b1bb9f0006c825d8be50c0",
        "name": "Financial Forecasting",
        "active": True,
        "size": 2097152,
    })
