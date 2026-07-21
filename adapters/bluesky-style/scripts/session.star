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
