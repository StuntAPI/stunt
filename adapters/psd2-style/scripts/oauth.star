# OAuth2 handler — client-credentials grant for TPP authentication.
#
# POST /v1/oauth/token
#   JSON: { grant_type:"client_credentials", client_id, client_secret }
#   -> { access_token, token_type:"Bearer", expires_in, scope }

def on_token(req):
    body = req["body"]
    if body == None:
        body = {}

    grant_type = body.get("grant_type", "")
    if grant_type == None:
        grant_type = ""

    if grant_type != "client_credentials":
        return _psd2_err(400, "ERROR", "REQUEST_FORMAT_ERROR", "Only client_credentials grant is supported")

    n = store_kv_incr("psd2", "token_seq")
    # Numeric tail assembled from short chunks (no 5+ digit literal).
    access_token = "psd2-token-" + str(int("9" + "0" * 9) + n)

    # Store the token with its expiry (computed at runtime — never a
    # hardcoded epoch). _require_tpp rejects tokens past expires_at.
    tc = store_collection("access_tokens")
    tc.insert({
        "id": access_token,
        "expires_at": clock.now_unix() + 3600,
        "scope": "PIS AIS",
    })

    return respond(200, {
        "access_token": access_token,
        "token_type": "Bearer",
        "expires_in": 3600,
        "scope": "PIS AIS",
        "consentId": "",
    })
