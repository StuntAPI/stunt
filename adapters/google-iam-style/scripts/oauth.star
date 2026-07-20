# OAuth2 JWT-bearer token exchange handler.
#
# This simulates the service-account JWT exchange:
# POST /oauth2/v4/token (grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer)
#
# The client sends a signed JWT assertion. The mock accepts any non-empty
# assertion and mints an access token. The assertion is a base64url-encoded
# JWT with the service account email as the issuer (iss claim).

# Shared helpers (_bearer, _require_bearer, _contains, _pad3) are preloaded
# from scripts/lib.star.

# on_jwt_exchange handles the JWT-bearer grant.
def on_jwt_exchange(req):
    body = req["body"]
    if body == None:
        body = {}

    grant_type = body.get("grant_type", "")
    if grant_type != "urn:ietf:params:oauth:grant-type:jwt-bearer":
        return respond(400, {
            "error": "unsupported_grant_type",
            "error_description": "Only jwt-bearer grant is supported.",
        })

    assertion = body.get("assertion", "")
    if assertion == "":
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "Missing assertion.",
        })

    # The assertion is a JWT (header.payload.signature). We extract the
    # payload (middle segment) to determine the service account email.
    # Since it's base64url, we just check it has 3 dot-separated parts.
    # In a real scenario, the assertion would be signed. We accept any
    # syntactically valid JWT shape.
    sa_email = _extract_sa_from_assertion(assertion)

    # Mint an access token.
    token_seq = store_kv_incr("iam", "token_seq")
    access = "ya29.mock-iam-token-" + str(token_seq)

    tc = store_collection("tokens")
    tc.insert({
        "id": access,
        "service_account": sa_email,
        "scope": body.get("scope", "https://www.googleapis.com/auth/cloud-platform"),
    })

    return respond(200, {
        "access_token": access,
        "expires_in": 3600,
        "token_type": "Bearer",
    })

# _extract_sa_from_assertion attempts to find the service account email from
# the JWT assertion payload. The assertion is base64url-encoded, so we look
# for the gserviceaccount.com pattern in the raw string.
def _extract_sa_from_assertion(assertion):
    # Try to find a gserviceaccount.com pattern.
    idx = assertion.find("gserviceaccount.com")
    if idx < 0:
        # Can't decode base64 in Starlark; return a default.
        return "mock-sa@mock-project.iam.gserviceaccount.com"
    # Walk backwards to find the start of the email.
    start = idx
    while start > 0 and assertion[start-1] != "." and assertion[start-1] != "@":
        # Look for alphanumeric, @, - characters.
        ch = assertion[start-1]
        if (ch >= "a" and ch <= "z") or (ch >= "A" and ch <= "Z") or (ch >= "0" and ch <= "9") or ch == "-" or ch == "@":
            start -= 1
        else:
            break
    end = idx + len("gserviceaccount.com")
    return assertion[start:end]
