# Cognito Service API handlers (user pool + identity pool).
#
# POST / with X-Amz-Target header dispatches to the appropriate operation.
#
# Supported X-Amz-Target values:
#   AWSCognitoIdentityProviderService.SignUp
#   AWSCognitoIdentityProviderService.InitiateAuth
#   AWSCognitoIdentityProviderService.RespondToAuthChallenge
#   AWSCognitoIdentityProviderService.ConfirmSignUp
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
    target = req["headers"].get("X-Amz-Target", "")

    if target == "AWSCognitoIdentityProviderService.SignUp":
        return _do_signup(req)
    if target == "AWSCognitoIdentityProviderService.InitiateAuth":
        return _do_initiate_auth(req)
    if target == "AWSCognitoIdentityProviderService.RespondToAuthChallenge":
        return _do_respond_to_challenge(req)
    if target == "AWSCognitoIdentityProviderService.ConfirmSignUp":
        return _do_confirm_signup(req)
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
    body = req["body"]
    if body == None:
        body = {}
    username = body.get("Username", "")
    password = body.get("Password", "")
    client_id = body.get("ClientId", "")

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
    sub = "00000000-0000-0000-0000-" + _pad6(seq)

    # Parse UserAttributes.
    attrs = {}
    for attr in body.get("UserAttributes", []):
        name = attr.get("Name", "")
        value = attr.get("Value", "")
        if name != "":
            attrs[name] = value

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

# InitiateAuth: start an auth flow (USER_PASSWORD_AUTH).
# {AuthFlow: "USER_PASSWORD_AUTH", AuthParameters: {USERNAME, PASSWORD}, ClientId}
def _do_initiate_auth(req):
    body = req["body"]
    if body == None:
        body = {}
    auth_flow = body.get("AuthFlow", "")
    auth_params = body.get("AuthParameters", {})

    if auth_flow != "USER_PASSWORD_AUTH":
        return _cognito_err("InvalidParameterException",
            "AuthFlow " + auth_flow + " is not supported")

    username = auth_params.get("USERNAME", "")
    password = auth_params.get("PASSWORD", "")

    uc = store_collection("users")
    user = uc.get(username)
    if user == None:
        return _cognito_err("NotAuthorizedException",
            "Incorrect username or password.")

    if user.get("password", "") != password:
        return _cognito_err("NotAuthorizedException",
            "Incorrect username or password.")

    if user.get("status", "") == "UNCONFIRMED":
        return _cognito_err("UserNotConfirmedException",
            "User is not confirmed.")

    return respond(200, _auth_result(user))

# RespondToAuthChallenge: handle an auth challenge.
# {ChallengeName, ChallengeResponses, ClientId, Session}
def _do_respond_to_challenge(req):
    body = req["body"]
    if body == None:
        body = {}
    challenge_name = body.get("ChallengeName", "")

    # For simplicity, any challenge response succeeds and returns tokens.
    responses = body.get("ChallengeResponses", {})
    username = responses.get("USERNAME", "mock_user")

    uc = store_collection("users")
    user = uc.get(username)
    if user == None:
        user = _mint_user_by_name(username)

    return respond(200, _auth_result(user))

# ConfirmSignUp: confirm a user's registration.
# {ClientId, Username, ConfirmationCode}
def _do_confirm_signup(req):
    body = req["body"]
    if body == None:
        body = {}
    username = body.get("Username", "")

    uc = store_collection("users")
    user = uc.get(username)
    if user == None:
        return _cognito_err("UserNotFoundException",
            "User does not exist.")

    uc.delete(username)
    user["status"] = "CONFIRMED"
    uc.insert(user)

    return respond(200, {})

# GetUser: get user attributes from an access token.
# {AccessToken}
def _do_get_user(req):
    body = req["body"]
    if body == None:
        body = {}
    access_token = body.get("AccessToken", "")

    if access_token == "":
        return _cognito_err("NotAuthorizedException",
            "Access token is required")

    # Look up the user via the token → user binding.
    tc = store_collection("tokens")
    tok_doc = tc.get(access_token)
    if tok_doc == None:
        return _cognito_err("NotAuthorizedException",
            "Invalid Access Token")

    user_id = tok_doc.get("user_id", "")
    uc = store_collection("users")
    user = uc.get(user_id)
    if user == None:
        return _cognito_err("UserNotFoundException",
            "User not found")

    # Build the GetUser response with UserAttributes array.
    attrs = user.get("attributes", {})
    user_attributes = []
    for name in attrs:
        user_attributes.append({"Name": name, "Value": attrs[name]})
    # Ensure sub is present.
    has_sub = False
    for a in user_attributes:
        if a["Name"] == "sub":
            has_sub = True
    if not has_sub:
        user_attributes.append({"Name": "sub", "Value": user["sub"]})

    return respond(200, {
        "Username": user["username"],
        "UserAttributes": user_attributes,
    })

# ListUsers: list users in the user pool.
# {UserPoolId, [Filter], [Limit]}
def _do_list_users(req):
    body = req["body"]
    if body == None:
        body = {}

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

# AdminCreateUser: create a user as an admin.
# {UserPoolId, Username, UserAttributes}
def _do_admin_create_user(req):
    body = req["body"]
    if body == None:
        body = {}
    username = body.get("Username", "")

    seq = store_kv_incr("cognito", "user_seq")
    sub = "00000000-0000-0000-0000-" + _pad6(seq)

    attrs = {}
    for attr in body.get("UserAttributes", []):
        name = attr.get("Name", "")
        value = attr.get("Value", "")
        if name != "":
            attrs[name] = value
    if "email" not in attrs:
        attrs["email"] = username + "@mock-cognito.com"

    user = {
        "id": username,
        "sub": sub,
        "username": username,
        "email": attrs.get("email", ""),
        "attributes": attrs,
        "password": "",
        "enabled": True,
        "status": "FORCE_CHANGE_PASSWORD",
    }
    uc = store_collection("users")
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
    body = req["body"]
    if body == None:
        body = {}
    pool_id = body.get("IdentityPoolId", "mock-identity-pool")
    seq = store_kv_incr("cognito", "identity_seq")
    identity_id = pool_id + ":" + _pad6(seq)
    return respond(200, {"IdentityId": identity_id})

# GetCredentialsForIdentity: get AWS credentials for a Cognito identity.
# {IdentityId, Logins: {...}}
def _do_get_credentials(req):
    body = req["body"]
    if body == None:
        body = {}
    identity_id = body.get("IdentityId", "")
    seq = store_kv_incr("cognito", "creds_seq")
    return respond(200, {
        "IdentityId": identity_id,
        "Credentials": {
            "AccessKeyId": "ASIA" + _mock_key(seq),
            "SecretKey": "mock-secret-key-" + str(seq),
            "SessionToken": "mock-session-token-" + str(seq),
            "Expiration": 1718534400,
        },
    })

# --- helpers ---

# _auth_result returns an AuthenticationResult for a successfully authed user.
def _auth_result(user):
    access_seq = store_kv_incr("cognito", "access_seq")
    refresh_seq = store_kv_incr("cognito", "refresh_seq")

    access = _mint_jwt(user["sub"], user["username"], user.get("email", ""), "acc" + str(access_seq))
    id_token = _mint_jwt(user["sub"], user["username"], user.get("email", ""), "id" + str(access_seq))
    refresh = "mock-refresh-token-" + str(refresh_seq)

    # Store the access token → user binding for GetUser.
    tc = store_collection("tokens")
    tc.insert({
        "id": access,
        "user_id": user["id"],
        "token_type": "access",
    })

    # Store refresh → user binding in KV.
    store_kv_set("cognito_refresh", refresh, user["id"])

    return {
        "AuthenticationResult": {
            "AccessToken": access,
            "IdToken": id_token,
            "RefreshToken": refresh,
            "TokenType": "Bearer",
            "ExpiresIn": 3600,
        },
        "ChallengeParameters": {},
    }

# _mint_user_by_name creates a user if not found (for challenge responses).
def _mint_user_by_name(username):
    seq = store_kv_incr("cognito", "user_seq")
    sub = "00000000-0000-0000-0000-" + _pad6(seq)
    user = {
        "id": username,
        "sub": sub,
        "username": username,
        "email": username + "@mock-cognito.com",
        "attributes": {
            "email": username + "@mock-cognito.com",
            "email_verified": "true",
            "sub": sub,
        },
        "password": "",
        "enabled": True,
        "status": "CONFIRMED",
    }
    uc = store_collection("users")
    uc.insert(user)
    return user

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
