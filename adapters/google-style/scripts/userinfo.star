# Userinfo handler.
#
# GET /oauth2/v3/userinfo (Bearer) -> { sub, name, email, picture }
# Returns 401 if the token is invalid.

# Shared helpers (_bearer, _user_for_token) are preloaded from
# scripts/lib.star.

# on_userinfo returns the user bound to the Bearer token.
def on_userinfo(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    return respond(200, {
        "sub": user["sub"],
        "name": user["name"],
        "email": user["email"],
        "picture": user["picture"],
        "email_verified": True,
    })
