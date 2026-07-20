# Cognito Hosted UI OAuth handlers.
#
# GET  /oauth2/authorize  → 302 to redirect_uri?code=CODE&state=STATE
# POST /oauth2/token      → {access_token, id_token, refresh_token, ...}
# GET  /oauth2/userInfo   → {sub, username, email, ...} (Bearer)
# GET  /login             → 302 to /oauth2/authorize (hosted UI login)
# GET  /logout            → 302 to redirect_uri (hosted UI logout)

# on_authorize handles the authorization-code redirect.
# GET /oauth2/authorize?client_id=&redirect_uri=&response_type=code&scope=&state=
def on_authorize(req):
    redirect_uri = req["query"].get("redirect_uri", "")
    state = req["query"].get("state", "")
    client_id = req["query"].get("client_id", "")

    if redirect_uri == "" or client_id == "":
        return respond(400, {
            "error": "invalid_request",
            "error_description": "Missing required parameter: redirect_uri or client_id",
        })

    # Mint an auth code and create a synthetic user for this OAuth session.
    user = _mint_user()
    code_seq = store_kv_incr("cognito", "code_seq")
    code = "mock-auth-code-" + str(code_seq)

    cc = store_collection("oauth_codes")
    cc.insert({
        "id": code,
        "user_id": user["sub"],
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "used": False,
    })

    sep = "?"
    if _contains(redirect_uri, "?"):
        sep = "&"
    location = redirect_uri + sep + "code=" + code + "&state=" + state
    return respond(302, headers={"Location": location})

# on_token handles the authorization_code grant.
# POST /oauth2/token (form: grant_type=authorization_code, code, client_id, redirect_uri, [client_secret])
def on_token(req):
    # The body may be form-encoded for /oauth2/token.
    body = req["body"]
    if body == None:
        body = {}
    grant_type = body.get("grant_type", "")

    if grant_type == "authorization_code":
        return _handle_auth_code_grant(body)
    if grant_type == "refresh_token":
        return _handle_refresh_grant(body)
    return respond(400, {
        "error": "unsupported_grant_type",
        "error_description": "Unsupported grant_type: " + grant_type,
    })

# on_user_info returns the user info for a Bearer access token.
# GET /oauth2/userInfo (Bearer)
def on_user_info(req):
    tok = _bearer(req)
    if tok == "":
        return respond(401, {
            "error": "invalid_token",
            "error_description": "Access token is missing",
        })

    tc = store_collection("tokens")
    tok_doc = tc.get(tok)
    if tok_doc == None:
        return respond(401, {
            "error": "invalid_token",
            "error_description": "Invalid access token",
        })

    user_id = tok_doc.get("user_id", "")
    uc = store_collection("users")
    user = uc.get(user_id)
    if user == None:
        return respond(401, {
            "error": "invalid_token",
            "error_description": "User not found",
        })

    return respond(200, _user_info(user))

# on_login redirects to the authorize endpoint (Cognito hosted UI login).
# GET /login?client_id=&redirect_uri=&response_type=&scope=&state=
def on_login(req):
    # Redirect to /oauth2/authorize with the same params.
    qs = req["query"]
    parts = []
    for k in qs:
        parts.append(k + "=" + qs[k])
    location = "/oauth2/authorize"
    if len(parts) > 0:
        location = location + "?" + parts[0]
        for i in range(1, len(parts)):
            location = location + "&" + parts[i]
    return respond(302, headers={"Location": location})

# on_logout redirects to the redirect_uri (or a default).
# GET /logout?client_id=&logout_uri= or redirect_uri=
def on_logout(req):
    redirect = req["query"].get("logout_uri", req["query"].get("redirect_uri", ""))
    if redirect == "":
        redirect = "/"
    return respond(302, headers={"Location": redirect})

# --- internal ---

def _handle_auth_code_grant(body):
    code = body.get("code", "")
    client_id = body.get("client_id", "")
    redirect_uri = body.get("redirect_uri", "")
    client_secret = body.get("client_secret", "")

    cc = store_collection("oauth_codes")
    code_doc = cc.get(code)
    if code_doc == None:
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "Invalid authorization code",
        })

    if code_doc.get("used", False):
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "Authorization code already used",
        })

    want_cid = code_doc.get("client_id", "")
    want_uri = code_doc.get("redirect_uri", "")
    if client_id != want_cid or redirect_uri != want_uri:
        return respond(400, {
            "error": "invalid_client",
            "error_description": "Client or redirect_uri mismatch",
        })

    # Mark code as used.
    cc.delete(code)
    code_doc["used"] = True
    cc.insert(code_doc)

    user_id = code_doc.get("user_id", "")
    uc = store_collection("users")
    user = uc.get(user_id)
    if user == None:
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "User not found for this code",
        })

    return respond(200, _issue_tokens(user))

def _handle_refresh_grant(body):
    presented = body.get("refresh_token", "")
    if presented == "":
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "Missing refresh_token",
        })

    # Look up the user by refresh token (stored in KV).
    user_id = store_kv_get("cognito_refresh", presented)
    if user_id == "" or user_id == None:
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "Invalid refresh token",
        })

    uc = store_collection("users")
    user = uc.get(user_id)
    if user == None:
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "User not found",
        })

    return respond(200, _issue_tokens(user))

def _issue_tokens(user):
    access_seq = store_kv_incr("cognito", "access_seq")
    refresh_seq = store_kv_incr("cognito", "refresh_seq")

    access = _mint_jwt(user["sub"], user["username"], user.get("email", ""), "acc" + str(access_seq))
    id_token = _mint_jwt(user["sub"], user["username"], user.get("email", ""), "id" + str(access_seq))
    refresh = "mock-refresh-token-" + str(refresh_seq)

    # Store the access token → user binding.
    tc = store_collection("tokens")
    tc.insert({
        "id": access,
        "user_id": user["sub"],
        "token_type": "access",
    })

    # Store the refresh → user binding in KV.
    store_kv_set("cognito_refresh", refresh, user["sub"])

    return {
        "access_token": access,
        "id_token": id_token,
        "refresh_token": refresh,
        "token_type": "Bearer",
        "expires_in": 3600,
    }

def _user_info(user):
    attrs = user.get("attributes", {})
    return {
        "sub": user["sub"],
        "username": user["username"],
        "email": attrs.get("email", user.get("email", "")),
        "email_verified": attrs.get("email_verified", "true"),
        "given_name": attrs.get("given_name", ""),
        "family_name": attrs.get("family_name", ""),
        "preferred_username": user["username"],
    }

# _mint_user creates a new synthetic Cognito user with a UUID-like sub.
def _mint_user():
    seq = store_kv_incr("cognito", "user_seq")
    sub = "00000000-0000-0000-0000-" + _pad6(seq)
    username = "mock_user_" + str(seq)
    user = {
        "id": sub,
        "sub": sub,
        "username": username,
        "email": username + "@mock-cognito.com",
        "attributes": {
            "email": username + "@mock-cognito.com",
            "email_verified": "true",
            "given_name": "Mock",
            "family_name": "User" + str(seq),
        },
        "password": "MockPass123!",
        "enabled": True,
        "status": "CONFIRMED",
    }
    uc = store_collection("users")
    uc.insert(user)
    return user
