# AWS IAM/STS query-API handler.
#
# AWS IAM and STS are QUERY-APIs: the operation is selected by the `Action`
# query parameter. Both GET (query string) and POST (form-encoded body)
# carry the same parameters. Responses are XML.
#
# Supported actions:
#
#   STS:
#     AssumeRole              -> temp ASIA creds + AssumedRoleUser
#     AssumeRoleWithWebIdentity -> temp ASIA creds (OIDC federation)
#     GetSessionToken         -> temp ASIA creds
#     GetCallerIdentity       -> Arn/UserId/Account (who am I)
#     DecodeAuthorizationMessage -> decoded message
#
#   IAM:
#     ListRoles               -> list roles
#     GetRole                 -> single role
#     CreateRole              -> create role
#     ListUsers               -> list IAM users
#     CreateAccessKey         -> create AKIA access key
#
# Credential provider chain:
#   Local code resolves AssumeRole -> temp creds -> used for subsequent S3/etc
#   calls. This mock mints realistic ASIA... (temp) vs AKIA... (long-term)
#   access key prefixes so client code can distinguish them.
#
# Shared helpers (_require_auth, _xml_*, _gen_*) are preloaded from
# scripts/lib.star.

# _get_param returns the value of a named parameter from either the query
# string or the parsed form body (POST). Query takes precedence.
def _get_param(req, name):
    query = req.get("query")
    if query != None:
        val = query.get(name, "")
        if val != None and val != "":
            return val
    body = req.get("body")
    if body != None:
        val = body.get(name, "")
        if val != None and val != "":
            return str(val)
    return ""

# _get_action returns the Action parameter (dispatch key for query API).
def _get_action(req):
    action = _get_param(req, "Action")
    return action

# on_dispatch routes based on the Action query/body parameter.
def on_dispatch(req):
    err = _require_auth(req)
    if err != None:
        return err

    action = _get_action(req)
    if action == "":
        return _invalid_action(req, "")

    # STS actions
    if action == "AssumeRole":
        return _assume_role(req)
    if action == "AssumeRoleWithWebIdentity":
        return _assume_role_with_web_identity(req)
    if action == "GetSessionToken":
        return _get_session_token(req)
    if action == "GetCallerIdentity":
        return _get_caller_identity(req)
    if action == "DecodeAuthorizationMessage":
        return _decode_authorization_message(req)

    # IAM actions
    if action == "ListRoles":
        return _list_roles(req)
    if action == "GetRole":
        return _get_role(req)
    if action == "CreateRole":
        return _create_role(req)
    if action == "ListUsers":
        return _list_users(req)
    if action == "CreateAccessKey":
        return _create_access_key(req)

    return _invalid_action(req, action)

# ====================================================================
# STS actions
# ====================================================================

# _assume_role mints temporary credentials for a role.
# ?Action=AssumeRole&RoleArn=arn:...&RoleSessionName=...&DurationSeconds=3600
def _assume_role(req):
    role_arn = _get_param(req, "RoleArn")
    session_name = _get_param(req, "RoleSessionName")
    duration = _get_param(req, "DurationSeconds")

    if role_arn == "":
        return _invalid_param("RoleArn", "AssumeRole")

    if session_name == "":
        session_name = "stunt-session"

    # Mint temp creds
    access_key = _gen_temp_access_key()
    secret_key = _gen_secret_key()
    session_token = _gen_session_token()
    assumed_role_id = _gen_assumed_role_id()
    expiration = _expiration_time(duration)

    # Track issued temp creds (STATEFUL)
    tc = store_collection("temp_credentials")
    tc.insert({
        "accessKeyId": access_key,
        "secretAccessKey": secret_key,
        "sessionToken": session_token,
        "expiration": expiration,
        "arn": role_arn,
        "assumedRoleId": assumed_role_id,
        "type": "assumed-role",
    })

    # Also store a caller-identity record so GetCallerIdentity can resolve
    # the assumed role (model the credential provider chain).
    store_kv_set("sts", "caller_arn", role_arn)
    store_kv_set("sts", "caller_user_id", assumed_role_id)
    store_kv_set("sts", "caller_type", "assumed-role")

    xml = '<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">\n'
    xml = xml + "  <AssumeRoleResult>\n"
    xml = xml + "    <Credentials>\n"
    xml = xml + "      <AccessKeyId>" + _xml_escape(access_key) + "</AccessKeyId>\n"
    xml = xml + "      <SecretAccessKey>" + _xml_escape(secret_key) + "</SecretAccessKey>\n"
    xml = xml + "      <SessionToken>" + _xml_escape(session_token) + "</SessionToken>\n"
    xml = xml + "      <Expiration>" + _xml_escape(expiration) + "</Expiration>\n"
    xml = xml + "    </Credentials>\n"
    xml = xml + "    <AssumedRoleUser>\n"
    xml = xml + "      <Arn>" + _xml_escape(role_arn) + "</Arn>\n"
    xml = xml + "      <AssumedRoleId>" + _xml_escape(assumed_role_id) + ":" + _xml_escape(session_name) + "</AssumedRoleId>\n"
    xml = xml + "    </AssumedRoleUser>\n"
    xml = xml + "    <PackedPolicySize>6</PackedPolicySize>\n"
    xml = xml + "  </AssumeRoleResult>\n"
    xml = xml + "  <ResponseMetadata>\n"
    xml = xml + "    <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "  </ResponseMetadata>\n"
    xml = xml + "</AssumeRoleResponse>"
    return respond(200, xml, {"Content-Type": "text/xml"})

# _assume_role_with_web_identity mints temp creds via OIDC federation.
# ?Action=AssumeRoleWithWebIdentity&RoleArn=...&WebIdentityToken=...
def _assume_role_with_web_identity(req):
    role_arn = _get_param(req, "RoleArn")
    web_token = _get_param(req, "WebIdentityToken")
    session_name = _get_param(req, "RoleSessionName")

    if role_arn == "":
        return _invalid_param("RoleArn", "AssumeRoleWithWebIdentity")
    if web_token == "":
        return _invalid_param("WebIdentityToken", "AssumeRoleWithWebIdentity")

    if session_name == "":
        session_name = "web-identity-session"

    access_key = _gen_temp_access_key()
    secret_key = _gen_secret_key()
    session_token = _gen_session_token()
    assumed_role_id = _gen_assumed_role_id()
    expiration = _expiration_time("3600")

    tc = store_collection("temp_credentials")
    tc.insert({
        "accessKeyId": access_key,
        "secretAccessKey": secret_key,
        "sessionToken": session_token,
        "expiration": expiration,
        "arn": role_arn,
        "assumedRoleId": assumed_role_id,
        "type": "web-identity",
    })

    store_kv_set("sts", "caller_arn", role_arn)
    store_kv_set("sts", "caller_user_id", assumed_role_id)
    store_kv_set("sts", "caller_type", "web-identity")

    xml = '<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">\n'
    xml = xml + "  <AssumeRoleWithWebIdentityResult>\n"
    xml = xml + "    <Credentials>\n"
    xml = xml + "      <AccessKeyId>" + _xml_escape(access_key) + "</AccessKeyId>\n"
    xml = xml + "      <SecretAccessKey>" + _xml_escape(secret_key) + "</SecretAccessKey>\n"
    xml = xml + "      <SessionToken>" + _xml_escape(session_token) + "</SessionToken>\n"
    xml = xml + "      <Expiration>" + _xml_escape(expiration) + "</Expiration>\n"
    xml = xml + "    </Credentials>\n"
    xml = xml + "    <SubjectFromWebIdentityToken>amzn1.account." + _xml_escape(session_name) + "</SubjectFromWebIdentityToken>\n"
    xml = xml + "    <AssumedRoleUser>\n"
    xml = xml + "      <Arn>" + _xml_escape(role_arn) + "</Arn>\n"
    xml = xml + "      <AssumedRoleId>" + _xml_escape(assumed_role_id) + ":" + _xml_escape(session_name) + "</AssumedRoleId>\n"
    xml = xml + "    </AssumedRoleUser>\n"
    xml = xml + "    <PackedPolicySize>6</PackedPolicySize>\n"
    xml = xml + "    <Provider>www.amazon.com</Provider>\n"
    xml = xml + "    <Audience>stunt.local.audience</Audience>\n"
    xml = xml + "  </AssumeRoleWithWebIdentityResult>\n"
    xml = xml + "  <ResponseMetadata>\n"
    xml = xml + "    <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "  </ResponseMetadata>\n"
    xml = xml + "</AssumeRoleWithWebIdentityResponse>"
    return respond(200, xml, {"Content-Type": "text/xml"})

# _get_session_token mints temporary credentials for the caller.
# ?Action=GetSessionToken&DurationSeconds=3600
def _get_session_token(req):
    duration = _get_param(req, "DurationSeconds")

    access_key = _gen_temp_access_key()
    secret_key = _gen_secret_key()
    session_token = _gen_session_token()
    expiration = _expiration_time(duration)

    tc = store_collection("temp_credentials")
    tc.insert({
        "accessKeyId": access_key,
        "secretAccessKey": secret_key,
        "sessionToken": session_token,
        "expiration": expiration,
        "type": "session-token",
    })

    xml = '<GetSessionTokenResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">\n'
    xml = xml + "  <GetSessionTokenResult>\n"
    xml = xml + "    <Credentials>\n"
    xml = xml + "      <AccessKeyId>" + _xml_escape(access_key) + "</AccessKeyId>\n"
    xml = xml + "      <SecretAccessKey>" + _xml_escape(secret_key) + "</SecretAccessKey>\n"
    xml = xml + "      <SessionToken>" + _xml_escape(session_token) + "</SessionToken>\n"
    xml = xml + "      <Expiration>" + _xml_escape(expiration) + "</Expiration>\n"
    xml = xml + "    </Credentials>\n"
    xml = xml + "  </GetSessionTokenResult>\n"
    xml = xml + "  <ResponseMetadata>\n"
    xml = xml + "    <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "  </ResponseMetadata>\n"
    xml = xml + "</GetSessionTokenResponse>"
    return respond(200, xml, {"Content-Type": "text/xml"})

# _get_caller_identity returns who the current credentials represent.
# ?Action=GetCallerIdentity
# This is the canonical "who am I" — critical for local dev to know which
# creds are active. After AssumeRole, the caller identity reflects the
# assumed role (modeling the credential provider chain).
def _get_caller_identity(req):
    arn = store_kv_get("sts", "caller_arn")
    if arn == None or arn == "":
        arn = "arn:aws:sts::123456789012:assumed-role/stunt-role/stunt-session"
    user_id = store_kv_get("sts", "caller_user_id")
    if user_id == None or user_id == "":
        user_id = "AROAEXAMPLE12345:stunt-session"

    # Extract account ID from the ARN (4th field)
    account = _extract_account(arn)

    xml = '<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">\n'
    xml = xml + "  <GetCallerIdentityResult>\n"
    xml = xml + "    <Arn>" + _xml_escape(arn) + "</Arn>\n"
    xml = xml + "    <UserId>" + _xml_escape(user_id) + "</UserId>\n"
    xml = xml + "    <Account>" + _xml_escape(account) + "</Account>\n"
    xml = xml + "  </GetCallerIdentityResult>\n"
    xml = xml + "  <ResponseMetadata>\n"
    xml = xml + "    <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "  </ResponseMetadata>\n"
    xml = xml + "</GetCallerIdentityResponse>"
    return respond(200, xml, {"Content-Type": "text/xml"})

# _decode_authorization_message decodes an encoded authorization message.
# ?Action=DecodeAuthorizationMessage&EncodedMessage=...
def _decode_authorization_message(req):
    encoded = _get_param(req, "EncodedMessage")
    if encoded == "":
        return _invalid_param("EncodedMessage", "DecodeAuthorizationMessage")

    # Return a synthetic decoded message
    decoded = '{"allowed": "false", "encodedVerb": "sts:AssumeRole", "encodedResource": "arn:aws:sts::123456789012:assumed-role/example"}'

    xml = '<DecodeAuthorizationMessageResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">\n'
    xml = xml + "  <DecodeAuthorizationMessageResult>\n"
    xml = xml + "    <DecodedMessage>" + _xml_escape(decoded) + "</DecodedMessage>\n"
    xml = xml + "  </DecodeAuthorizationMessageResult>\n"
    xml = xml + "  <ResponseMetadata>\n"
    xml = xml + "    <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "  </ResponseMetadata>\n"
    xml = xml + "</DecodeAuthorizationMessageResponse>"
    return respond(200, xml, {"Content-Type": "text/xml"})

# ====================================================================
# IAM actions
# ====================================================================

# _list_roles lists IAM roles.
# ?Action=ListRoles&Version=2010-05-08
def _list_roles(req):
    rc = store_collection("roles")
    _ensure_seed_roles()
    roles = rc.list()

    xml = '<ListRolesResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/">\n'
    xml = xml + "  <ListRolesResult>\n"
    xml = xml + "    <Roles>\n"
    for r in roles:
        xml = xml + "      <member>\n"
        xml = xml + "        <Path>" + _xml_escape(r.get("path", "/")) + "</Path>\n"
        xml = xml + "        <RoleName>" + _xml_escape(r.get("roleName", "")) + "</RoleName>\n"
        xml = xml + "        <RoleId>" + _xml_escape(r.get("roleId", "")) + "</RoleId>\n"
        xml = xml + "        <Arn>" + _xml_escape(r.get("arn", "")) + "</Arn>\n"
        xml = xml + "        <CreateDate>" + _xml_escape(r.get("createDate", "2024-01-01T00:00:00Z")) + "</CreateDate>\n"
        xml = xml + "        <AssumeRolePolicyDocument>" + _xml_escape(r.get("assumeRolePolicyDocument", "")) + "</AssumeRolePolicyDocument>\n"
        xml = xml + "        <MaxSessionDuration>3600</MaxSessionDuration>\n"
        xml = xml + "      </member>\n"
    xml = xml + "    </Roles>\n"
    xml = xml + "    <IsTruncated>false</IsTruncated>\n"
    xml = xml + "  </ListRolesResult>\n"
    xml = xml + "  <ResponseMetadata>\n"
    xml = xml + "    <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "  </ResponseMetadata>\n"
    xml = xml + "</ListRolesResponse>"
    return respond(200, xml, {"Content-Type": "text/xml"})

# _get_role returns a single role.
# ?Action=GetRole&RoleName=...
def _get_role(req):
    role_name = _get_param(req, "RoleName")
    if role_name == "":
        return _invalid_param("RoleName", "GetRole")

    _ensure_seed_roles()
    rc = store_collection("roles")
    role = None
    for r in rc.list():
        if r.get("roleName", "") == role_name:
            role = r
            break
    if role == None:
        return _no_such_entity("Role", role_name)

    xml = '<GetRoleResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/">\n'
    xml = xml + "  <GetRoleResult>\n"
    xml = xml + "    <Role>\n"
    xml = xml + "      <Path>" + _xml_escape(role.get("path", "/")) + "</Path>\n"
    xml = xml + "      <RoleName>" + _xml_escape(role.get("roleName", "")) + "</RoleName>\n"
    xml = xml + "      <RoleId>" + _xml_escape(role.get("roleId", "")) + "</RoleId>\n"
    xml = xml + "      <Arn>" + _xml_escape(role.get("arn", "")) + "</Arn>\n"
    xml = xml + "      <CreateDate>" + _xml_escape(role.get("createDate", "2024-01-01T00:00:00Z")) + "</CreateDate>\n"
    xml = xml + "      <AssumeRolePolicyDocument>" + _xml_escape(role.get("assumeRolePolicyDocument", "")) + "</AssumeRolePolicyDocument>\n"
    xml = xml + "      <MaxSessionDuration>3600</MaxSessionDuration>\n"
    xml = xml + "    </Role>\n"
    xml = xml + "  </GetRoleResult>\n"
    xml = xml + "  <ResponseMetadata>\n"
    xml = xml + "    <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "  </ResponseMetadata>\n"
    xml = xml + "</GetRoleResponse>"
    return respond(200, xml, {"Content-Type": "text/xml"})

# _create_role creates a new IAM role.
# ?Action=CreateRole&RoleName=...&AssumeRolePolicyDocument=...
def _create_role(req):
    role_name = _get_param(req, "RoleName")
    policy_doc = _get_param(req, "AssumeRolePolicyDocument")

    if role_name == "":
        return _invalid_param("RoleName", "CreateRole")

    role_id = _gen_unique_id()
    arn = "arn:aws:iam::123456789012:role/" + role_name

    if policy_doc == "":
        policy_doc = '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}'

    rc = store_collection("roles")
    rc.insert({
        "roleName": role_name,
        "roleId": role_id,
        "arn": arn,
        "path": "/",
        "createDate": "2024-01-01T00:00:00Z",
        "assumeRolePolicyDocument": policy_doc,
    })

    xml = '<CreateRoleResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/">\n'
    xml = xml + "  <CreateRoleResult>\n"
    xml = xml + "    <Role>\n"
    xml = xml + "      <Path>/</Path>\n"
    xml = xml + "      <RoleName>" + _xml_escape(role_name) + "</RoleName>\n"
    xml = xml + "      <RoleId>" + _xml_escape(role_id) + "</RoleId>\n"
    xml = xml + "      <Arn>" + _xml_escape(arn) + "</Arn>\n"
    xml = xml + "      <CreateDate>2024-01-01T00:00:00Z</CreateDate>\n"
    xml = xml + "      <AssumeRolePolicyDocument>" + _xml_escape(policy_doc) + "</AssumeRolePolicyDocument>\n"
    xml = xml + "      <MaxSessionDuration>3600</MaxSessionDuration>\n"
    xml = xml + "    </Role>\n"
    xml = xml + "  </CreateRoleResult>\n"
    xml = xml + "  <ResponseMetadata>\n"
    xml = xml + "    <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "  </ResponseMetadata>\n"
    xml = xml + "</CreateRoleResponse>"
    return respond(200, xml, {"Content-Type": "text/xml"})

# _list_users lists IAM users.
# ?Action=ListUsers
def _list_users(req):
    _ensure_seed_users()
    uc = store_collection("users")
    users = uc.list()

    xml = '<ListUsersResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/">\n'
    xml = xml + "  <ListUsersResult>\n"
    xml = xml + "    <Users>\n"
    for u in users:
        xml = xml + "      <member>\n"
        xml = xml + "        <Path>" + _xml_escape(u.get("path", "/")) + "</Path>\n"
        xml = xml + "        <UserName>" + _xml_escape(u.get("userName", "")) + "</UserName>\n"
        xml = xml + "        <UserId>" + _xml_escape(u.get("userId", "")) + "</UserId>\n"
        xml = xml + "        <Arn>" + _xml_escape(u.get("arn", "")) + "</Arn>\n"
        xml = xml + "        <CreateDate>" + _xml_escape(u.get("createDate", "2024-01-01T00:00:00Z")) + "</CreateDate>\n"
        xml = xml + "      </member>\n"
    xml = xml + "    </Users>\n"
    xml = xml + "    <IsTruncated>false</IsTruncated>\n"
    xml = xml + "  </ListUsersResult>\n"
    xml = xml + "  <ResponseMetadata>\n"
    xml = xml + "    <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "  </ResponseMetadata>\n"
    xml = xml + "</ListUsersResponse>"
    return respond(200, xml, {"Content-Type": "text/xml"})

# _create_access_key creates a long-term access key for a user.
# ?Action=CreateAccessKey&UserName=...
def _create_access_key(req):
    user_name = _get_param(req, "UserName")
    if user_name == "":
        return _invalid_param("UserName", "CreateAccessKey")

    access_key = _gen_long_access_key()
    secret_key = _gen_secret_key()

    akc = store_collection("access_keys")
    akc.insert({
        "userName": user_name,
        "accessKeyId": access_key,
        "status": "Active",
    })

    xml = '<CreateAccessKeyResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/">\n'
    xml = xml + "  <CreateAccessKeyResult>\n"
    xml = xml + "    <AccessKey>\n"
    xml = xml + "      <UserName>" + _xml_escape(user_name) + "</UserName>\n"
    xml = xml + "      <AccessKeyId>" + _xml_escape(access_key) + "</AccessKeyId>\n"
    xml = xml + "      <Status>Active</Status>\n"
    xml = xml + "      <SecretAccessKey>" + _xml_escape(secret_key) + "</SecretAccessKey>\n"
    xml = xml + "      <CreateDate>2024-01-01T00:00:00Z</CreateDate>\n"
    xml = xml + "    </AccessKey>\n"
    xml = xml + "  </CreateAccessKeyResult>\n"
    xml = xml + "  <ResponseMetadata>\n"
    xml = xml + "    <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "  </ResponseMetadata>\n"
    xml = xml + "</CreateAccessKeyResponse>"
    return respond(200, xml, {"Content-Type": "text/xml"})

# ====================================================================
# Helpers
# ====================================================================

# _expiration_time returns a synthetic ISO 8601 expiration timestamp.
# AWS STS temp creds expire after DurationSeconds (default 3600 = 1 hour).
def _expiration_time(duration_str):
    if duration_str == None or duration_str == "":
        duration_str = "3600"
    # Parse duration to int (best-effort)
    dur = _parse_int(duration_str)
    if dur == 0:
        dur = 3600
    # Synthesize a future timestamp. We use a fixed base + duration.
    # Format: 2024-01-01T01:00:00Z (base + dur hours, simplified).
    hours = dur // 3600
    if hours < 1:
        hours = 1
    h_str = _pad2(hours)
    return "2024-01-01T" + h_str + ":00:00Z"

# _parse_int parses a decimal string to int. Returns 0 on failure.
def _parse_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _pad2 zero-pads a number to 2 digits.
def _pad2(n):
    if n < 10:
        return "0" + str(n)
    return str(n)

# _extract_account pulls the account ID (12 digits) from an ARN.
# arn:aws:iam::123456789012:role/MyRole -> 123456789012
def _extract_account(arn):
    if arn == None or arn == "":
        return "123456789012"
    parts = _split(arn, ":")
    if len(parts) >= 5:
        acct = parts[4]
        if len(acct) == 12:
            return acct
    return "123456789012"

# _ensure_seed_roles seeds the roles collection with a default role if empty.
def _ensure_seed_roles():
    rc = store_collection("roles")
    if len(rc.list()) > 0:
        return
    seeded = store_kv_get("sts", "roles_seeded")
    if seeded == "1":
        return
    rc.insert({
        "roleName": "stunt-role",
        "roleId": "AROAEXAMPLESEED0",
        "arn": "arn:aws:iam::123456789012:role/stunt-role",
        "path": "/",
        "createDate": "2024-01-01T00:00:00Z",
        "assumeRolePolicyDocument": '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}',
    })
    store_kv_set("sts", "roles_seeded", "1")

# _ensure_seed_users seeds the users collection with a default user if empty.
def _ensure_seed_users():
    uc = store_collection("users")
    if len(uc.list()) > 0:
        return
    seeded = store_kv_get("sts", "users_seeded")
    if seeded == "1":
        return
    uc.insert({
        "userName": "stunt-admin",
        "userId": "AIDAEXAMPLEUSER0",
        "arn": "arn:aws:iam::123456789012:user/stunt-admin",
        "path": "/",
        "createDate": "2024-01-01T00:00:00Z",
    })
    store_kv_set("sts", "users_seeded", "1")

# ====================================================================
# Error responses (AWS IAM/STS XML shape)
# ====================================================================

def _invalid_action(req, action):
    xml = '<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">\n'
    xml = xml + "  <Error>\n"
    xml = xml + "    <Type>Sender</Type>\n"
    xml = xml + "    <Code>InvalidAction</Code>\n"
    if action == "":
        xml = xml + "    <Message>Missing required parameter: Action</Message>\n"
    else:
        xml = xml + "    <Message>Invalid action: " + _xml_escape(action) + "</Message>\n"
    xml = xml + "  </Error>\n"
    xml = xml + "  <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "</ErrorResponse>"
    return respond(400, xml, {"Content-Type": "text/xml"})

def _invalid_param(param, action):
    xml = '<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">\n'
    xml = xml + "  <Error>\n"
    xml = xml + "    <Type>Sender</Type>\n"
    xml = xml + "    <Code>ValidationError</Code>\n"
    xml = xml + "    <Message>Missing required parameter: " + _xml_escape(param) + " for action " + _xml_escape(action) + "</Message>\n"
    xml = xml + "  </Error>\n"
    xml = xml + "  <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "</ErrorResponse>"
    return respond(400, xml, {"Content-Type": "text/xml"})

def _no_such_entity(entity_type, name):
    xml = '<ErrorResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/">\n'
    xml = xml + "  <Error>\n"
    xml = xml + "    <Type>Sender</Type>\n"
    xml = xml + "    <Code>NoSuchEntity</Code>\n"
    xml = xml + "    <Message>The " + _xml_escape(entity_type) + " with name " + _xml_escape(name) + " cannot be found.</Message>\n"
    xml = xml + "  </Error>\n"
    xml = xml + "  <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "</ErrorResponse>"
    return respond(404, xml, {"Content-Type": "text/xml"})
