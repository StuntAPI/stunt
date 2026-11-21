# Synthetic OAuth-app sign-in, separate from GitHub App installation tokens.
def on_authorize(req):
    q = req["query"]
    client = q.get("client_id", "")
    redirect = q.get("redirect_uri", "")
    state = q.get("state", "")
    if client == "" or redirect == "" or state == "":
        return respond(400, {"error": "invalid_request"})
    code = "stunt_oauth_code_" + str(store_kv_incr("oauth", "code_seq"))
    store_collection("oauth_codes").insert({"id": code, "client_id": client, "redirect_uri": redirect, "scope": q.get("scope", ""), "code_challenge": q.get("code_challenge", ""), "code_challenge_method": q.get("code_challenge_method", "plain"), "expires_at": clock.now_unix() + 600})
    sep = "&" if "?" in redirect else "?"
    return respond(302, headers={"Location": redirect + sep + "code=" + code + "&state=" + state})

def on_token(req):
    body = req["body"] or {}
    code = body.get("code", "")
    codes = store_collection("oauth_codes")
    doc = codes.get(code)
    if doc == None or doc["expires_at"] <= clock.now_unix():
        return respond(400, {"error": "bad_verification_code"})
    if body.get("client_id", "") != doc["client_id"] or body.get("client_secret", "") == "" or body.get("redirect_uri", "") != doc["redirect_uri"]:
        return respond(400, {"error": "incorrect_client_credentials"})
    challenge = doc["code_challenge"]
    if challenge != "":
        verifier = body.get("code_verifier", "")
        method = doc["code_challenge_method"]
        actual = crypto.sha256(verifier, encoding="base64url") if method == "S256" else verifier
        if method not in ["S256", "plain"] or actual != challenge:
            return respond(400, {"error": "bad_verification_code"})
    codes.delete(code)
    seq = store_kv_incr("oauth", "token_seq")
    token = "stunt_oauth_access_" + str(seq)
    store_collection("oauth_tokens").insert({"id": token, "user_id": seq, "scope": doc["scope"]})
    return respond(200, {"access_token": token, "token_type": "bearer", "scope": doc["scope"]})

def _oauth_user(req):
    auth = req["headers"].get("Authorization", "")
    if not auth.startswith("Bearer "):
        return None
    return store_collection("oauth_tokens").get(auth[7:])

def on_user(req):
    user = _oauth_user(req)
    if user == None:
        return respond(401, {"message": "Bad credentials"})
    uid = user["user_id"]
    return respond(200, {"id": uid, "login": "stunt-user-" + str(uid), "name": "Stunt User " + str(uid), "email": None, "avatar_url": "https://avatars.example.test/stunt/" + str(uid)})

def on_emails(req):
    user = _oauth_user(req)
    if user == None:
        return respond(401, {"message": "Bad credentials"})
    return respond(200, [{"email": "stunt-user-" + str(user["user_id"]) + "@example.test", "primary": True, "verified": True, "visibility": "private"}])
