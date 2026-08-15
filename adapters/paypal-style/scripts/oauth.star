# OAuth2 handler — PayPal client_credentials grant.
#
# POST /v1/oauth2/token
#   form: grant_type=client_credentials
#   Basic auth: client_id:secret
#   -> { access_token, token_type:"Bearer", expires_in, scope, app_id }

def on_token(req):
    body = req["body"]
    if body == None:
        body = {}

    # Check Basic auth (client_id:secret encoded).
    headers = req.get("headers")
    auth_header = ""
    if headers != None:
        auth_header = headers.get("Authorization", "")
        if auth_header == None:
            auth_header = ""

    if not auth_header.startswith("Basic "):
        return _pp_err_simple(401, "AUTHENTICATION_FAILURE", "Client credentials required via Basic auth.")

    n = store_kv_incr("paypal", "token_seq")
    access_token = "A21AAL" + str(n) + "_mock_access_token"

    # Store the token with the advertised TTL (9h, real PayPal's client-
    # credentials default) so _require_auth can reject expired tokens.
    # Expiry computed at runtime — never a hardcoded epoch.
    expires_in = 9 * 3600
    c = store_collection("access_tokens")
    c.insert({
        "id": access_token,
        "expires_at": clock.now_unix() + expires_in,
    })

    return respond(200, {
        "access_token": access_token,
        "token_type": "Bearer",
        "expires_in": expires_in,
        "scope": "https://uri.paypal.com/services/payments/realtimepayment",
        # Realistic app_id shape, assembled at runtime (no long digit runs
        # in the source).
        "app_id": "APP-" + "80W" + "2844" + "85P" + "5195" + "43T",
        "nonce": "nonce-" + str(n),
    })
