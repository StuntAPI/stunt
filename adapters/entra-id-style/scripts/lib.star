# Shared library for entra-id-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _user_for_token looks up the user/principal document bound to a Bearer
# token. Returns None if the token is absent, not found in the store, or
# expired (expires_at, a unix timestamp set at mint time — the access-token
# TTL reported by the real identity platform is ~3600s). The JWT itself is
# also VERIFIED cryptographically (RS256 signature over header.payload with
# the JWKS public key, plus exp/iss/aud claim checks) — not just a store
# lookup — so forged or claim-tampered tokens are rejected even if a store
# row existed.
def _user_for_token(req):
    token = _bearer(req)
    if token == "":
        return None
    claims = _verify_access_jwt(token)
    if claims == None:
        return None
    c = store_collection("tokens")
    doc = c.get(token)
    if doc == None:
        return None
    exp = doc.get("expires_at", 0)
    if exp != None and exp != 0 and clock.now_unix() > exp:
        return None
    # Cross-check the token's aud against the client_id recorded at mint
    # time (defends against a minted token being replayed against another
    # API audience).
    want_aud = doc.get("client_id", "")
    if want_aud != "" and claims.get("aud", "") != want_aud:
        return None
    return doc

# _require_bearer returns the user doc for the Bearer token, or a 401
# response if missing/invalid. Handlers call this at the top to guard
# protected endpoints.
def _require_bearer(req):
    user = _user_for_token(req)
    if user == None:
        return None, respond(401, {
            "error": {
                "code": "InvalidAuthenticationToken",
                "message": "Access token is missing or invalid.",
            },
        })
    return user, None

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
def _to_int(s):
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

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _get_query safely returns a query parameter value.
def _get_query(req, key, default_val):
    query = req.get("query")
    if query == None:
        query = {}
    v = query.get(key, default_val)
    if v == None:
        v = default_val
    return v

# _list_page reads Microsoft Graph OData $top (page size) / $skipToken
# (opaque cursor) query params, slices the already-filtered docs via the
# paginate() builtin, and returns (page, next_link) where next_link is an
# @odata.nextLink URL string the client can follow to round-trip
# $top/$skipToken, or None when there is no further page. Paging is DISABLED
# (whole list returned, next_link None) when $top is missing or <= 0 —
# preserving prior unpaginated behavior. base_path is the route used to build
# the next-link URL.
def _list_page(req, docs, base_path):
    top = _to_int(_get_query(req, "$top", ""))
    skip_token = _get_query(req, "$skipToken", "")
    if skip_token == None:
        skip_token = ""

    page, next_cursor = paginate(docs, top, skip_token)

    next_link = None
    if next_cursor != None:
        # Real Graph serves ABSOLUTE @odata.nextLink URLs on the graph host —
        # graph clients (microsoft-graph-client PageIterator) parse host and
        # version straight out of them via Client.api(nextLink).
        next_link = "https://graph.microsoft.com" + base_path + "?$top=" + str(top) + "&$skipToken=" + next_cursor
    return page, next_link

# _pad3 zero-pads a number to 3 digits.
def _pad3(n):
    if n < 10:
        return "00" + str(n)
    if n < 100:
        return "0" + str(n)
    return str(n)

# _pad6 zero-pads a number to 6 digits.
def _pad6(n):
    s = str(n)
    while len(s) < 6:
        s = "0" + s
    return s

# _b64url mimics a base64url-encode of a string (synthetic — not real
# base64, but deterministic and URL-safe). Used to produce JWT-shaped token
# segments.
def _b64url(s):
    # Simple substitution-based encoding for deterministic URL-safe output.
    # This is NOT real base64 — it's a synthetic representation for mock
    # tokens that looks structurally like a JWT.
    out = ""
    for i in range(len(s)):
        ch = s[i]
        n = ord(ch)
        # Shift into printable ASCII range A-z,0-9
        v = ((n - 32) * 3 + 1) % 63
        if v < 26:
            out += chr(v + 65)       # A-Z
        elif v < 52:
            out += chr(v - 26 + 97)  # a-z
        elif v < 62:
            out += chr(v - 52 + 48)  # 0-9
        else:
            out += "_"
    return out

# _mint_jwt builds a real RS256-signed access token (header.payload.sig).
# The payload encodes the user id, scopes, issuer, audience (client_id) and
# iat/exp (1h TTL) so downstream verifiers can validate it with the JWKS at
# /common/discovery/v2.0/keys. The nonce (typically the access_seq) ensures
# each token is unique even when refreshing for the same user.
def _mint_jwt(sub, scopes, name, nonce, aud):
    now = clock.now_unix()
    header = crypto.base64url_encode('{"alg":"RS256","typ":"JWT","kid":"' + _JWT_KID + '"}')
    payload_parts = '{"sub":"' + sub + '","name":"' + name + '","scp":"' + scopes + '","nonce":"' + nonce + '","iss":"' + _JWT_ISS + '","aud":"' + aud + '","iat":' + str(now) + ',"exp":' + str(now + 3599) + '}'
    payload = crypto.base64url_encode(payload_parts)
    signing_input = header + "." + payload
    sig = crypto.rsa_sign(_JWT_PRIVATE_KEY, signing_input, encoding="base64url")
    return signing_input + "." + sig

# --- inbound JWT verification ---

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
# dict via crypto.base64url_decode + json.decode, or None when malformed.
# Shape guards keep json.decode from erroring on garbage.
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
# Returns None when absent/None.
def _claim_int(v):
    if v == None:
        return None
    if type(v) == "int":
        return v
    return None

# _verify_access_jwt fully verifies an inbound access token:
#   - 3 dot-separated, base64url-valid segments
#   - JOSE header alg=="RS256" and kid == the served JWKS kid
#   - RSA-SHA256 signature over header.payload verified with the JWKS
#     public key (crypto.rsa_verify)
#   - exp in the future (clock.now_unix()), iss == the mock tenant issuer,
#     aud present and non-empty
# Returns the claims dict, or None when the token fails any check.
def _verify_access_jwt(token):
    if token == "" or token == None:
        return None
    header = _jwt_json(token, 0)
    if header == None:
        return None
    if header.get("alg", "") != "RS256":
        return None
    if header.get("kid", "") != _JWT_KID:
        return None
    parts = token.split(".")
    if not crypto.rsa_verify(_JWT_PUBLIC_KEY, parts[0] + "." + parts[1], parts[2], encoding="base64url"):
        return None
    claims = _jwt_json(token, 1)
    if claims == None:
        return None
    exp = _claim_int(claims.get("exp", None))
    if exp == None:
        return None
    if clock.now_unix() >= exp:
        return None
    if claims.get("iss", "") != _JWT_ISS:
        return None
    if claims.get("aud", "") == "":
        return None
    return claims
_JWT_KID = "mock-entra-kid-1"
_JWT_ISS = "https://login.microsoftonline.com/mock-tenant/v2.0"
_JWT_PRIVATE_KEY = """-----BEGIN PRIVATE KEY-----
MIIEowIBAAKCAQEAvr9/xXKqJmLa53C2IDf3DUu83XmYEwqLbxD62LJg7x+aLJ1d
1uFHDiCphI5ab6d1fEx6BKa0CVJ9VG74+Fg5vM07SgztJgWvHpFGDM4lB5v/BCe4
5sfObjMkPkRCcBAvZoPmRt3OilX8pmbABpCAjBiaT/nb6O7a85VfrYzif7iYpDWq
pymZgCS6PO/o9ju/UHqwhB2EzjZdNZGiwPfn2dEri50WCoKWtXSJEG3GhsBueCRG
5jzbhIGtPP7XjnZCf79ISr2V44OahuK0LxrFYV9d3r7sCGj0mtH5ExXUjlX9yIKq
WDwHr2RuadNFzE8AjkyKyH7Xgc06y5t2/9cAoQIDAQABAoIBAA77NOKR1y3FIVrA
ljlRE/ECJA8EBA7oyts6Cu2Wkvjs8zuyT2K3VlCUfaPo108CKL7Otd2kJys9RJUr
UxgUO9KpjtDJ051jIGYm9EjArxVaKe0OXp4Xjs3GbAAM9efdyY9EaENkG9rvFnUO
SGIrmsEGFKaX4e75RY6Qio9zq31q5B3Iw8u8hWe0/EdcFSLdRojmRfNb5KrIrF2U
V/TwWkaY77QrctX2uXMliq2p4vE1jQunCaiMM6hLHhu3lBLXq/OWtPgB0fW+Gugh
jQODvW2/Oo/3eA3Ie25mg1c1cEhqXwxh2cgB4vmes7VEvLMS8IzAQfQrvNWtmL0Z
l49IY/ECgYEA7GVcGjnqYqYeOsW6u0nooXaKtp4oBwyAcA+fXTXo8mSXfyufgj6Q
k9KTTrKryJ6sOWIWGneBi7hatyiSVGCqjRTQIdfGm6NaKrsyKBub9ypIRcVtZgBh
4Z6HSio6fD7j3farJdSiLvYBByZGWnSbEC8Nt0APJriDR92+vpwNwVkCgYEAzpEL
GLa9bEfolTUqiQbvMOQntkbNJGX+iE9NpWGpzIpqFo32ltjpLNT6rSH11eokrpRR
E7KKT+86DVOqWzIV7IlNWJJVVueElOYdFlZG99HFhnoG9PdAq14sbqP8AlAVrjyN
0HEqal+oMiK3KJnbOUMXpkoyCxwZ/u4xoqY1yIkCgYAYN8IZxbknZhFOwBcDPO0i
LXzEfKtpHXTDBjazW+SDgJ6snpF2zGYPXtFMjK1gnjDSqCPPjlKtN7PDc9qZ3lVa
ork33l0wcKm6GvdmeH2f8qr4yuMMQhnE/XKqvGzFccPyZ2TdOU1sNjOgweEPP0br
f4aOMXfb5ac9Y5A5As+98QKBgQCOfDIhS/wBcuCV+2RpvKTFHrvd2ZyrnMckEz/F
8kYD1v4yrJ4Jk3nT+N0pC6HdenLvEVOTuLX7SVLL2ohJ+5Rv4o29qMLA/VXQt6Ic
xEqTqtkLV6Tw2JR9IKqZbvfoSIGL/Cz+OPE/CtikLJoWoXo8V3E6vTcjvrCXzoni
Xa//sQKBgEzzVG6cFZ3heWNs1aw0iLWkiyNVy1bMGHG0uCHdkUooBVVDhePuFJjC
d1e1sGLm/t1FsCtnZfjUaz4UDBtZIuzsa8l+6V4oYonWEL4za2S28BZEOKfI0I5d
CbHMqyTew/B29KnXJaLkcOiQ4NFgjOQ/fMjzckrXedZp7Za6WZEg
-----END PRIVATE KEY-----"""
_JWT_PUBLIC_KEY = """-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAvr9/xXKqJmLa53C2IDf3
DUu83XmYEwqLbxD62LJg7x+aLJ1d1uFHDiCphI5ab6d1fEx6BKa0CVJ9VG74+Fg5
vM07SgztJgWvHpFGDM4lB5v/BCe45sfObjMkPkRCcBAvZoPmRt3OilX8pmbABpCA
jBiaT/nb6O7a85VfrYzif7iYpDWqpymZgCS6PO/o9ju/UHqwhB2EzjZdNZGiwPfn
2dEri50WCoKWtXSJEG3GhsBueCRG5jzbhIGtPP7XjnZCf79ISr2V44OahuK0LxrF
YV9d3r7sCGj0mtH5ExXUjlX9yIKqWDwHr2RuadNFzE8AjkyKyH7Xgc06y5t2/9cA
oQIDAQAB
-----END PUBLIC KEY-----"""
