# Member resolution handler.
#
# GET /v2/userinfo (Bearer) -> { sub, name, email, picture }
# Returns 401 if the token is invalid.

# NOTE: Starlark load() is unavailable in stunt, so shared helpers are inlined.

def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

def _member_for_token(req):
    token = _bearer(req)
    if token == "":
        return None
    c = store_collection("tokens")
    doc = c.get(token)
    if doc == None:
        return None
    return doc

# on_userinfo returns the member bound to the Bearer token.
def on_userinfo(req):
    member = _member_for_token(req)
    if member == None:
        return respond(401, {"status": 401, "code": "AUTHORIZED", "message": "Invalid or expired token"})

    return respond(200, {
        "sub": member["sub"],
        "name": member["name"],
        "email": member["email"],
        "picture": member["picture"],
    })
