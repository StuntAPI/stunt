# Member resolution handler.
#
# GET /v2/userinfo (Bearer) -> { sub, name, email, picture }
# Returns 401 if the token is invalid.

# Shared helpers (_bearer, _member_for_token) are preloaded from
# scripts/lib.star.

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
