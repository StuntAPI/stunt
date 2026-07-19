# OAuth2 handlers — Meta Threads authorization code flow.
#
# GET  /oauth/authorize   -> 302 redirect with code+state
# POST /oauth/access_token -> { access_token, token_type, expires_in, user_id }
#
# The authorization code is single-use: exchanging it deletes it so a replay
# returns 400 invalid_grant.

# Shared helper (_bearer_present) is preloaded from scripts/lib.star.

# --- helpers ---

def _contains(s, substr):
    return s.find(substr) >= 0

# --- handlers ---

# on_authorize handles the authorization-code redirect.
def on_authorize(req):
    redirect_uri = req["query"].get("redirect_uri", "")
    state = req["query"].get("state", "")
    client_id = req["query"].get("client_id", "")

    if redirect_uri == "" or state == "" or client_id == "":
        return respond(400, {"error": "invalid_request", "message": "missing redirect_uri/state/client_id"})

    code_seq = store_kv_incr("threads", "code_seq")
    code = "mock_code_" + str(code_seq)

    cc = store_collection("codes")
    cc.insert({
        "id": code,
        "client_id": client_id,
        "redirect_uri": redirect_uri,
    })

    sep = "?"
    if _contains(redirect_uri, "?"):
        sep = "&"
    location = redirect_uri + sep + "code=" + code + "&state=" + state
    return respond(302, headers={"Location": location})

# on_access_token exchanges an authorization code for an access token.
def on_access_token(req):
    body = req["body"]
    if body == None:
        body = {}
    grant_type = body.get("grant_type", "")

    if grant_type != "authorization_code":
        return respond(400, {"error": "unsupported_grant_type"})

    code = body.get("code", "")
    client_id = body.get("client_id", "")
    client_secret = body.get("client_secret", "")
    redirect_uri = body.get("redirect_uri", "")

    cc = store_collection("codes")
    code_doc = cc.get(code)
    if code_doc == None:
        return respond(400, {"error": "invalid_grant", "error_description": "invalid/used code"})

    # Single-use: delete the code immediately.
    cc.delete(code)

    want_cid = code_doc.get("client_id", "")
    want_uri = code_doc.get("redirect_uri", "")
    if client_id != want_cid or redirect_uri != want_uri or client_secret == "":
        return respond(400, {"error": "invalid_client", "error_description": "client mismatch"})

    user = _mint_user()
    token = _mint_token(user)

    return respond(200, {
        "access_token": token,
        "token_type": "bearer",
        "expires_in": 5184000,
        "user_id": user["user_id"],
    })

def _mint_user():
    seq = store_kv_incr("threads", "user_seq")
    uid = "u_" + str(seq)
    return {
        "user_id": uid,
        "username": "mock_user_" + str(seq),
    }

def _mint_token(user):
    token_seq = store_kv_incr("threads", "token_seq")
    token = "mock_token_" + str(token_seq)
    tc = store_collection("tokens")
    tc.insert({
        "id": token,
        "user_id": user["user_id"],
        "username": user["username"],
    })
    return token
