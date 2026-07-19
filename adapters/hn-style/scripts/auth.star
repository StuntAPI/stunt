# Auth handlers — login/logout (mirrors ***REMOVED*** mock_hn).
#
# POST /login {acct, pw} -> 302 redirect + Set-Cookie: user=<token>
# GET  /logout            -> 302 redirect (clears session)
#
# The mock_hn server uses a session cookie ("user") to track login state.
# This adapter mirrors that contract: login mints a session token, and the
# submit endpoint requires the cookie.

# Shared helpers (_to_int) are preloaded from scripts/lib.star.

def on_login(req):
    body = req["body"]
    if body == None:
        body = {}
    acct = body.get("acct", "")
    pw = body.get("pw", "")

    if acct == "":
        return respond(400, {"error": "missing acct"})

    # Mint a session token.
    token_seq = store_kv_incr("hn", "session_seq")
    token = "hn_session_" + str(token_seq)

    sc = store_collection("sessions")
    sc.insert({
        "id": token,
        "username": acct,
    })

    # Ensure the user exists in the users collection (auto-create on first login).
    uc = store_collection("users")
    user = uc.get(acct)
    if user == None:
        uc.insert({
            "id": acct,
            "karma": "1",
            "about": "",
            "created": str(_now()),
            "submitted": [],
        })

    return respond(302, "", headers={
        "Location": "/news",
        "Set-Cookie": "user=" + token + "; Path=/; HttpOnly",
    })

def on_logout(req):
    return respond(302, "", headers={"Location": "/news"})
