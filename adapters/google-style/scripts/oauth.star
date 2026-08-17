# OAuth2 handlers — Google-style authorization code + refresh-token flow.
#
# GET  /o/oauth2/auth   -> 302 redirect with code+state
# POST /o/oauth2/token  -> { access_token, expires_in, refresh_token, scope, token_type:"Bearer" }
#                          plus a real RS256 id_token when the granted scope
#                          includes openid (Google's OIDC behavior)
# GET  /oauth2/v3/certs -> JWKS for the id_token signing key (Google's real
#                          discovery path)
#
# The authorization-code flow mints a new user per code; the refresh_token
# grant issues a new access token for the same user (Google refresh tokens are
# NOT rotated — the same refresh token persists across refreshes).

# Shared helpers (_bearer, _user_for_token, _to_int, _contains) are preloaded
# from scripts/lib.star.

# --- adapter-specific helpers (not shared) ---

def _pad3(n):
    if n < 10:
        return "00" + str(n)
    if n < 100:
        return "0" + str(n)
    return str(n)

def _mint_user():
    seq = store_kv_incr("google", "user_seq")
    sub = "mock-user-" + _pad3(seq)
    return {
        "sub": sub,
        "name": "Mock User " + str(seq),
        "email": "user" + str(seq) + "@example.test",
        "picture": "https://mock-google.example/pic/" + sub + ".jpg",
    }

# _issue_tokens mints an access token + refresh token for a user and stores
# them. When the granted scope includes openid, a real RS256 id_token is
# minted too (verifiable against GET /oauth2/v3/certs). Returns the token
# response body.
def _issue_tokens(user, scope, client_id):
    access_seq = store_kv_incr("google", "access_seq")
    refresh_seq = store_kv_incr("google", "refresh_seq")
    access = "ya29.mock_access_" + str(access_seq)
    refresh = "1//mock_refresh_" + str(refresh_seq)

    tc = store_collection("tokens")
    u = {}
    for k in user:
        u[k] = user[k]
    u["id"] = access
    # Google access tokens expire after ~1h; store the expiry so
    # _user_for_token can reject stale tokens with 401.
    u["expires_at"] = clock.now_unix() + 3599
    tc.insert(u)

    rc = store_collection("refresh_tokens")
    rdoc = {}
    for k in user:
        rdoc[k] = user[k]
    rdoc["id"] = refresh
    rdoc["scope"] = scope
    rc.insert(rdoc)

    resp = {
        "access_token": access,
        "token_type": "Bearer",
        "expires_in": 3599,
        "refresh_token": refresh,
        "scope": scope,
    }
    if _contains(scope, "openid"):
        resp["id_token"] = _mint_id_token(user, client_id)
    return resp

# --- handlers ---

# on_authorize handles the authorization-code redirect.
def on_authorize(req):
    redirect_uri = req["query"].get("redirect_uri", "")
    state = req["query"].get("state", "")
    client_id = req["query"].get("client_id", "")

    if redirect_uri == "" or state == "" or client_id == "":
        return respond(400, {"error": "invalid_request", "error_description": "missing redirect_uri/state/client_id"})

    code_seq = store_kv_incr("google", "code_seq")
    code = "4/mock_code_" + str(code_seq)

    cc = store_collection("codes")
    cc.insert({
        "id": code,
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "scope": req["query"].get("scope", "openid email profile"),
    })

    sep = "?"
    if _contains(redirect_uri, "?"):
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
        client_id = body.get("client_id") or ""
        client_secret = body.get("client_secret") or ""

        if client_id == "" or client_secret == "":
            return respond(400, {"error": "invalid_client", "error_description": "missing client creds"})

        rc = store_collection("refresh_tokens")
        user = rc.get(presented)
        if user == None:
            return respond(400, {"error": "invalid_grant", "error_description": "invalid refresh token"})

        # Google does NOT rotate refresh tokens — keep the same one.
        u = {}
        for k in user:
            if k != "id":
                u[k] = user[k]
        scope = user.get("scope", "openid email profile")
        return respond(200, _issue_tokens_keep_refresh(u, presented, scope, client_id))

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

    cc.delete(code)

    want_cid = code_doc.get("client_id", "")
    want_uri = code_doc.get("redirect_uri", "")
    if client_id != want_cid or redirect_uri != want_uri or client_secret == "":
        return respond(400, {"error": "invalid_client", "error_description": "client mismatch"})

    scope = code_doc.get("scope", "openid email profile")
    return respond(200, _issue_tokens(_mint_user(), scope, client_id))

# on_certs serves the JWKS at Google's real discovery path
# (/oauth2/v3/certs). The key is REAL: derived from the fixed synthetic
# RSA keypair whose private half signs the id_tokens minted when the
# granted scope includes openid.
def on_certs(req):
    key = crypto.rsa_public_jwk(_JWT_PUBLIC_KEY)
    key["kid"] = _JWT_KID
    key["alg"] = "RS256"
    key["use"] = "sig"
    return respond(200, {"keys": [key]})

# _issue_tokens_keep_refresh mints a new access token for an existing user
# but keeps the same refresh_token (Google's behavior). An id_token is
# minted when the granted scope still includes openid.
def _issue_tokens_keep_refresh(user, refresh, scope, client_id):
    access_seq = store_kv_incr("google", "access_seq")
    access = "ya29.mock_access_" + str(access_seq)

    tc = store_collection("tokens")
    u = {}
    for k in user:
        u[k] = user[k]
    u["id"] = access
    u["expires_at"] = clock.now_unix() + 3599
    tc.insert(u)

    resp = {
        "access_token": access,
        "token_type": "Bearer",
        "expires_in": 3599,
        "refresh_token": refresh,
        "scope": scope,
    }
    if _contains(scope, "openid"):
        resp["id_token"] = _mint_id_token(user, client_id)
    return resp
