# OAuth2 PKCE handlers — X Articles confidential-client authorization-code flow.
#
# Faithful port of a reference X client OAuth surface:
#
#   GET  /2/oauth2/authorize  -> 302 redirect with code+state
#   POST /2/oauth2/token      -> { token_type, access_token, refresh_token, expires_in, scope }
#
# Requirements reproduced:
#   - Authorize requires redirect_uri + S256 code_challenge (else 400).
#   - Token requires HTTP Basic client creds (else 401 invalid_client).
#   - Token requires grant_type=authorization_code (else 400 unsupported_grant_type).
#   - Auth code is single-use (consumed on first exchange).
#   - redirect_uri at token must match the one at authorize (else 400 invalid_grant).
#   - code_verifier must be present (else 400 invalid_grant).
#
# PKCE S256 RELAXATION (documented):
#   The real X server verifies code_verifier by computing
#     expected = base64url_no_pad(sha256(code_verifier))
#   and comparing against the stored code_challenge. Starlark in stunt has no
#   crypto/hashing builtins (no sha256, no base64), so this mock CANNOT
#   perform that comparison. Instead it performs a RELAXED check: the
#   code_verifier must be PRESENT and non-empty (catching a client that
#   forgets to send it), but the cryptographic sha256 match is NOT verified.
#
#   This is acceptable for a pipeline double: the Python mock's own docstring
#   says it "validates the pipeline, not real authz." A real client that
#   generates a valid S256 pair will always pass (the verifier is present),
#   and a client that omits the verifier fails appropriately. The only gap is
#   that a deliberately-wrong-but-present verifier is accepted — acceptable
#   for local testing.

# Shared helper (_pad5) is preloaded from scripts/lib.star.

# --- adapter-specific helpers ---

def _is_basic(req):
    auth = req["headers"].get("Authorization", "")
    return auth[:6] == "Basic "

def _contains(s, substr):
    return s.find(substr) >= 0

# _rand_code generates a synthetic authorization code. Starlark has no
# crypto-secure RNG, so we use the KV sequence counter to produce a unique,
# non-guessable-enough-for-local-testing value prefixed with "code_".
def _rand_code():
    seq = store_kv_incr("xarticles", "code_seq")
    return "code_" + _pad5(seq)

# _rand_token generates a synthetic bearer/refresh token.
def _rand_token(prefix):
    seq = store_kv_incr("xarticles", prefix + "_seq")
    return prefix + "_" + _pad5(seq)

# --- handlers ---

# on_authorize handles the PKCE authorization-code redirect.
# GET /2/oauth2/authorize?redirect_uri=...&state=...&code_challenge=...&code_challenge_method=S256
def on_authorize(req):
    q = req["query"]
    redirect_uri = q.get("redirect_uri", "")
    state = q.get("state", "")
    challenge = q.get("code_challenge", "")
    method = q.get("code_challenge_method", "")

    if redirect_uri == "" or method != "S256" or challenge == "":
        return respond(400, {"error": "invalid_request", "detail": "need redirect_uri, S256 code_challenge"})

    code = _rand_code()

    cc = store_collection("oauth_codes")
    cc.insert({
        "id": code,
        "challenge": challenge,
        "redirect_uri": redirect_uri,
    })

    sep = "?"
    if _contains(redirect_uri, "?"):
        sep = "&"
    location = redirect_uri + sep + "code=" + code + "&state=" + state
    return respond(302, headers={"Location": location})

# on_token exchanges an authorization code for an access+refresh token pair.
# POST /2/oauth2/token (HTTP Basic client creds; form body)
def on_token(req):
    # Confidential client -> HTTP Basic creds.
    if not _is_basic(req):
        return respond(401, {"error": "invalid_client"})

    body = req["body"]
    if body == None:
        body = {}
    grant_type = body.get("grant_type", "")

    if grant_type != "authorization_code":
        return respond(400, {"error": "unsupported_grant_type"})

    code = body.get("code", "")

    # One-time use: consume the code.
    cc = store_collection("oauth_codes")
    entry = cc.get(code)
    if entry == None:
        return respond(400, {"error": "invalid_grant", "detail": "invalid/used code"})
    cc.delete(code)

    # redirect_uri must match the one presented at authorize.
    redirect_uri = body.get("redirect_uri", "")
    if redirect_uri != entry["redirect_uri"]:
        return respond(400, {"error": "invalid_grant", "detail": "redirect_uri mismatch"})

    # PKCE (relaxed — see module docstring): code_verifier must be present.
    verifier = body.get("code_verifier", "")
    if verifier == "":
        return respond(400, {"error": "invalid_grant", "detail": "PKCE verifier mismatch"})

    return respond(200, {
        "token_type": "bearer",
        "access_token": _rand_token("mock_access"),
        "refresh_token": _rand_token("mock_refresh"),
        "expires_in": 7200,
        "scope": "tweet.read tweet.write users.read offline.access",
    })
