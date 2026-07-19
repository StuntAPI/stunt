# Profile handler — GET /v21.0/me (Bearer presence required).
#
# Returns the IG profile bound to the bearer token (matching the OAuth flow),
# with standard Graph API fields: id, username, followers_count, media_count.
# The token value is validated against the tokens collection — if the token
# is known, we return that user's profile; if not, a default mock profile.
#
# Shared helper (_bearer_present) is preloaded from scripts/lib.star.

# _bearer extracts the token from an Authorization: Bearer header.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# on_profile returns the mock Instagram profile for the current token.
def on_profile(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "code": 190}})

    token = _bearer(req)
    tc = store_collection("tokens")
    tok_doc = tc.get(token)

    if tok_doc != None:
        ig_user_id = tok_doc.get("ig_user_id", "ig_1")
        username = tok_doc.get("username", "mock_user_1")
    else:
        ig_user_id = "ig_1"
        username = "mock_user_1"

    return respond(200, {
        "id": ig_user_id,
        "username": username,
        "followers_count": 1000,
        "media_count": 0,
        "account_type": "BUSINESS",
    })
