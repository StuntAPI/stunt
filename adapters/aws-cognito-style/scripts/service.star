# Cognito Service API handlers (user pool + identity pool).
#
# POST / with X-Amz-Target header dispatches to the appropriate operation.
#
# Supported X-Amz-Target values:
#   AWSCognitoIdentityProviderService.SignUp
#   AWSCognitoIdentityProviderService.ConfirmSignUp
#   AWSCognitoIdentityProviderService.InitiateAuth
#   AWSCognitoIdentityProviderService.AdminInitiateAuth
#   AWSCognitoIdentityProviderService.RespondToAuthChallenge
#   AWSCognitoIdentityProviderService.AdminRespondToAuthChallenge
#   AWSCognitoIdentityProviderService.ForgotPassword
#   AWSCognitoIdentityProviderService.ConfirmForgotPassword
#   AWSCognitoIdentityProviderService.GlobalSignOut
#   AWSCognitoIdentityProviderService.AdminUserGlobalSignOut
#   AWSCognitoIdentityProviderService.GetUser
#   AWSCognitoIdentityProviderService.ListUsers
#   AWSCognitoIdentityProviderService.AdminCreateUser
#   AWSCognitoIdentityService.GetId            (identity pool)
#   AWSCognitoIdentityService.GetCredentialsForIdentity
#
# Auth: SigV4 structural check (or X-Amz-Target without auth for some
# operations like InitiateAuth, which uses USER_PASSWORD_AUTH flow).

# on_service_api dispatches based on the X-Amz-Target header.
# POST / (X-Amz-Target: <service>.<operation>)
def on_service_api(req):
    # Structural SigV4 validation when an Authorization header is present.
    sig_err = _sigv4_check(req)
    if sig_err != None:
        return sig_err

    target = req["headers"].get("X-Amz-Target", "")

    if target == "AWSCognitoIdentityProviderService.SignUp":
        return _do_signup(req)
    if target == "AWSCognitoIdentityProviderService.ConfirmSignUp":
        return _do_confirm_signup(req)
    if target == "AWSCognitoIdentityProviderService.InitiateAuth":
        return _do_initiate_auth(req)
    if target == "AWSCognitoIdentityProviderService.AdminInitiateAuth":
        return _do_admin_initiate_auth(req)
    if target == "AWSCognitoIdentityProviderService.RespondToAuthChallenge":
        return _do_respond_to_challenge(req)
    if target == "AWSCognitoIdentityProviderService.AdminRespondToAuthChallenge":
        return _do_respond_to_challenge(req)
    if target == "AWSCognitoIdentityProviderService.ForgotPassword":
        return _do_forgot_password(req)
    if target == "AWSCognitoIdentityProviderService.ConfirmForgotPassword":
        return _do_confirm_forgot_password(req)
    if target == "AWSCognitoIdentityProviderService.GlobalSignOut":
        return _do_global_sign_out(req)
    if target == "AWSCognitoIdentityProviderService.AdminUserGlobalSignOut":
        return _do_admin_user_global_sign_out(req)
    if target == "AWSCognitoIdentityProviderService.GetUser":
        return _do_get_user(req)
    if target == "AWSCognitoIdentityProviderService.ListUsers":
        return _do_list_users(req)
    if target == "AWSCognitoIdentityProviderService.AdminCreateUser":
        return _do_admin_create_user(req)
    if target == "AWSCognitoIdentityService.GetId":
        return _do_get_id(req)
    if target == "AWSCognitoIdentityService.GetCredentialsForIdentity":
        return _do_get_credentials(req)

    return _cognito_err("NotImplementedException",
        "Operation not supported: " + target)

# --- User pool operations ---

# SignUp: create a new user in the user pool.
# {ClientId, Username, Password, UserAttributes: [{Name, Value}]}
def _do_signup(req):
    body = _json_body(req)
    username = body.get("Username", "")
    password = body.get("Password", "")

    if username == "" or password == "":
        return _cognito_err("InvalidParameterException",
            "Username and Password are required")

    # Check if user already exists.
    uc = store_collection("users")
    existing = uc.get(username)
    if existing != None:
        return _cognito_err("UsernameExistsException",
            "User already exists")

    seq = store_kv_incr("cognito", "user_seq")
    sub = _SUB_PREFIX + _pad6(seq)

    attrs = _parse_user_attributes(body.get("UserAttributes", []))

    if "email" not in attrs:
        attrs["email"] = username + "@mock-cognito.com"
    if "email_verified" not in attrs:
        attrs["email_verified"] = "true"

    user = {
        "id": username,
        "sub": sub,
        "username": username,
        "email": attrs.get("email", ""),
        "attributes": attrs,
        "password": password,
        "enabled": True,
        "status": "UNCONFIRMED",
    }
    uc.insert(user)

    return respond(200, {
        "UserConfirmed": False,
        "UserSub": sub,
        "CodeDeliveryDetails": {
            "AttributeName": "email",
            "DeliveryMedium": "EMAIL",
            "Destination": attrs.get("email", ""),
        },
    })

# ConfirmSignUp: confirm a user's registration with the deterministic code
# (see _gen_code — the twilio-verify convention shared with
# ConfirmForgotPassword). Wrong code → CodeMismatchException; already
# confirmed → NotAuthorizedException, like real Cognito.
# {ClientId, Username, ConfirmationCode}
def _do_confirm_signup(req):
    body = _json_body(req)
    username = body.get("Username", "")
    if username == "":
        return _cognito_err("InvalidParameterException",
            "Username is required")
    code = body.get("ConfirmationCode", "")

    uc = store_collection("users")
    user = uc.get(username)
    if user == None:
        return _cognito_err("UserNotFoundException",
            "User does not exist.")

    if user.get("status", "") == "CONFIRMED":
        return _cognito_err("NotAuthorizedException",
            "User cannot be confirmed. Current status is CONFIRMED")

    if code != _gen_code(username):
        return _cognito_err("CodeMismatchException",
            "Invalid code provided, please request a code again.")

    user["status"] = "CONFIRMED"
    uc.update(user["id"], user)

    return respond(200, {})

# InitiateAuth: start a user auth flow.
# Supported AuthFlows: USER_PASSWORD_AUTH (password login),
# REFRESH_TOKEN_AUTH / REFRESH_TOKEN_REFRESH (rotate access+id, refresh
# token stays valid — real Cognito default). FORCE_CHANGE_PASSWORD users
# signing in with their temporary password get a NEW_PASSWORD_REQUIRED
# challenge (plus a 3-minute Session) instead of tokens.
# {AuthFlow, AuthParameters: {USERNAME, PASSWORD | REFRESH_TOKEN}, ClientId}
def _do_initiate_auth(req):
    _seed_users()
    return _initiate_auth_flow(_json_body(req), False)

# AdminInitiateAuth: admin variant. Password flows are ADMIN_USER_PASSWORD_AUTH
# / ADMIN_NO_SRP_AUTH; refresh flows are accepted like InitiateAuth.
def _do_admin_initiate_auth(req):
    _seed_users()
    return _initiate_auth_flow(_json_body(req), True)

# RespondToAuthChallenge: complete a challenge issued by InitiateAuth /
# AdminInitiateAuth. Only NEW_PASSWORD_REQUIRED is modeled: the session must
# be known and unexpired (single use — consumed on success), and NEW_PASSWORD
# must satisfy the pool policy.
# {ChallengeName, ChallengeResponses: {USERNAME, NEW_PASSWORD}, ClientId, Session}
def _do_respond_to_challenge(req):
    body = _json_body(req)
    challenge_name = body.get("ChallengeName", "")

    if challenge_name != "NEW_PASSWORD_REQUIRED":
        return _cognito_err("InvalidParameterException",
            "ChallengeName " + challenge_name + " is not supported")

    session_id = body.get("Session", "")
    if session_id == "":
        return _cognito_err("InvalidParameterException",
            "Session is required")

    username = _peek_session(session_id, "NEW_PASSWORD_REQUIRED")
    if username == "":
        return _cognito_err("NotAuthorizedException",
            "Invalid session for the user, session is expired")

    responses = body.get("ChallengeResponses", {})
    if type(responses) != "dict":
        responses = {}
    new_password = responses.get("NEW_PASSWORD", "")
    if new_password == "":
        return _cognito_err("InvalidParameterException",
            "NEW_PASSWORD is required in ChallengeResponses")

    policy_err = _password_policy_error(new_password)
    if policy_err != "":
        return _cognito_err("InvalidPasswordException",
            "Password did not conform with policy: " + policy_err)

    uc = store_collection("users")
    user = uc.get(username)
    if user == None:
        return _cognito_err("UserNotFoundException",
            "User does not exist.")

    # Consume the session (single use) and finalize the password change.
    sc = store_collection("auth_sessions")
    sc.delete(session_id)
    user["password"] = new_password
    user["status"] = "CONFIRMED"
    uc.update(user["id"], user)

    client_id = body.get("ClientId", "")
    if client_id == "":
        client_id = "mock-client-id"
    return respond(200, _auth_result(user, client_id))

# ForgotPassword: start a password reset. Stores a 1-hour code window (the
# code itself is the deterministic _gen_code(username)) and resets the
# failed-attempt counter.
# {ClientId, Username}
def _do_forgot_password(req):
    body = _json_body(req)
    username = body.get("Username", "")
    if username == "":
        return _cognito_err("InvalidParameterException",
            "Username is required")

    uc = store_collection("users")
    user = uc.get(username)
    if user == None:
        return _cognito_err("UserNotFoundException",
            "User does not exist.")

    store_kv_set("cognito_fp_exp", username,
        str(clock.now_unix() + _CODE_TTL_SECS))
    store_kv_set("cognito_fp_fails", username, "0")

    return respond(200, {
        "CodeDeliveryDetails": {
            "AttributeName": "email",
            "DeliveryMedium": "EMAIL",
            "Destination": user.get("email", ""),
        },
    })

# ConfirmForgotPassword: finish a password reset. Real Cognito error ladder:
# 5 wrong codes → LimitExceededException; lapsed window →
# ExpiredCodeException; wrong code → CodeMismatchException; weak password →
# InvalidPasswordException. Success sets the new password, confirms the
# user, and returns {}.
# {ClientId, Username, ConfirmationCode, Password}
def _do_confirm_forgot_password(req):
    body = _json_body(req)
    username = body.get("Username", "")
    if username == "":
        return _cognito_err("InvalidParameterException",
            "Username is required")
    code = body.get("ConfirmationCode", "")
    password = body.get("Password", "")

    uc = store_collection("users")
    user = uc.get(username)
    if user == None:
        return _cognito_err("UserNotFoundException",
            "User does not exist.")

    fails = _to_int(store_kv_get("cognito_fp_fails", username))
    if fails >= 5:
        return _cognito_err("LimitExceededException",
            "Attempt limit exceeded, please try after some time.")

    exp_raw = store_kv_get("cognito_fp_exp", username)
    exp = _to_int(exp_raw)
    if exp_raw == None or exp_raw == "" or exp <= 0 or clock.now_unix() >= exp:
        return _cognito_err("ExpiredCodeException",
            "Invalid code provided, please request a code again.")

    if code != _gen_code(username):
        store_kv_set("cognito_fp_fails", username, str(fails + 1))
        return _cognito_err("CodeMismatchException",
            "Invalid code provided, please request a code again.")

    policy_err = _password_policy_error(password)
    if policy_err != "":
        return _cognito_err("InvalidPasswordException",
            "Password did not conform with policy: " + policy_err)

    user["password"] = password
    user["status"] = "CONFIRMED"
    uc.update(user["id"], user)
    store_kv_delete("cognito_fp_exp", username)
    store_kv_delete("cognito_fp_fails", username)

    return respond(200, {})

# GlobalSignOut: revoke every token (access + refresh) issued to the access
# token's user. Subsequent GetUser / userInfo / refresh calls fail.
# {AccessToken}
def _do_global_sign_out(req):
    body = _json_body(req)
    access_token = body.get("AccessToken", "")

    if access_token == "":
        return _cognito_err("NotAuthorizedException",
            "Access Token is required")

    user = _resolve_access(access_token)
    if user == None:
        return _cognito_err("NotAuthorizedException",
            "Invalid Access Token")

    _revoke_user_tokens(user["id"])
    return respond(200, {})

# AdminUserGlobalSignOut: revoke every token for a username (admin variant).
# {UserPoolId, Username}
def _do_admin_user_global_sign_out(req):
    body = _json_body(req)
    username = body.get("Username", "")

    uc = store_collection("users")
    user = uc.get(username)
    if user == None:
        return _cognito_err("ResourceNotFoundException",
            "User pool user does not exist.")

    _revoke_user_tokens(user["id"])
    return respond(200, {})

# GetUser: get user attributes from an access token.
# {AccessToken}. The token is a real RS256 JWT — signature + exp + iss +
# token_use are verified cryptographically, and the binding is gone after
# GlobalSignOut (NotAuthorizedException).
def _do_get_user(req):
    body = _json_body(req)
    access_token = body.get("AccessToken", "")

    if access_token == "":
        return _cognito_err("NotAuthorizedException",
            "Access token is required")

    user = _resolve_access(access_token)
    if user == None:
        return _cognito_err("NotAuthorizedException",
            "Invalid Access Token")

    # Build the GetUser response with UserAttributes array.
    attrs = user.get("attributes", {})
    user_attributes = []
    for name in attrs:
        user_attributes.append({"Name": name, "Value": attrs[name]})
    if not _has_sub_attr(user_attributes):
        user_attributes.append({"Name": "sub", "Value": user["sub"]})

    return respond(200, {
        "Username": user["username"],
        "UserAttributes": user_attributes,
    })

# ListUsers: list users in the user pool.
# {UserPoolId, [Filter], [Limit]}
def _do_list_users(req):
    uc = store_collection("users")
    docs = uc.list()
    users = []
    for d in docs:
        attrs = d.get("attributes", {})
        user_attributes = []
        for name in attrs:
            user_attributes.append({"Name": name, "Value": attrs[name]})
        if not _has_sub_attr(user_attributes):
            user_attributes.append({"Name": "sub", "Value": d["sub"]})
        users.append({
            "Username": d["username"],
            "Attributes": user_attributes,
            "Enabled": d.get("enabled", True),
            "UserStatus": d.get("status", "CONFIRMED"),
        })

    return respond(200, {"Users": users})

# AdminCreateUser: create a user as an admin. The user lands in
# FORCE_CHANGE_PASSWORD with a temporary password (TemporaryPassword param
# or a deterministic default) — the first password login returns a
# NEW_PASSWORD_REQUIRED challenge.
# {UserPoolId, Username, UserAttributes, [TemporaryPassword]}
def _do_admin_create_user(req):
    body = _json_body(req)
    username = body.get("Username", "")
    if username == "":
        return _cognito_err("InvalidParameterException",
            "Username is required")

    temp_password = body.get("TemporaryPassword", "")
    if temp_password == "":
        temp_password = "TempPass" + "1A!"

    uc = store_collection("users")
    existing = uc.get(username)
    if existing != None:
        return _cognito_err("UsernameExistsException",
            "User already exists")

    seq = store_kv_incr("cognito", "user_seq")
    sub = _SUB_PREFIX + _pad6(seq)

    attrs = _parse_user_attributes(body.get("UserAttributes", []))
    if "email" not in attrs:
        attrs["email"] = username + "@mock-cognito.com"

    user = {
        "id": username,
        "sub": sub,
        "username": username,
        "email": attrs.get("email", ""),
        "attributes": attrs,
        "password": temp_password,
        "enabled": True,
        "status": "FORCE_CHANGE_PASSWORD",
    }
    uc.insert(user)

    user_attributes = []
    for name in attrs:
        user_attributes.append({"Name": name, "Value": attrs[name]})
    user_attributes.append({"Name": "sub", "Value": sub})

    return respond(200, {
        "User": {
            "Username": username,
            "Attributes": user_attributes,
            "Enabled": True,
            "UserStatus": "FORCE_CHANGE_PASSWORD",
        },
    })

# --- Identity pool operations ---

# GetId: get a Cognito identity ID.
# {IdentityPoolId, Logins: {provider: token}}
def _do_get_id(req):
    body = _json_body(req)
    pool_id = body.get("IdentityPoolId", "mock-identity-pool")
    seq = store_kv_incr("cognito", "identity_seq")
    identity_id = pool_id + ":" + _pad6(seq)
    return respond(200, {"IdentityId": identity_id})

# GetCredentialsForIdentity: get AWS credentials for a Cognito identity.
# {IdentityId, Logins: {...}}
def _do_get_credentials(req):
    body = _json_body(req)
    identity_id = body.get("IdentityId", "")
    seq = store_kv_incr("cognito", "creds_seq")
    # Expiration is derived at runtime (real Cognito returns a future
    # timestamp one hour out, not a fixed epoch).
    return respond(200, {
        "IdentityId": identity_id,
        "Credentials": {
            "AccessKeyId": "ASIA" + _mock_key(seq),
            "SecretKey": "mock-secret-key-" + str(seq),
            "SessionToken": "mock-session-token-" + str(seq),
            "Expiration": clock.now_unix() + _CODE_TTL_SECS,
        },
    })

# --- auth flow cores ---

# _initiate_auth_flow is shared by InitiateAuth and AdminInitiateAuth.
# is_admin switches which password AuthFlows are allowed (the ADMIN_* flows
# belong to AdminInitiateAuth; USER_PASSWORD_AUTH to InitiateAuth).
def _initiate_auth_flow(body, is_admin):
    auth_flow = body.get("AuthFlow", "")
    auth_params = body.get("AuthParameters", {})
    if type(auth_params) != "dict":
        auth_params = {}
    client_id = body.get("ClientId", "")
    if client_id == "":
        client_id = "mock-client-id"

    # Refresh flows: fresh RS256 access/id pair; the presented refresh token
    # stays valid (no rotation — the real Cognito default) and is not
    # returned. Revoked/expired/unknown tokens are rejected.
    if auth_flow == "REFRESH_TOKEN_AUTH" or auth_flow == "REFRESH_TOKEN_REFRESH":
        presented = auth_params.get("REFRESH_TOKEN", "")
        user = _refresh_user(presented)
        if user == None:
            return _cognito_err("NotAuthorizedException",
                "Invalid Refresh Token")
        return respond(200, _refresh_result(user, client_id))

    if auth_flow == "":
        return _cognito_err("InvalidParameterException",
            "AuthFlow is required")

    if auth_flow == "USER_PASSWORD_AUTH" and is_admin:
        return _cognito_err("InvalidParameterException",
            "AuthFlow USER_PASSWORD_AUTH is not supported for AdminInitiateAuth")
    if (auth_flow == "ADMIN_USER_PASSWORD_AUTH" or auth_flow == "ADMIN_NO_SRP_AUTH") and not is_admin:
        return _cognito_err("InvalidParameterException",
            "AuthFlow " + auth_flow + " is not supported")

    if auth_flow != "USER_PASSWORD_AUTH" and auth_flow != "ADMIN_USER_PASSWORD_AUTH" and auth_flow != "ADMIN_NO_SRP_AUTH":
        return _cognito_err("InvalidParameterException",
            "AuthFlow " + auth_flow + " is not supported")

    username = auth_params.get("USERNAME", "")
    password = auth_params.get("PASSWORD", "")

    uc = store_collection("users")
    user = uc.get(username)
    if user == None or user.get("password", "") != password:
        return _cognito_err("NotAuthorizedException",
            "Incorrect username or password.")

    if not user.get("enabled", True):
        return _cognito_err("NotAuthorizedException",
            "User is disabled.")

    if user.get("status", "") == "UNCONFIRMED":
        return _cognito_err("UserNotConfirmedException",
            "User is not confirmed.")

    # First login of a FORCE_CHANGE_PASSWORD user with the temporary
    # password: challenge instead of tokens (real Cognito behavior).
    if user.get("status", "") == "FORCE_CHANGE_PASSWORD":
        return respond(200, _challenge_response(user, "NEW_PASSWORD_REQUIRED"))

    return respond(200, _auth_result(user, client_id))

# --- helpers ---

# _parse_user_attributes flattens a UserAttributes list into a dict.
def _parse_user_attributes(attrs):
    out = {}
    if type(attrs) != "list":
        return out
    for attr in attrs:
        if type(attr) != "dict":
            continue
        name = attr.get("Name", "")
        value = attr.get("Value", "")
        if name != "":
            out[name] = value
    return out

def _has_sub_attr(attrs):
    for a in attrs:
        if a["Name"] == "sub":
            return True
    return False

def _mock_key(seq):
    # Generate a synthetic AWS access key suffix.
    s = ""
    v = 0xCAFEBABE + seq
    for i in range(16):
        rem = v % 36
        if rem < 10:
            s = chr(ord("0") + rem) + s
        else:
            s = chr(ord("A") + rem - 10) + s
        v = v // 36
    return s
