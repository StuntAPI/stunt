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
    grant_type = body.get("grant_type") or ""
    if grant_type == None:
        grant_type = ""

    if grant_type == "":
        return _sq_err(400, "INVALID_REQUEST_ERROR", "MISSING_REQUIRED_PARAMETER", "grant_type is required")

    n = store_kv_incr("square", "token_seq")
    access_token = "EAAA" + str((5*1000*1000*1000) + n) + "_mock_access_token"

    # Store the token for validation, with a 30-day TTL enforced by
    # _require_auth (expiry computed at runtime — never a hardcoded epoch).
    expires_at = clock.now_unix() + 30 * 24 * 3600
    tc = store_collection("access_tokens")
    tc.insert({
        "id": access_token,
        "expires_at": expires_at,
    })

    return respond(200, {
        "access_token": access_token,
        "token_type": "Bearer",
        "expires_at": clock.unix_to_rfc3339(expires_at),
        "merchant_id": "ML" + str((6*1000*1000*1000) + n),
    })
