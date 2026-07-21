# OAuth2 handler — Reddit-style token endpoint.
#
# POST /api/v1/access_token  (HTTP Basic client creds; form body)
#
#   grant_type=authorization_code + duration=permanent -> access + refresh
#   grant_type=refresh_token                           -> access ONLY (no new refresh)
#
# Faithful behaviors ported from a reference client's Reddit mock:
#   - Rejects requests without a descriptive User-Agent (429).
#   - Requires HTTP Basic client credentials (401 invalid_client otherwise).
#   - refresh_token grant returns NO new refresh_token (Reddit only issues
#     one on the initial permanent authorization-code grant).

# Shared helpers (_has_ua, _ua_rejected) are preloaded from scripts/lib.star.

# --- adapter-specific helpers (not shared) ---

def _is_basic(req):
    auth = req["headers"].get("Authorization", "")
    return auth[:6] == "Basic "

# --- handlers ---

# on_access_token handles authorization_code and refresh_token grants.
def on_access_token(req):
    if not _has_ua(req):
        return _ua_rejected(req)

    # Client creds via HTTP Basic (real Reddit requirement).
    if not _is_basic(req):
        return respond(401, {"error": "invalid_client"})

    body = req["body"]
    if body == None:
        body = {}
    grant = body.get("grant_type", "")

    if grant == "refresh_token":
        presented = body.get("refresh_token", "")
        rc = store_collection("refresh_tokens")
        if rc.get(presented) == None:
            return respond(400, {"error": "invalid_grant"})
        access = _mint_access()
        # NB: no new refresh_token on a refresh grant (matches real Reddit).
        return respond(200, {
            "access_token": access,
            "token_type": "bearer",
            "expires_in": 3600,
            "scope": "submit identity",
        })

    if grant == "authorization_code":
        access = _mint_access()
        out = {
            "access_token": access,
            "token_type": "bearer",
            "expires_in": 3600,
            "scope": "submit identity",
        }
        # duration=permanent -> also issue a refresh_token.
        if body.get("duration") == "permanent":
            refresh = _mint_refresh()
            out["refresh_token"] = refresh
        return respond(200, out)

    return respond(400, {"error": "unsupported_grant_type"})

def _mint_access():
    seq = store_kv_incr("reddit", "access_seq")
    token = "rdtok_" + str(seq)
    tc = store_collection("tokens")
    tc.insert({"id": token})
    return token

def _mint_refresh():
    seq = store_kv_incr("reddit", "refresh_seq")
    token = "rdref_" + str(seq)
    rc = store_collection("refresh_tokens")
    rc.insert({"id": token})
    return token
