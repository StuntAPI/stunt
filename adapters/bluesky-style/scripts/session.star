# Session handler — createSession.
#
# POST /xrpc/com.atproto.server.createSession
#   body: { identifier, password }
#   -> 200 { accessJwt, refreshJwt, did, handle, email }
#
# A fresh session is minted per createSession call (the reference client adapter does
# this — app passwords don't expire, so caching isn't needed). The
# accessJwt is an opaque token stored in the sessions collection and
# validated as a Bearer token on subsequent requests.

# Shared helpers (_mint_did, _mint_jwt, _mint_refresh, _pad12) are preloaded
# from scripts/lib.star.

# on_create_session mints a session for the given identifier + password.
def on_create_session(req):
    body = req["body"]
    if body == None:
        body = {}
    identifier = body.get("identifier", "")
    password = body.get("password", "")

    if identifier == "" or password == "":
        return respond(400, {
            "error": "InvalidRequest",
            "message": "identifier and password are required",
        })

    seq = store_kv_incr("bluesky", "account_seq")
    did = _mint_did(seq)
    handle = identifier
    access = _mint_jwt(seq)
    refresh = _mint_refresh(seq)

    sc = store_collection("sessions")
    sc.insert({
        "id": access,
        "did": did,
        "handle": handle,
        "refresh": refresh,
    })

    return respond(200, {
        "did": did,
        "handle": handle,
        "accessJwt": access,
        "refreshJwt": refresh,
        "email": "user" + str(seq) + "@example.test",
    })

# on_refresh_session: POST /xrpc/com.atproto.server.refreshSession with the
# refreshJwt as the Bearer token -> a fresh access/refresh pair (the old
# session is rotated). Makes the refreshJwt usable instead of decorative.
def on_refresh_session(req):
    auth = req["headers"].get("Authorization", "")
    if auth == None:
        auth = ""
    presented = ""
    if auth[:7] == "Bearer ":
        presented = auth[7:]
    if presented == "":
        return respond(401, {"error": "InvalidToken", "message": "refreshJwt required as Bearer"})

    sc = store_collection("sessions")
    session = None
    for s in sc.list():
        if s.get("refresh") == presented:
            session = s
            break
    if session == None:
        return respond(401, {"error": "InvalidToken", "message": "invalid refreshJwt"})

    did = session["did"]
    handle = session["handle"]
    old_id = session["id"]
    seq = store_kv_incr("bluesky", "account_seq")
    new_access = _mint_jwt(seq)
    new_refresh = _mint_refresh(seq)

    # Rotate: drop the old session, store a new one keyed by the new accessJwt.
    sc.delete(old_id)
    sc.insert({"id": new_access, "did": did, "handle": handle, "refresh": new_refresh})

    return respond(200, {
        "did": did,
        "handle": handle,
        "accessJwt": new_access,
        "refreshJwt": new_refresh,
    })
