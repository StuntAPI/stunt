# OAuth2 handler — Square OAuth2 token endpoint.
#
# POST /oauth2/token
#   form: grant_type=authorization_code&code=...&client_id=...&client_secret=...
#   -> { access_token, token_type:"Bearer", expires_at, merchant_id }

def on_token(req):
    body = req["body"]
    if body == None:
        body = {}

    # Square expects form-encoded body for OAuth.
    grant_type = body.get("grant_type", "")
    if grant_type == None:
        grant_type = ""

    if grant_type == "":
        return _sq_err(400, "INVALID_REQUEST_ERROR", "MISSING_REQUIRED_PARAMETER", "grant_type is required")

    n = store_kv_incr("square", "token_seq")
    access_token = "EAAA" + str(5000000000 + n) + "_mock_access_token"

    # Store the token for validation.
    tc = store_collection("access_tokens")
    tc.insert({"id": access_token})

    return respond(200, {
        "access_token": access_token,
        "token_type": "Bearer",
        "expires_at": "2025-12-31T23:59:59Z",
        "merchant_id": "ML" + str(6000000000 + n),
    })
