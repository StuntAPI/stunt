# OAuth2 handler — Salesforce token endpoint.
#
# POST /services/oauth2/token
#   (form: grant_type=password|authorization_code|refresh_token|
#          urn:ietf:params:oauth:grant-type:jwt-bearer,
#          client_id, client_secret, username, password, assertion)
#   -> { access_token:"00D...", instance_url, token_type:"Bearer",
#        id, issued_at, signature, refresh_token }
#
# Refresh semantics follow real Salesforce: refresh tokens are long-lived
# and REUSABLE — redeeming one never invalidates it — while access tokens
# rotate on every grant and expire after the session TTL (2h), after which
# the client refreshes again with the same refresh token. The refresh_token
# grant response omits refresh_token entirely (the caller keeps the one it
# has); password/code grants mint and return a new one. The JWT bearer
# grant (see the bottom of this file) issues an access token only — the
# client mints a fresh assertion instead of refreshing.

# Shared helpers from lib.star (_SESSION_TTL lives there).

def on_token(req):
    body = req["body"]
    if body == None:
        body = {}
    grant_type = body.get("grant_type", "")
    client_id = body.get("client_id", "")
    client_secret = body.get("client_secret", "")

    if grant_type == _JWT_GRANT:
        assertion = body.get("assertion", "")
        if assertion == "" or type(assertion) != "string":
            return _oauth_error("invalid_request", "assertion is required")
        claims = _verify_assertion(assertion)
        if claims == None:
            return _oauth_error("invalid_grant", "invalid assertion")
        username = _claim_str(claims.get("sub", None))
        if username == "":
            username = _claim_str(claims.get("prn", None))  # legacy claim
        return _issue_token(username, _claim_str(claims.get("iss", None)), None, False)

    if grant_type == "password":
        username = body.get("username", "")
        password = body.get("password", "")
        if username == "" or password == "" or client_id == "":
            return _oauth_error("invalid_request", "missing required parameters")
        return _issue_token(username, client_id)

    if grant_type == "authorization_code":
        code = body.get("code", "")
        if code == "":
            return _oauth_error("invalid_grant", "invalid or expired code")
        return _issue_token("user@mock.org", client_id)

    if grant_type == "refresh_token":
        refresh_token = body.get("refresh_token", "")
        username = store_kv_get("salesforce", "refresh_" + refresh_token)
        if refresh_token == "" or username == None:
            return _oauth_error("invalid_grant", "invalid refresh_token")
        # Reuse-safe: the refresh token stays valid for the next redemption;
        # only the access token rotates.
        return _issue_token(username, client_id, refresh_token)

    return _oauth_error("unsupported_grant_type", "grant_type not supported")

# _issue_token issues a Salesforce-style session token. `refresh` is the
# existing refresh token on a refresh grant (reused, not echoed) or None to
# mint a fresh one (password/code grants). `with_refresh=False` (JWT bearer
# grant) skips refresh tokens entirely.
def _issue_token(username, client_id, refresh=None, with_refresh=True):
    seq = store_kv_incr("salesforce", "token_seq")
    # Session IDs are 00D-prefixed (org key prefix).
    access = "00D" + _pad_b62(seq, 15)
    user_id = "005" + _pad_b62(1, 15)

    ac = store_collection("access_tokens")
    ac.insert({
        "id": access,
        "user_id": user_id,
        "username": username,
        "client_id": client_id,
        # Access tokens expire with the session; refresh tokens do not.
        "expires_at": clock.now_unix() + _SESSION_TTL,
    })

    issued_at = _epoch_ms()

    result = {
        "access_token": access,
        "instance_url": "https://mock-instance.my.salesforce.com",
        "id": "https://mock-instance.my.salesforce.com/id/00D" + ("0" * 12) + "EAA/" + user_id,
        "token_type": "Bearer",
        "issued_at": issued_at,
        "signature": "mock-signature-base64",
    }

    if with_refresh and (refresh == None or refresh == ""):
        refresh = "refresh_" + _pad_b62(seq, 25)
        store_kv_set("salesforce", "refresh_" + refresh, username)
        result["refresh_token"] = refresh

    return respond(200, result)

# _oauth_error returns an OAuth2 error response.
def _oauth_error(error, description):
    return respond(400, {
        "error": error,
        "error_description": description,
    })

# _epoch_ms returns the current time as epoch milliseconds (Salesforce's
# issued_at wire format) from the live clock.
def _epoch_ms():
    return str(clock.now_unix() * 1000)

# _pad_b62 encodes n in base-62, left-padded to width chars.
_B62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

def _pad_b62(n, width):
    if n <= 0:
        return "0" * width
    s = ""
    v = n
    while v > 0:
        s = _B62[v % 62] + s
        v = v // 62
    while len(s) < width:
        s = "0" + s
    return s

# ====================================================================
# JWT bearer grant (RFC 7523) — server-to-server auth the way real
# Salesforce connected apps do it: the client signs an RS256 assertion
# with a private key whose certificate is uploaded to the app; the token
# endpoint verifies the signature and claims, then issues a session.
# ====================================================================

_JWT_GRANT = "urn:ietf:params:oauth:grant-type:jwt-bearer"

# The assertion's aud must name a Salesforce login host (production or
# sandbox); anything else is a client misconfiguration.
_JWT_AUDS = ["https://login.salesforce.com", "https://test.salesforce.com"]

# The fixed synthetic public half of the "connected-app certificate" —
# the private half lives only in tests, mirroring how a real app's cert
# is registered out-of-band. Same throwaway repo material as the other
# JWT adapters. Assert with the matching key; anything else is rejected.
_JWT_PUBLIC_KEY = """-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAvBDsZejhK5crr0/kWSHt
hMSxv42QviE9IYlSQf9lZG4AjBymTX4q6UTuYoFnppDoLA0Llm2k8Ybj6GBpPFq1
DzRuOF0/Iee8+qB+FCJb1hA4O1FLBSoGHnyzx8PmvDth4LTKMgft9mtuozUe04WL
0Cf/cx96wjo4BeO72jYZDYOI2kpCH8lahdwYyykqnIdEALoTdIpCHd4P0cgBHz+s
S3UCPcBF1yt61vUJrKcoJCFqQ9oQ0t+aOHfIQpvoAgedjeJL9x2v9IUZN5lKfV2i
2ShaeiCTe8444oLHYHKh59tiMdL3DiJSdtMyrfJNT5+rvAqzYurkyR4eajWLTlxo
6wIDAQAB
-----END PUBLIC KEY-----"""

# _B64URL is the base64url alphabet (- and _ replace + and /).
_B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz" + "0123" + "45678" + "9-_"

# _b64url_ok reports whether seg is a syntactically valid unpadded
# base64url segment (alphabet chars only, length not == 1 mod 4). Guards
# the crypto.base64url_decode / crypto.rsa_verify builtins, which error
# (surfacing as a 500) on malformed input.
def _b64url_ok(seg):
    if seg == "":
        return False
    if len(seg) % 4 == 1:
        return False
    for i in range(len(seg)):
        if _B64URL.find(seg[i]) < 0:
            return False
    return True

# _jwt_json decodes a JWT segment (0=header, 1=payload) into a Starlark
# dict via crypto.base64url_decode + json_safe_decode, or None when
# malformed. Shape guards keep the decoders from erroring on garbage.
def _jwt_json(token, seg):
    parts = token.split(".")
    if len(parts) != 3:
        return None
    if not _b64url_ok(parts[0]) or not _b64url_ok(parts[1]) or not _b64url_ok(parts[2]):
        return None
    txt = crypto.base64url_decode(parts[seg])
    if txt == "" or txt[:1] != "{":
        return None
    out = json_safe_decode(txt)
    if type(out) != "dict":
        return None
    return out

# _claim_int coerces a claim value to int (JSON numbers decode as int).
# Returns None when absent or non-numeric.
def _claim_int(v):
    if v == None:
        return None
    if type(v) == "int":
        return v
    return None

# _claim_str coerces a claim value to string. JSON null decodes to None,
# and None == "" is False in Starlark — so a `sub: null` would sail past
# a plain `get(...) == ""` guard. Type-strict instead.
def _claim_str(v):
    if v == None:
        return ""
    if type(v) == "string":
        return v
    return ""

# _verify_assertion fully verifies a jwt-bearer assertion the way the real
# token endpoint does (modulo the fixed mock certificate):
#   - 3 dot-separated, base64url-valid segments
#   - JOSE header alg=="RS256"
#   - RSA-SHA256 signature over header.payload verified against the mock
#     connected-app certificate
#   - iss non-empty (the consumer key), sub or legacy prn non-empty (the
#     user the token is for), aud a Salesforce login host, exp in the
#     future and no more than ~5 minutes out (the real window, plus the
#     documented clock-skew allowance)
# Returns the claims dict, or None when the assertion fails any check.
def _verify_assertion(assertion):
    header = _jwt_json(assertion, 0)
    if header == None:
        return None
    if header.get("alg", "") != "RS256":
        return None
    parts = assertion.split(".")
    if not crypto.rsa_verify(_JWT_PUBLIC_KEY, parts[0] + "." + parts[1], parts[2], encoding="base64url"):
        return None
    claims = _jwt_json(assertion, 1)
    if claims == None:
        return None
    if _claim_str(claims.get("iss", None)) == "":
        return None
    if _claim_str(claims.get("sub", None)) == "" and _claim_str(claims.get("prn", None)) == "":
        return None
    exp = _claim_int(claims.get("exp", None))
    if exp == None:
        return None
    if clock.now_unix() >= exp:
        return None
    # Real Salesforce rejects assertions whose exp is more than ~5
    # minutes out (plus the documented clock-skew allowance) — clients
    # can't mint long-lived JWTs.
    if exp > clock.now_unix() + 480:
        return None
    if claims.get("aud", "") not in _JWT_AUDS:
        return None
    return claims
