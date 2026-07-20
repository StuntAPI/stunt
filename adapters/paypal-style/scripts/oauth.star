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

    c = store_collection("access_tokens")
    c.insert({"id": access_token})

    return respond(200, {
        "access_token": access_token,
        "token_type": "Bearer",
        "expires_in": 32400,
        "scope": "https://uri.paypal.com/services/payments/realtimepayment",
        "app_id": "APP-80W284485P519543T",
        "nonce": "nonce-" + str(n),
    })
