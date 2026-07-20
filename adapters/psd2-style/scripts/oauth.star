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
    access_token = "psd2-token-" + str(9000000000 + n)

    # Store the token.
    tc = store_collection("access_tokens")
    tc.insert({"id": access_token})

    return respond(200, {
        "access_token": access_token,
        "token_type": "Bearer",
        "expires_in": 3600,
        "scope": "PIS AIS",
        "consentId": "",
    })
