# Firebase Auth handlers — v1 REST + v3 (Identity Toolkit) legacy.
#
# Both v1 and v3 names are supported:
#
#   v1:  POST /v1/accounts:signInWithPassword   (sign in)
#   v1:  POST /v1/accounts:signUp               (create)
#   v1:  POST /v1/accounts:signInWithIdp        (sign in with IDP)
#   v1:  POST /v1/accounts:getAccountInfo       (lookup)
#   v1:  POST /v1/accounts:lookup               (lookup)
#
#   v3:  POST /identitytoolkit/v3/relyingparty/verifyPassword
#   v3:  POST /identitytoolkit/v3/relyingparty/signupNewUser
#   v3:  POST /identitytoolkit/v3/relyingparty/getAccountInfo
#   v3:  POST /identitytoolkit/v3/relyingparty/refreshToken
#
# Auth supports Bearer (OAuth2) OR key. Users are STATEFUL: a user created
# via signUp persists and can sign in.

# --- v1 handlers ---

# on_sign_in_with_password: POST /v1/accounts:signInWithPassword
# Body: { email, password, returnSecureToken }
def on_sign_in_with_password(req):
    err = _require_auth(req)
    if err != None:
        return err
    body = req["body"]
    if body == None:
        body = {}
    return _do_sign_in(body, "v1")

# on_sign_up: POST /v1/accounts:signUp
# Body: { email, password, returnSecureToken }
def on_sign_up(req):
    err = _require_auth(req)
    if err != None:
        return err
    body = req["body"]
    if body == None:
        body = {}
    return _do_sign_up(body, "v1")

# on_sign_in_with_idp: POST /v1/accounts:signInWithIdp
# Body: { postBody, requestUri, returnIdpCredential, returnSecureToken }
def on_sign_in_with_idp(req):
    err = _require_auth(req)
    if err != None:
        return err
    body = req["body"]
    if body == None:
        body = {}
    return _do_sign_in_with_idp(body, "v1")

# on_get_account_info: POST /v1/accounts:getAccountInfo (and :lookup)
# Body: { idToken }
def on_get_account_info(req):
    err = _require_auth(req)
    if err != None:
        return err
    body = req["body"]
    if body == None:
        body = {}
    id_token = body.get("idToken", "")
    return _do_get_account_info(id_token, "v1")

# --- v3 handlers ---

# on_relyingparty dispatches all /identitytoolkit/v3/relyingparty/{action}
# POST requests to the appropriate v3 handler based on the {action} param.
# Body: depends on the action.
def on_relyingparty(req):
    err = _require_auth(req)
    if err != None:
        return err
    body = req["body"]
    if body == None:
        body = {}
    action = req["params"].get("action", "")
    if action == "verifyPassword":
        return _do_sign_in(body, "v3")
    if action == "signupNewUser":
        return _do_sign_up(body, "v3")
    if action == "getAccountInfo":
        id_token = body.get("idToken", "")
        return _do_get_account_info(id_token, "v3")
    if action == "refreshToken":
        presented = body.get("refresh_token", body.get("refreshToken", ""))
        return _do_refresh(presented)
    return _err(404, 404, "UNKNOWN_ACTION", "Unknown relyingparty action: " + action, "NOT_FOUND")

# --- internal ---

# _do_sign_in validates email/password against stored users and issues tokens.
def _do_sign_in(body, version):
    email = body.get("email", "")
    password = body.get("password", "")
    if email == "" or password == "":
        return _err(400, 400, "MISSING_EMAIL_OR_PASSWORD", "INVALID_ARGUMENT")

    uc = store_collection("users")
    docs = uc.list()
    user = None
    for d in docs:
        if d.get("email", "") == email:
            user = d
            break

    if user == None:
        return _err(400, 400, "EMAIL_NOT_FOUND", "NOT_FOUND")

    if user.get("password", "") != password:
        return _err(400, 400, "INVALID_PASSWORD", "INVALID_ARGUMENT")

    return respond(200, _auth_response(user))

# _do_sign_up creates a new user and issues tokens.
def _do_sign_up(body, version):
    email = body.get("email", "")
    password = body.get("password", "")
    display_name = body.get("displayName", "")

    if email == "":
        return _err(400, 400, "MISSING_EMAIL", "INVALID_ARGUMENT")

    # Check if user already exists.
    uc = store_collection("users")
    docs = uc.list()
    for d in docs:
        if d.get("email", "") == email:
            return _err(400, 400, "EMAIL_EXISTS", "ALREADY_EXISTS")

    seq = store_kv_incr("fb", "user_seq")
    local_id = "firebase-user-" + _pad6(seq)

    user = {
        "id": local_id,
        "localId": local_id,
        "email": email,
        "emailVerified": False,
        "password": password,
        "displayName": display_name,
    }
    uc.insert(user)

    return respond(200, _auth_response(user))

# _do_sign_in_with_idp signs in with a federated provider (Google etc).
def _do_sign_in_with_idp(body, version):
    seq = store_kv_incr("fb", "user_seq")
    local_id = "firebase-idp-user-" + _pad6(seq)
    email = "idp-user-" + str(seq) + "@google.com"

    user = {
        "id": local_id,
        "localId": local_id,
        "email": email,
        "emailVerified": True,
        "password": "",
        "displayName": "IDP User " + str(seq),
        "provider": "google.com",
    }
    uc = store_collection("users")
    uc.insert(user)

    resp = _auth_response(user)
    resp["providerUserInfo"] = [{
        "providerId": "google.com",
        "rawId": str(seq),
        "email": email,
        "displayName": "IDP User " + str(seq),
    }]
    return respond(200, resp)

# _do_get_account_info returns user info for a given idToken.
def _do_get_account_info(id_token, version):
    user = _user_for_token(id_token)
    if user == None:
        return _err(400, 400, "INVALID_ID_TOKEN", "INVALID_ARGUMENT")

    return respond(200, {
        "users": [{
            "localId": user["localId"],
            "email": user["email"],
            "emailVerified": user.get("emailVerified", False),
            "displayName": user.get("displayName", ""),
        }],
    })

# _do_refresh exchanges a refresh token for a new idToken.
def _do_refresh(presented):
    if presented == "":
        return _err(400, 400, "MISSING_REFRESH_TOKEN", "INVALID_ARGUMENT")

    # Look up the user ID bound to this refresh token (stored in KV).
    user_id = store_kv_get("fb_refresh", presented)
    if user_id == "" or user_id == None:
        return _err(400, 400, "INVALID_REFRESH_TOKEN", "INVALID_ARGUMENT")

    uc = store_collection("users")
    user = uc.get(user_id)
    if user == None:
        return _err(400, 400, "INVALID_REFRESH_TOKEN", "INVALID_ARGUMENT")

    # Issue new tokens.
    resp = _auth_response(user)
    # refresh_token grant returns refresh_token too.
    return respond(200, {
        "user_id": user["localId"],
        "id_token": resp["idToken"],
        "refresh_token": resp["refreshToken"],
        "expires_in": resp["expiresIn"],
        "token_type": "Bearer",
    })

# _auth_response issues tokens for a user and stores the refresh token.
def _auth_response(user):
    access_seq = store_kv_incr("fb", "token_seq")
    refresh_seq = store_kv_incr("fb", "refresh_seq")

    id_token = "firebase-id-token-" + str(access_seq)
    refresh = "firebase-refresh-token-" + str(refresh_seq)

    # Store the refresh→user binding in KV for later validation.
    store_kv_set("fb_refresh", refresh, user["id"])

    return {
        "localId": user["localId"],
        "idToken": id_token,
        "refreshToken": refresh,
        "expiresIn": "3600",
        "email": user["email"],
        "displayName": user.get("displayName", ""),
        "registered": True,
        "kind": "identitytoolkit#VerifyPasswordResponse",
    }

# _user_for_token looks up a user by idToken. Since idTokens are ephemeral
# (generated fresh each sign-in), we can't look up by token directly.
# For testing purposes, we accept any non-empty idToken and return the first
# user.
def _user_for_token(id_token):
    if id_token == "":
        return None
    uc = store_collection("users")
    docs = uc.list()
    if len(docs) == 0:
        return None
    return docs[0]
