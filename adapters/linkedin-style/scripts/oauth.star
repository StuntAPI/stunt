# OAuth2 handlers — Arctic-style authorization code + refresh-token rotation.
#
# LinkedIn uses body-param client credentials (NOT HTTP Basic Auth), per Arctic.
# The authorization-code flow mints a new member per code; the refresh_token
# grant rotates (the presented refresh token is consumed and a new pair is
# minted for the same member).
#
# GET  /oauth/v2/authorization  -> 302 redirect with code+state
# POST /oauth/v2/accessToken    -> { access_token, expires_in, refresh_token, scope }

# NOTE: Starlark load() is unavailable in stunt, so shared helpers are inlined.

# --- shared helpers (copied; keep in sync across scripts) ---

def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

def _member_for_token(req):
    token = _bearer(req)
    if token == "":
        return None
    c = store_collection("tokens")
    doc = c.get(token)
    if doc == None:
        return None
    return doc

def _pad3(n):
    if n < 10:
        return "00" + str(n)
    if n < 100:
        return "0" + str(n)
    return str(n)

def _mint_member():
    seq = store_kv_incr("linkedin", "member_seq")
    sub = "mock-member-" + _pad3(seq)
    return {
        "sub": sub,
        "name": "Mock Member " + str(seq),
        "email": "member" + str(seq) + "@example.test",
        "picture": "https://mock-linkedin.example/pic/" + sub + ".jpg",
    }

def _issue_tokens(member):
    access_seq = store_kv_incr("linkedin", "access_seq")
    refresh_seq = store_kv_incr("linkedin", "refresh_seq")
    access = "mock_access_" + str(access_seq)
    refresh = "mock_refresh_" + str(refresh_seq)

    tc = store_collection("tokens")
    member["id"] = access
    tc.insert(member)

    rc = store_collection("refresh_tokens")
    rdoc = {}
    for k in member:
        rdoc[k] = member[k]
    rdoc["id"] = refresh
    rc.insert(rdoc)

    return {
        "access_token": access,
        "expires_in": 5184000,
        "refresh_token": refresh,
        "scope": "openid profile w_member_social email",
    }

# --- handlers ---

# on_authorize handles the authorization-code redirect.
def on_authorize(req):
    redirect_uri = req["query"].get("redirect_uri", "")
    state = req["query"].get("state", "")
    client_id = req["query"].get("client_id", "")

    if redirect_uri == "" or state == "" or client_id == "":
        return respond(400, {"error": "invalid_request", "message": "missing redirect_uri/state/client_id"})

    code_seq = store_kv_incr("linkedin", "code_seq")
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

# on_access_token handles both authorization_code and refresh_token grants.
def on_access_token(req):
    body = req["body"]
    if body == None:
        body = {}
    grant_type = body.get("grant_type", "")

    if grant_type == "refresh_token":
        presented = body.get("refresh_token", "")
        client_id = body.get("client_id", "")
        client_secret = body.get("client_secret", "")

        if client_id == "" or client_secret == "":
            return respond(400, {"error": "invalid_client", "error_description": "missing client creds"})

        rc = store_collection("refresh_tokens")
        member = rc.get(presented)
        if member == None:
            return respond(400, {"error": "invalid_grant", "error_description": "invalid/used refresh token"})

        rc.delete(presented)
        member.pop("id")
        return respond(200, _issue_tokens(member))

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

    return respond(200, _issue_tokens(_mint_member()))

def _contains(s, substr):
    return s.find(substr) >= 0
