# OAuth2 handlers — Discord v10 authorization-code + refresh-token flow.
#
# GET  /oauth2/authorize   -> 302 redirect with code+state
# POST /oauth2/token       -> { access_token, token_type, expires_in,
#                               refresh_token, scope, guild }
# GET  /oauth2/@me         -> { application: {...}, user: {...} }
#
# Authorization codes are single-use. The refresh_token grant issues a new
# access token for the same user but does NOT return a new refresh token
# (matching Discord's behaviour — the refresh token is reusable).

# Shared helpers (_bearer, _oauth_user, _snowflake, _seed, _bot_user,
# _token, _require_bot) are preloaded from scripts/lib.star.

# --- adapter-specific helpers (not shared) ---

def _mint_user():
    seq = store_kv_incr("discord", "user_seq")
    return {
        "user_id": _snowflake(seq),
        "username": "mock_user_" + str(seq),
        "global_name": "Mock User " + str(seq),
        "discriminator": "0001",
        "avatar": None,
    }

def _issue_tokens(user):
    access_seq = store_kv_incr("discord", "access_seq")
    refresh_seq = store_kv_incr("discord", "refresh_seq")
    access = "mock_access_" + str(access_seq)
    refresh = "mock_refresh_" + str(refresh_seq)

    tc = store_collection("access_tokens")
    doc = {}
    for k in user:
        doc[k] = user[k]
    doc["id"] = access
    tc.insert(doc)

    rc = store_collection("refresh_tokens")
    rdoc = {}
    for k in user:
        rdoc[k] = user[k]
    rdoc["id"] = refresh
    rc.insert(rdoc)

    _seed()

    guild_id = store_kv_get("discord", "guild_id")

    return {
        "access_token": access,
        "token_type": "Bearer",
        "expires_in": 604800,
        "refresh_token": refresh,
        "scope": "identify bot applications.commands",
        "guild": {
            "id": guild_id,
            "name": "Mock Guild",
            "icon": None,
            "owner": False,
            "permissions": "68672",
        },
    }

# --- handlers ---

# on_authorize handles the authorization-code redirect.
def on_authorize(req):
    redirect_uri = req["query"].get("redirect_uri", "")
    state = req["query"].get("state", "")
    client_id = req["query"].get("client_id", "")

    if redirect_uri == "" or state == "" or client_id == "":
        return respond(400, {
            "error": "invalid_request",
            "error_description": "missing redirect_uri/state/client_id",
        })

    code_seq = store_kv_incr("discord", "code_seq")
    code = "mock_code_" + str(code_seq)

    cc = store_collection("oauth_codes")
    cc.insert({
        "id": code,
        "client_id": client_id,
        "redirect_uri": redirect_uri,
    })

    sep = "?"
    if "?" in redirect_uri:
        sep = "&"
    location = redirect_uri + sep + "code=" + code + "&state=" + state
    return respond(302, headers={"Location": location})

# on_token handles both authorization_code and refresh_token grants.
def on_token(req):
    body = req["body"]
    if body == None:
        body = {}
    grant_type = body.get("grant_type", "")

    if grant_type == "refresh_token":
        presented = body.get("refresh_token", "")
        client_id = body.get("client_id", "")
        client_secret = body.get("client_secret", "")

        if client_id == "" or client_secret == "":
            return respond(400, {
                "error": "invalid_client",
                "error_description": "client credentials required",
            })

        rc = store_collection("refresh_tokens")
        user = rc.get(presented)
        if user == None:
            return respond(400, {
                "error": "invalid_grant",
                "error_description": "invalid refresh token",
            })

        # Discord does NOT rotate refresh tokens — issue a new access token
        # for the same user without returning a new refresh token.
        access_seq = store_kv_incr("discord", "access_seq")
        access = "mock_access_" + str(access_seq)

        tc = store_collection("access_tokens")
        doc = {}
        for k in user:
            doc[k] = user[k]
        doc["id"] = access
        tc.insert(doc)

        _seed()
        guild_id = store_kv_get("discord", "guild_id")

        return respond(200, {
            "access_token": access,
            "token_type": "Bearer",
            "expires_in": 604800,
            "scope": "identify bot applications.commands",
            "guild": {
                "id": guild_id,
                "name": "Mock Guild",
                "icon": None,
                "owner": False,
                "permissions": "68672",
            },
        })

    if grant_type != "authorization_code":
        return respond(400, {"error": "unsupported_grant_type"})

    code = body.get("code", "")
    client_id = body.get("client_id", "")
    client_secret = body.get("client_secret", "")
    redirect_uri = body.get("redirect_uri", "")

    cc = store_collection("oauth_codes")
    code_doc = cc.get(code)
    if code_doc == None:
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "invalid or used code",
        })

    cc.delete(code)

    want_cid = code_doc.get("client_id", "")
    want_uri = code_doc.get("redirect_uri", "")
    if client_id != want_cid or redirect_uri != want_uri or client_secret == "":
        return respond(400, {
            "error": "invalid_client",
            "error_description": "client mismatch",
        })

    return respond(200, _issue_tokens(_mint_user()))

# on_oauth_me returns the current application + user for a Bearer token.
def on_oauth_me(req):
    user = _oauth_user(req)
    if user == None:
        return respond(401, {"code": 0, "message": "401: Unauthorized"})

    _seed()
    app_id = "9000000000000000003"

    return respond(200, {
        "application": {
            "id": app_id,
            "name": "Mock App",
            "icon": None,
            "description": "",
            "bot_public": True,
            "bot_require_code_grant": False,
            "verify_key": "mock_verify_key_000000000000000000000000000000000000",
        },
        "user": {
            "id": user["user_id"],
            "username": user["username"],
            "global_name": user["global_name"],
            "discriminator": user["discriminator"],
            "avatar": None,
        },
    })
