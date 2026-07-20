# Auth handlers — Marketo OAuth client_credentials token mint.
#
# GET /identity/oauth/token?client_id&client_secret&grant_type=client_credentials
#   -> {access_token, token_type, expires_in, scope}
#
# Marketo tokens expire every hour (the token-churn pain). This mock mints
# synthetic tokens with a 3600-second lifetime.

# Shared helpers from lib.star.

def on_token(req):
    q = req.get("query")
    if q == None:
        q = {}
    grant_type = q.get("grant_type", "")
    client_id = q.get("client_id", "")
    client_secret = q.get("client_secret", "")

    if grant_type != "client_credentials":
        return respond(400, {
            "error": "unsupported_grant_type",
            "error_description": "Only client_credentials is supported",
        })

    if client_id == "" or client_secret == "":
        return respond(400, {
            "error": "invalid_request",
            "error_description": "client_id and client_secret are required",
        })

    # Mint a synthetic access token.
    seq = store_kv_incr("marketo", "token_seq")
    access = "synthetic_token_" + str(seq)

    # Store it so we could validate later (though this mock accepts any
    # non-empty token).
    tc = store_collection("tokens")
    tc.insert({
        "id": access,
        "client_id": client_id,
        "scope": "",
    })

    return respond(200, {
        "access_token": access,
        "token_type": "bearer",
        "expires_in": 3600,
        "scope": "",
    })
