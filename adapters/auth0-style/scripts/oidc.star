# Auth0-style OIDC / OAuth2 authentication handlers.
#
# GET  /.well-known/openid-configuration → discovery doc (issuer derived
#                                       from the request Host)
# GET  /.well-known/jwks.json            → JWKS (kid + rsa_public_jwk)
# GET  /authorize                        → 302 to redirect_uri?code=...&state=...
#                                       (invalid client/redirect → 302 with
#                                       error/error_description)
# POST /oauth/token                      → RS256 access_token (+id_token for
#                                       code grant), refresh_token,
#                                       expires_in, token_type
# GET  /userinfo                         → profile for a Bearer access token
#                                       (REAL RS256 verification on the way in)
# POST /oauth/revoke                     → idempotent 200 (RFC 7009)
# POST /dbconnections/signup             → email/password user + verified flag

# on_discovery serves the OIDC discovery document. Every URL (and the
# issuer) is derived from the request Host so a client configured against
# the adapter's own origin finds a self-consistent tenant.
def on_discovery(req):
    base = _base_url(req)
    return respond(200, {
        "issuer": _issuer(req),
        "authorization_endpoint": base + "/authorize",
        "token_endpoint": base + "/oauth/token",
        "userinfo_endpoint": base + "/userinfo",
        "jwks_uri": base + "/.well-known/jwks.json",
        "revocation_endpoint": base + "/oauth/revoke",
        "scopes_supported": ["openid", "profile", "email", "read:users", "write:users", "read:roles"],
        "response_types_supported": ["code"],
        "response_modes_supported": ["query"],
        "grant_types_supported": ["authorization_code", "refresh_token", "client_credentials"],
        "subject_types_supported": ["public"],
        "id_token_signing_alg_values_supported": ["RS256"],
        "token_endpoint_auth_methods_supported": ["client_secret_basic", "client_secret_post"],
        "claims_supported": ["sub", "iss", "aud", "iat", "exp", "azp", "scope", "jti",
            "email", "email_verified", "name", "nickname", "nonce"],
    })

# on_jwks serves the tenant JWKS. The key is REAL: derived from the fixed
# synthetic RSA keypair whose private half signs every access/id token
# minted by _mint_access_jwt/_mint_id_jwt, so any standards-compliant JWT
# library can verify them.
def on_jwks(req):
    key = crypto.rsa_public_jwk(_JWT_PUBLIC_KEY)
    key["kid"] = _JWT_KID
    key["alg"] = "RS256"
    key["use"] = "sig"
    return respond(200, {"keys": [key]})

# on_authorize handles the authorization-code redirect.
# GET /authorize?client_id=&redirect_uri=&response_type=code&scope=&state=
#   &[login_hint=<email or user_id>]
#
# The code is bound to an EXISTING user: the login_hint (or username) query
# parameter when it matches a user's email or user_id, else the seeded
# tenant's first user. client_id/redirect_uri are validated against the
# seeded application clients; failures redirect back with the OAuth2
# error/error_description pair.
def on_authorize(req):
    q = req["query"]
    redirect_uri = q.get("redirect_uri", "")
    client_id = q.get("client_id", "")
    response_type = q.get("response_type", "code")
    state = q.get("state", "")
    login_hint = q.get("login_hint", q.get("username", ""))

    if redirect_uri == "":
        return _oauth_err(400, "invalid_request",
            "Missing required parameter: redirect_uri")

    client = _client_by_id(client_id)
    if client == None:
        return respond(302, headers={"Location": _authz_error(redirect_uri,
            "invalid_request", "unknown+client_id", state)})
    if not _redirect_allowed(client, redirect_uri):
        return respond(302, headers={"Location": _authz_error(redirect_uri,
            "invalid_request", "redirect_uri+is+not+allowed+for+this+client", state)})
    if response_type != "code":
        return respond(302, headers={"Location": _authz_error(redirect_uri,
            "unsupported_response_type", "only+response_type=code+is+supported", state)})

    user = _authz_user(login_hint)
    if user == None:
        return respond(302, headers={"Location": _authz_error(redirect_uri,
            "access_denied", "login_hint+user+does+not+exist", state)})

    code = _new_auth_code(user["user_id"], client_id, redirect_uri)
    _touch_login(user)

    location = redirect_uri + _redirect_sep(redirect_uri) + "code=" + code
    if state != "":
        location = location + "&state=" + state
    return respond(302, headers={"Location": location})

# on_token handles the OAuth2 token grants (form or JSON body).
# POST /oauth/token grant_type=authorization_code | refresh_token |
# client_credentials, with client credentials in the body or HTTP Basic.
def on_token(req):
    body = _json_body(req)
    grant_type = body.get("grant_type", "")

    client, client_id, auth_err = _client_auth(req, body)
    if auth_err != None:
        return auth_err

    if grant_type == "authorization_code":
        return _token_auth_code_grant(body, req, client_id)
    if grant_type == "refresh_token":
        return _token_refresh_grant(body, req, client_id)
    if grant_type == "client_credentials":
        return _token_client_credentials_grant(req, body, client, client_id)
    return _oauth_err(400, "unsupported_grant_type",
        "Grant type '" + grant_type + "' not supported")

# on_userinfo returns the profile for a Bearer access token. The token is a
# real RS256 JWT — signature + exp + iss are verified cryptographically, and
# the sub claim must resolve to a tenant user (machine-to-machine tokens
# have no userinfo).
def on_userinfo(req):
    tok = _bearer(req)
    if tok == "":
        return _oauth_err(401, "invalid_token", "Access token is missing")

    claims, _reason = _verify_access(req, tok)
    if claims == None:
        return _oauth_err(401, "invalid_token", "Invalid access token")

    user = store_collection("users").get(claims.get("sub", ""))
    if user == None:
        return _oauth_err(401, "invalid_token",
            "Token subject is not a user (machine-to-machine tokens cannot call userinfo)")

    return respond(200, {
        "sub": user["user_id"],
        "name": user.get("name", ""),
        "nickname": user.get("nickname", ""),
        "email": _user_email(user),
        "email_verified": bool(user.get("email_verified", False)),
        "updated_at": user.get("updated_at", ""),
    })

# on_revoke revokes a refresh token. RFC 7009 semantics: unknown tokens and
# repeat revocations are still a 200 (idempotent); only client
# authentication can fail.
def on_revoke(req):
    body = _json_body(req)
    _client, _client_id, auth_err = _client_auth(req, body)
    if auth_err != None:
        return auth_err

    token = body.get("token", "")
    if token != "":
        store_collection("refresh_tokens").delete(token)
    return respond(200, "")

# on_signup is the dbconnections signup (email + password user creation in
# the Username-Password-Authentication connection). New users start
# unverified.
def on_signup(req):
    body = _json_body(req)
    email = body.get("email", "")
    password = body.get("password", "")
    client_id = body.get("client_id", "")
    connection = body.get("connection", _DEFAULT_CONNECTION)

    if _client_by_id(client_id) == None:
        return _oauth_err(403, "unauthorized_client", "Invalid client_id")
    if email == "" or password == "":
        return _oauth_err(400, "invalid_request",
            "Missing required parameter: email or password")
    if email.find("@") < 0:
        return _oauth_err(400, "invalid_request", "The email provided is invalid")
    if len(password) < 8:
        return _oauth_err(400, "invalid_password",
            "Password must have at least 8 characters")
    if _find_user_by_email(email) != None:
        return _oauth_err(400, "user_exists", "The user already exists")

    nickname = body.get("nickname", "")
    if nickname == "":
        nickname = email[:email.find("@")]
    name = body.get("name", "")
    if name == "":
        name = nickname

    user = _create_user(email, name, nickname, password, False, connection)
    return respond(200, {
        "_id": user["user_id"],
        "email_verified": False,
        "email": _user_email(user),
    })

# --- internal ---

# _authz_user resolves who the authorization code is bound to: login_hint
# matching a user's email (case-insensitive) or user_id, else the tenant's
# first user by user_id (deterministic default subject).
def _authz_user(login_hint):
    users = store_collection("users").list()
    if login_hint != "":
        for user in users:
            if user.get("user_id", "") == login_hint:
                return user
        for user in users:
            if _user_email(user).lower() == login_hint.lower():
                return user
        return None
    ordered = query_select(users, None, "user_id", "asc", None, None, None)
    if len(ordered) == 0:
        return None
    return ordered[0]

# _redirect_sep returns the query separator for redirect_uri ("?" or "&").
def _redirect_sep(redirect_uri):
    if _contains(redirect_uri, "?"):
        return "&"
    return "?"

# _authz_error builds the OAuth2 error redirect (descriptions are
# pre-joined with "+": the Location header must not carry raw spaces).
def _authz_error(redirect_uri, error, desc, state):
    location = redirect_uri + _redirect_sep(redirect_uri) + "error=" + error
    location = location + "&error_description=" + desc
    if state != "":
        location = location + "&state=" + state
    return location

# _token_auth_code_grant exchanges a single-use code. The code must be
# unexpired and re-present the same client_id + redirect_uri it was issued
# with (OAuth2 code-interchange binding).
def _token_auth_code_grant(body, req, client_id):
    code = body.get("code", "")
    redirect_uri = body.get("redirect_uri", "")

    cc = store_collection("oauth_codes")
    doc = cc.get(code)
    if doc == None:
        return _oauth_err(400, "invalid_grant", "Invalid authorization code")

    exp = _to_int(doc.get("expires_at", ""))
    if exp > 0 and clock.now_unix() >= exp:
        cc.delete(code)
        return _oauth_err(400, "invalid_grant", "Authorization code expired")

    if doc.get("client_id", "") != client_id:
        return _oauth_err(400, "invalid_grant",
            "Code was not issued to this client")
    if doc.get("redirect_uri", "") != redirect_uri:
        return _oauth_err(400, "invalid_grant",
            "redirect_uri did not match the authorization request")

    cc.delete(code)  # single use, like the real token endpoint

    user = store_collection("users").get(doc.get("user_id", ""))
    if user == None:
        return _oauth_err(400, "invalid_grant", "User not found for this code")

    _touch_login(user)
    return respond(200, _issue_user_tokens(req, user, client_id))

# _token_refresh_grant implements grant_type=refresh_token with Auth0's
# default semantics: rotation is OFF, so the presented refresh token stays
# valid and only a fresh access/id pair is returned. Revoked or expired
# refresh tokens answer invalid_grant.
def _token_refresh_grant(body, req, client_id):
    presented = body.get("refresh_token", "")

    doc = store_collection("refresh_tokens").get(presented)
    if doc == None:
        return _oauth_err(400, "invalid_grant", "Unknown or invalid refresh token")

    exp = _to_int(doc.get("expires_at", ""))
    if exp > 0 and clock.now_unix() >= exp:
        return _oauth_err(400, "invalid_grant", "Refresh token expired")

    if doc.get("client_id", "") != client_id:
        return _oauth_err(400, "invalid_grant",
            "Refresh token was not issued to this client")

    user = store_collection("users").get(doc.get("user_id", ""))
    if user == None:
        return _oauth_err(400, "invalid_grant", "User not found for this refresh token")

    return respond(200, _rotate_user_tokens(req, user, client_id))

# _token_client_credentials_grant mints the machine-to-machine token: sub is
# the client, aud is the API identifier (the body audience when given), and
# no id_token or refresh_token is issued. Only clients with the
# client_credentials grant may use it.
def _token_client_credentials_grant(req, body, client, client_id):
    grants = client.get("grant_types", [])
    if "client_credentials" not in grants:
        return _oauth_err(400, "unauthorized_client",
            "Client is not authorized to use the client_credentials grant")
    audience = body.get("audience", "")
    if audience == "":
        audience = _base_url(req) + "/api/v2/"
    access = _mint_access_jwt(req, client_id + "@clients", client_id, audience,
        "read:users write:users read:roles")
    return respond(200, {
        "access_token": access,
        "token_type": "Bearer",
        "expires_in": _ACCESS_TTL,
    })
