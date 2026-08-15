# OAuth2 JWT-bearer token exchange handler.
#
# This simulates the service-account JWT exchange:
# POST /oauth2/v4/token (grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer)
#
# The client sends a signed RS256 JWT assertion. The assertion is VERIFIED
# cryptographically (signature against the mock Google public key served at
# GET /oauth2/v3/certs, plus iss/exp/aud claim checks) — not just parsed.
# On success an access token is minted; when the scope includes openid a
# real RS256 id_token is returned too (Google returns one on SA flows).
#
# GET /oauth2/v3/certs serves the JWKS for both directions.

# Shared helpers (_bearer, _require_bearer, _contains, _pad3,
# _verify_assertion, _mint_id_token, _JWT_PUBLIC_KEY) are preloaded from
# scripts/lib.star.

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

    # Verify the assertion cryptographically: RS256 signature over
    # header.payload against the mock Google key, iss non-empty, exp in
    # the future, aud == this token endpoint.
    claims = _verify_assertion(assertion)
    if claims == None:
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "Invalid JWT: signature or claims verification failed.",
        })

    # The service-account identity is the assertion's iss claim.
    sa_email = claims.get("iss", "")

    scope = body.get("scope", claims.get("scope", "https://www.googleapis.com/auth/cloud-platform"))

    # Mint an access token.
    token_seq = store_kv_incr("iam", "token_seq")
    access = "ya29.mock-iam-token-" + str(token_seq)

    tc = store_collection("tokens")
    tc.insert({
        "id": access,
        "service_account": sa_email,
        "scope": scope,
        "expires_at": clock.now_unix() + 3600,
    })

    resp = {
        "access_token": access,
        "expires_in": 3600,
        "token_type": "Bearer",
    }
    if _contains(scope, "openid"):
        resp["id_token"] = _mint_id_token(sa_email, scope)
    return respond(200, resp)

# on_certs serves the JWKS at Google's real discovery path
# (/oauth2/v3/certs). The key is REAL: the fixed synthetic RSA keypair whose
# private half signs id_tokens and whose public half verifies jwt-bearer
# assertions.
def on_certs(req):
    key = crypto.rsa_public_jwk(_JWT_PUBLIC_KEY)
    key["kid"] = _JWT_KID
    key["alg"] = "RS256"
    key["use"] = "sig"
    return respond(200, {"keys": [key]})
