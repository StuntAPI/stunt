# Cognito Hosted UI OAuth handlers.
#
# GET  /oauth2/authorize  → 302 to redirect_uri?code=CODE&state=STATE
#                            (binds an EXISTING user: login_hint/username if
#                            given, else the seeded demo-user)
# POST /oauth2/token      → {access_token, id_token, refresh_token, ...}
#                            (authorization_code) or a fresh access/id pair
#                            WITHOUT a new refresh token (refresh_token grant
#                            — real Cognito reuses refresh tokens by default)
# GET  /oauth2/userInfo   → {sub, username, email, ...} (Bearer; the access
#                            token is a REAL RS256 JWT, verified on the way in)
# GET  /login             → 302 to /oauth2/authorize (hosted UI login)
# GET  /logout            → 302 to redirect_uri (hosted UI logout)
# GET  /{userPoolId}/.well-known/jwks.json → JWKS for the signing key

# on_authorize handles the authorization-code redirect.
# GET /oauth2/authorize?client_id=&redirect_uri=&response_type=code&scope=&state=
#   &[login_hint=<username>]
#
# The code is bound to an EXISTING user pool user: the login_hint (or
# username) query parameter when present, else the seeded demo-user. Unknown
# or unconfirmed users never mint codes — the redirect_uri gets an OAuth
# error redirect, the way real Cognito rejects the flow client-side.
def on_authorize(req):
    q = req["query"]
    redirect_uri = q.get("redirect_uri", "")
    state = q.get("state", "")
    client_id = q.get("client_id", "")
    response_type = q.get("response_type", "code")
    login_hint = q.get("login_hint", q.get("username", ""))

    if redirect_uri == "" or client_id == "":
        return respond(400, {
            "error": "invalid_request",
            "error_description": "Missing required parameter: redirect_uri or client_id",
        })

    if response_type != "code":
        return respond(302, headers={"Location": _oauth_error_redirect(
            redirect_uri, "unsupported_response_type",
            "Only+response_type=code+is+supported", state)})

    _seed_users()

    username = login_hint
    if username == "":
        username = _DEMO_USER

    uc = store_collection("users")
    user = uc.get(username)
    if user == None:
        return respond(302, headers={"Location": _oauth_error_redirect(
            redirect_uri, "invalid_request",
            "login_hint+user+does+not+exist", state)})
    if user.get("status", "") != "CONFIRMED":
        return respond(302, headers={"Location": _oauth_error_redirect(
            redirect_uri, "access_denied",
            "user+is+not+confirmed", state)})

    code_seq = store_kv_incr("cognito", "code_seq")
    code = "mock-auth-code-" + str(code_seq)

    cc = store_collection("oauth_codes")
    cc.insert({
        "id": code,
        "user_id": user["id"],
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "used": False,
    })

    location = redirect_uri + _redirect_sep(redirect_uri) + "code=" + code
    if state != "":
        location = location + "&state=" + state
    return respond(302, headers={"Location": location})

# on_token handles the token grants.
# POST /oauth2/token (form: grant_type=authorization_code, code, client_id,
# redirect_uri, [client_secret] | grant_type=refresh_token, refresh_token)
def on_token(req):
    # The body is form-encoded for /oauth2/token (the engine parses it into
    # req.body; _json_body falls back to it when raw_body is not JSON).
    body = _json_body(req)
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
# GET /oauth2/userInfo (Bearer). The access token is a real RS256 JWT —
# signature + exp + iss + token_use are verified cryptographically before
# the store lookup (the way real Cognito validates userInfo callers), and
# the token binding is gone after GlobalSignOut → 401.
def on_user_info(req):
    tok = _bearer(req)
    if tok == "":
        return respond(401, {
            "error": "invalid_token",
            "error_description": "Access token is missing",
        })

    user = _resolve_access(tok)
    if user == None:
        return respond(401, {
            "error": "invalid_token",
            "error_description": "Invalid access token",
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

# on_jwks serves the user-pool JWKS (public signing keys) at the real
# Cognito path /{userPoolId}/.well-known/jwks.json. The key is REAL:
# derived from the fixed synthetic RSA keypair whose private half signs
# the access/id tokens minted by _mint_jwt, so any standards-compliant
# JWT library can verify them.
def on_jwks(req):
    key = crypto.rsa_public_jwk(_JWT_PUBLIC_KEY)
    key["kid"] = _JWT_KID
    key["alg"] = "RS256"
    key["use"] = "sig"
    return respond(200, {"keys": [key]})

# --- internal ---

def _handle_auth_code_grant(body):
    code = body.get("code", "")
    client_id = body.get("client_id", "")
    redirect_uri = body.get("redirect_uri", "")

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

    # Mark code as used (single use, like real Cognito).
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

    return respond(200, _issue_tokens(user, client_id))

# _handle_refresh_grant implements grant_type=refresh_token with real
# Cognito default semantics: the refresh token is REUSABLE (rotation off),
# only a fresh RS256 access/id pair is minted and returned — no
# refresh_token field in the response. Revoked (GlobalSignOut) or expired
# refresh tokens are rejected with invalid_grant.
def _handle_refresh_grant(body):
    presented = body.get("refresh_token", "")
    user = _refresh_user(presented)
    if user == None:
        return respond(400, {
            "error": "invalid_grant",
            "error_description": "Invalid refresh token",
        })

    return respond(200, _rotate_access(user, body.get("client_id", "mock-client-id")))

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

# _redirect_sep returns the query separator for redirect_uri ("?" or "&").
def _redirect_sep(redirect_uri):
    if _contains(redirect_uri, "?"):
        return "&"
    return "?"

# _oauth_error_redirect builds an OAuth error redirect back to the client
# (descriptions are pre-joined with "+": the Location header must not carry
# raw spaces).
def _oauth_error_redirect(redirect_uri, error, desc, state):
    location = redirect_uri + _redirect_sep(redirect_uri) + "error=" + error + "&error_description=" + desc
    if state != "":
        location = location + "&state=" + state
    return location
