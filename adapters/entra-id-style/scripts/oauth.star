# OAuth2 handlers — Microsoft identity platform v2.0 authorization code flow.
#
# Microsoft Entra ID (Azure AD) uses the /common endpoint for multi-tenant
# apps. The v2.0 endpoint supports OIDC scopes (openid, profile, email,
# offline_access, User.Read, etc.) and returns tokens with ext_expires_in.
#
# GET  /common/oauth2/v2.0/authorize  -> 302 redirect with code+state
# POST /common/oauth2/v2.0/token      -> { token_type, expires_in, ext_expires_in,
#                                          access_token, refresh_token, scope }
#
# Supported grant types: authorization_code, refresh_token.
# Admin consent is modeled via prompt=admin_consent query param.

# Shared helpers (_bearer, _user_for_token, _mint_jwt, _pad3, _contains)
# are preloaded from scripts/lib.star.

# --- adapter-specific helpers ---

# _mint_user creates a new synthetic Entra ID user.
def _mint_user():
    seq = store_kv_incr("entra", "user_seq")
    uid = "00000000-0000-0000-0000-" + _pad6(seq)
    return {
        "id": uid,
        "displayName": "Mock User " + str(seq),
        "givenName": "Mock",
        "surname": "User" + str(seq),
        "mail": "mockuser" + str(seq) + "@mock-tenant.onmicrosoft.com",
        "userPrincipalName": "mockuser" + str(seq) + "@mock-tenant.onmicrosoft.com",
        "jobTitle": "Software Engineer",
        "officeLocation": "Building A",
        "accountEnabled": True,
    }

# _issue_tokens mints an access token + refresh token for a user and stores
# them. Returns the token response body.
def _issue_tokens(user, scope):
    access_seq = store_kv_incr("entra", "access_seq")
    refresh_seq = store_kv_incr("entra", "refresh_seq")

    # Preserve the original user UUID in a separate field; "id" is
    # overwritten below with the token value (used as the collection key).
    user_id = user["id"]

    token_doc = {}
    for k in user:
        token_doc[k] = user[k]
    token_doc["user_id"] = user_id

    # Mint a JWT-shaped access token. Include the access_seq in the payload
    # so each token is unique even for the same user (refresh mints a new one).
    access = _mint_jwt(user_id, scope, user["displayName"], str(access_seq))
    token_doc["id"] = access
    token_doc["scope"] = scope

    tc = store_collection("tokens")
    tc.insert(token_doc)

    # Store refresh token (only if offline_access scope was requested).
    refresh = ""
    if _contains(scope, "offline_access"):
        refresh = "OQAABgIA..." + str(refresh_seq) + "_mock_refresh"
        rc = store_collection("refresh_tokens")
        rdoc = {}
        for k in user:
            rdoc[k] = user[k]
        rdoc["user_id"] = user_id
        rdoc["id"] = refresh
        rdoc["scope"] = scope
        rc.insert(rdoc)

    return {
        "token_type": "Bearer",
        "scope": scope,
        "expires_in": 3599,
        "ext_expires_in": 3599,
        "access_token": access,
        "refresh_token": refresh,
    }

# --- handlers ---

# on_authorize handles the authorization-code redirect.
# Recognizes prompt=admin_consent to model admin consent (returns a
# session_state that indicates admin consent was granted).
def on_authorize(req):
    redirect_uri = req["query"].get("redirect_uri", "")
    state = req["query"].get("state", "")
    client_id = req["query"].get("client_id", "")

    if redirect_uri == "" or state == "" or client_id == "":
        return respond(400, {"error": "invalid_request", "error_description": "missing redirect_uri/state/client_id"})

    code_seq = store_kv_incr("entra", "code_seq")
    code = "0.AXoA." + _pad6(code_seq) + "-mock-code"

    scope = req["query"].get("scope", "openid profile User.Read")
    prompt = req["query"].get("prompt", "")

    cc = store_collection("codes")
    cc.insert({
        "id": code,
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "scope": scope,
        "admin_consent": prompt == "admin_consent",
    })

    sep = "?"
    if _contains(redirect_uri, "?"):
        sep = "&"
    location = redirect_uri + sep + "code=" + code + "&state=" + state + "&session_state=mock-session-state"
    return respond(302, headers={"Location": location})

# on_token handles both authorization_code and refresh_token grants.
def on_token(req):
    body = req["body"]
    if body == None:
        body = {}
    grant_type = body.get("grant_type", "")

    # --- refresh_token grant ---
    if grant_type == "refresh_token":
        presented = body.get("refresh_token", "")
        client_id = body.get("client_id", "")
        client_secret = body.get("client_secret", "")

        if client_id == "":
            return respond(400, {"error": "invalid_client", "error_description": "client_id is required"})

        rc = store_collection("refresh_tokens")
        user = rc.get(presented)
        if user == None:
            return respond(400, {"error": "invalid_grant", "error_description": "invalid refresh token"})

        scope = user.get("scope", "openid profile User.Read")
        # Reconstruct the user profile from the refresh-token doc.
        # user_id holds the original user UUID ("id" was overwritten with
        # the refresh-token value during insert).
        u = {}
        for k in user:
            if k != "id":
                u[k] = user[k]
        u["id"] = user.get("user_id", user["id"])
        return respond(200, _issue_tokens(u, scope))

    # --- authorization_code grant ---
    if grant_type != "authorization_code":
        return respond(400, {"error": "unsupported_grant_type", "error_description": "unsupported grant_type"})

    code = body.get("code", "")
    client_id = body.get("client_id", "")
    client_secret = body.get("client_secret", "")
    redirect_uri = body.get("redirect_uri", "")

    cc = store_collection("codes")
    code_doc = cc.get(code)
    if code_doc == None:
        return respond(400, {"error": "invalid_grant", "error_description": "authorization code not found or expired"})

    cc.delete(code)

    want_cid = code_doc.get("client_id", "")
    want_uri = code_doc.get("redirect_uri", "")
    if client_id != want_cid or redirect_uri != want_uri or client_secret == "":
        return respond(400, {"error": "invalid_client", "error_description": "client/redirect_uri mismatch"})

    scope = code_doc.get("scope", "openid profile User.Read")
    user = _mint_user()

    # Persist the user so they appear in listings.
    uc = store_collection("users")
    u = {}
    for k in user:
        u[k] = user[k]
    u["id"] = user["id"]
    uc.insert(u)

    return respond(200, _issue_tokens(user, scope))
