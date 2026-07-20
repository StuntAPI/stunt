# Sign in with Apple — OAuth2 handlers.
#
# GET  /auth/authorize  → 302 redirect with code+state (auth code, single-use)
# POST /auth/token      → token exchange (auth code) or refresh
# GET  /auth/keys       → JWKS public key set

# Shared helpers (_check_client_secret_jwt, _mint_id_token, _generate_user_id,
# _generate_email, _b64url_encode, _b64url_decode, _contains, _jose_header,
# _jwt_payload) are preloaded from scripts/lib.star.

# on_authorize handles the authorization-code redirect.
# GET /auth/authorize?client_id&redirect_uri&state&response_type=code&scope
def on_authorize(req):
    redirect_uri = req["query"].get("redirect_uri", "")
    state = req["query"].get("state", "")
    client_id = req["query"].get("client_id", "")
    response_type = req["query"].get("response_type", "")

    if redirect_uri == "" or client_id == "":
        return respond(400, {
            "error": "invalid_request",
            "error_description": "missing required parameters (client_id, redirect_uri)",
        })

    if response_type != "code":
        return respond(400, {
            "error": "unsupported_response_type",
        })

    code_seq = store_kv_incr("siwa", "code_seq")
    code = "c." + str(code_seq) + "." + "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"

    # Store the auth code (single-use).
    cc = store_collection("auth_codes")
    cc.insert({
        "id": code,
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "scope": req["query"].get("scope", ""),
    })

    sep = "?"
    if _contains(redirect_uri, "?"):
        sep = "&"

    # Build redirect URL with code and state.
    location = redirect_uri + sep + "code=" + code
    if state != "":
        location = location + "&state=" + state

    return respond(302, headers={"Location": location})

# on_token handles both authorization_code and refresh_token grants.
# POST /auth/token (form-encoded)
def on_token(req):
    body = req["body"]
    if body == None:
        body = {}
    grant_type = body.get("grant_type", "")

    if grant_type == "refresh_token":
        return _handle_refresh(body)
    if grant_type == "authorization_code":
        return _handle_auth_code(body)

    return respond(400, {
        "error": "unsupported_grant_type",
    })

# _handle_auth_code exchanges a single-use auth code for tokens.
def _handle_auth_code(body):
    code = body.get("code", "")
    client_id = body.get("client_id", "")
    client_secret = body.get("client_secret", "")
    redirect_uri = body.get("redirect_uri", "")

    # Validate client_secret is a structurally valid ES256 JWT.
    if not _check_client_secret_jwt(client_secret):
        return respond(400, {
            "error": "invalid_client",
            "error_description": "client_secret must be a valid ES256-signed JWT",
        })

    # Look up the auth code (single-use).
    cc = store_collection("auth_codes")
    code_doc = cc.get(code)
    if code_doc == None:
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "invalid or expired authorization code",
        })

    # Verify client_id and redirect_uri match.
    if code_doc.get("client_id", "") != client_id:
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "client_id mismatch",
        })
    if redirect_uri != "" and code_doc.get("redirect_uri", "") != redirect_uri:
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "redirect_uri mismatch",
        })

    # Consume the code (single-use).
    cc.delete(code)

    # Mint tokens.
    user_id = _generate_user_id()
    user_seq = store_kv_get("siwa", "user_seq")
    if user_seq == None:
        user_seq = "1"
    email = _generate_email(_to_int(user_seq))

    access_seq = store_kv_incr("siwa", "access_seq")
    refresh_seq = store_kv_incr("siwa", "refresh_seq")
    access = "ayh." + str(access_seq) + ".mock_access_token"
    refresh = "r." + str(refresh_seq) + ".mock_refresh_token"

    id_token = _mint_id_token(client_id, user_id, email)

    # Store tokens.
    tc = store_collection("tokens")
    tc.insert({
        "id": access,
        "client_id": client_id,
        "user_id": user_id,
        "email": email,
        "type": "access",
    })
    tc.insert({
        "id": refresh,
        "client_id": client_id,
        "user_id": user_id,
        "email": email,
        "type": "refresh",
    })

    return respond(200, {
        "access_token": access,
        "token_type": "Bearer",
        "expires_in": 3600,
        "refresh_token": refresh,
        "id_token": id_token,
    })

# _handle_refresh exchanges a refresh token for a new access token.
def _handle_refresh(body):
    refresh_token = body.get("refresh_token", "")
    client_id = body.get("client_id", "")
    client_secret = body.get("client_secret", "")

    if not _check_client_secret_jwt(client_secret):
        return respond(400, {
            "error": "invalid_client",
            "error_description": "client_secret must be a valid ES256-signed JWT",
        })

    tc = store_collection("tokens")
    doc = tc.get(refresh_token)
    if doc == None or doc.get("type", "") != "refresh":
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "invalid refresh token",
        })

    if doc.get("client_id", "") != client_id:
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "client_id mismatch",
        })

    # Mint a new access token.
    access_seq = store_kv_incr("siwa", "access_seq")
    access = "ayh." + str(access_seq) + ".mock_access_token"

    tc.insert({
        "id": access,
        "client_id": client_id,
        "user_id": doc.get("user_id", ""),
        "email": doc.get("email", ""),
        "type": "access",
    })

    return respond(200, {
        "access_token": access,
        "token_type": "Bearer",
        "expires_in": 3600,
    })

# on_get_keys returns the JWKS public key set.
# GET /auth/keys
def on_get_keys(req):
    return respond(200, {
        "keys": [
            {
                "kty": "EC",
                "kid": "A1B2C3D4E5",
                "use": "sig",
                "alg": "ES256",
                "crv": "P-256",
                "x": "LBL-ty8jZ3j8mRYf3BkQ0q2yJ5QWYs3q5m4Y6o7p8q0",
                "y": "M5gTc9b3Z8s2N4rV7wX1Y3aB6cD9eF2gH5iJ8kL0mN3",
            },
            {
                "kty": "EC",
                "kid": "F6G7H8I9J0",
                "use": "sig",
                "alg": "ES256",
                "crv": "P-256",
                "x": "oP1qR2sT3uV4wX5yZ6aB7cD8eF9gH0iJ1kL2mN3oP4",
                "y": "Q5rS6tU7vW8xY9zA0bC1dE2fG3hI4jK5lM6nO7pQ8",
            },
        ],
    })
