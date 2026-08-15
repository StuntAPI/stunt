# OAuth2 token endpoint — Apple Search Ads client-credential flow.
#
# POST /api/oauth2/token
#   Content-Type: application/x-www-form-urlencoded (JSON also accepted)
#   grant_type=client_credentials&client_id=<team/org id>&client_secret=<JWT>
#   → 200 {"access_token": "...", "token_type": "Bearer", "expires_in": 3600}
#
# The real flow signs an ES256 JWT (header: alg/kid; claims: sub=<client
# id>, aud=https://appleid.apple.com/oauth/token) with the private key and
# exchanges it for a bearer. The simulator validates the client_secret JWT
# structurally — 3 non-empty segments, ES256 + kid in the JOSE header, and
# a sub claim in the payload — then mints an opaque access token registered
# in the KV token registry with a one-hour expiry. The ECDSA signature is
# not verified (documented stretch goal).
#
# Shared helpers (_asa_valid_client_secret, _asa_mint_access_token) are
# preloaded from scripts/lib.star.

# on_token handles the client_credentials token exchange.
def on_token(req):
    body = req.get("body")
    if body == None:
        body = {}

    grant_type = body.get("grant_type", "")
    if grant_type != "client_credentials":
        return respond(400, {
            "error": "unsupported_grant_type",
            "error_description": "grant_type must be client_credentials",
        })

    client_secret = body.get("client_secret", "")
    if client_secret == None:
        client_secret = ""
    if not _asa_valid_client_secret(client_secret):
        return respond(400, {
            "error": "invalid_client",
            "error_description": "client_secret must be an ES256 JWT with a kid and a sub claim",
        })

    access = _asa_mint_access_token()
    return respond(200, {
        "access_token": access,
        "token_type": "Bearer",
        "expires_in": _ACCESS_TTL,
    })
