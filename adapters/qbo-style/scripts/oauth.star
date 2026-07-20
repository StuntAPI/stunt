# OAuth2 handlers — QBO authorization-code + refresh-token flow.
#
# GET  /oauth/v2/authorize  -> 302 redirect with code+state+realmId
# POST /oauth/v2/tokens/bearer (form: grant_type, code/refresh_token, client_id, client_secret)
#   -> { access_token, refresh_token, token_type, expires_in, x_refresh_token_expires_in }
#
# The INFAMOUS QBO refresh-token churn: each refresh returns a NEW
# refresh_token. The old refresh_token is invalidated.

# Shared helpers from lib.star.

def on_authorize(req):
    q = req.get("query")
    if q == None:
        q = {}
    client_id = q.get("client_id", "")
    redirect_uri = q.get("redirect_uri", "")
    state = q.get("state", "")
    response_type = q.get("response_type", "")
    scope = q.get("scope", "")

    if redirect_uri == "" or state == "" or client_id == "":
        return respond(400, {"error": "invalid_request", "error_description": "missing required params"})

    # Generate auth code and realm.
    code_seq = store_kv_incr("qbo", "code_seq")
    code = "Q0_" + str(code_seq) + "_mock_auth_code"

    realm_seq = store_kv_incr("qbo", "realm_seq")
    realm_id = str(9130000000000000000 + realm_seq)

    cc = store_collection("oauth_codes")
    cc.insert({
        "id": code,
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "realm_id": realm_id,
    })

    sep = "?"
    if "?" in redirect_uri:
        sep = "&"
    location = redirect_uri + sep + "code=" + code + "&state=" + state + "&realmId=" + realm_id
    return respond(302, headers={"Location": location})

def on_token(req):
    body = req["body"]
    if body == None:
        body = {}
    grant_type = body.get("grant_type", "")
    client_id = body.get("client_id", "")
    client_secret = body.get("client_secret", "")

    if grant_type == "authorization_code":
        code = body.get("code", "")

        cc = store_collection("oauth_codes")
        code_doc = cc.get(code)
        if code_doc == None:
            return respond(400, {"error": "invalid_grant", "error_description": "invalid or expired code"})

        # Codes are single-use.
        cc.delete(code)

        realm_id = code_doc.get("realm_id", "9130000000000000001")

        return respond(200, _issue_token_pair(realm_id))

    if grant_type == "refresh_token":
        presented = body.get("refresh_token", "")

        rc = store_collection("refresh_tokens")
        rt_doc = rc.get(presented)
        if rt_doc == None:
            return respond(400, {"error": "invalid_grant", "error_description": "invalid or expired refresh token"})

        realm_id = rt_doc.get("realm_id", "9130000000000000001")

        # INVALIDATE the old refresh token (QBO refresh churn).
        rc.delete(presented)

        return respond(200, _issue_token_pair(realm_id))

    return respond(400, {"error": "unsupported_grant_type"})

# _issue_token_pair issues a fresh access_token + refresh_token pair bound
# to a realmId. The refresh token is stored for later validation.
def _issue_token_pair(realm_id):
    access_seq = store_kv_incr("qbo", "access_seq")
    refresh_seq = store_kv_incr("qbo", "refresh_seq")

    access = "Q0_" + str(access_seq) + "_mock_access_token"
    refresh = "Q0_" + str(refresh_seq) + "_mock_refresh_token"

    ac = store_collection("access_tokens")
    ac.insert({
        "id": access,
        "realm_id": realm_id,
    })

    rc = store_collection("refresh_tokens")
    rc.insert({
        "id": refresh,
        "realm_id": realm_id,
    })

    return {
        "access_token": access,
        "refresh_token": refresh,
        "token_type": "bearer",
        "expires_in": 3600,
        "x_refresh_token_expires_in": 8726400,
    }
