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
# token. Returns None if the token is absent or not found in the store.
def _user_for_token(req):
    token = _bearer(req)
    if token == "":
        return None
    c = store_collection("tokens")
    doc = c.get(token)
    if doc == None:
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

# _mint_jwt builds a synthetic JWT-shaped access token (header.payload.sig).
# The payload encodes the user id and scopes so downstream handlers can
# validate it. The nonce (typically the access_seq) ensures each token is
# unique even when refreshing for the same user.
def _mint_jwt(sub, scopes, name, nonce="0"):
    header = _b64url('{"alg":"RS256","typ":"JWT"}')
    payload_parts = '{"sub":"' + sub + '","name":"' + name + '","scp":"' + scopes + '","nonce":"' + nonce + '","iss":"https://login.microsoftonline.com/mock-tenant/v2.0"}'
    payload = _b64url(payload_parts)
    sig = _b64url("mock-signature-" + sub + "-" + scopes + "-" + nonce)
    return header + "." + payload + "." + sig
