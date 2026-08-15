# Shared library for aws-cognito-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ====================================================================
# Cognito error helpers
# ====================================================================

# _cognito_err returns a Cognito-shaped error envelope. Cognito uses
# {"__type": "ExceptionName", "message": "..."} for service API errors.
def _cognito_err(error_type, message):
    return respond(400, {
        "__type": error_type,
        "message": message,
    })

# ====================================================================
# Auth helpers
# ====================================================================

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if absent.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _sigv4_check performs a STRUCTURAL validation of the SigV4 Authorization
# header. Returns None if valid (or if no header is present, since some
# service API calls are unauthenticated), or an error response if the
# header is malformed.
# NOTE: This is a structural check (presence of the SigV4 format), NOT
# a cryptographic signature verification. For a mock this is sufficient.
def _sigv4_check(req):
    auth = req["headers"].get("Authorization", "")
    if auth == None or auth == "":
        return None  # No auth header — will be handled per-endpoint.
    if auth[:18] != "AWS4-HMAC-SHA256 ":
        return _cognito_err("UnrecognizedClientException",
            "The AWS Access Key Id needs a subscription for the service")
    # Check for Credential, SignedHeaders, Signature components.
    body = auth[18:]
    if _find_substr(body, "Credential=") < 0:
        return _cognito_err("IncompleteSignatureException",
            "Authorization header requires 'Credential' parameter.")
    if _find_substr(body, "SignedHeaders=") < 0:
        return _cognito_err("IncompleteSignatureException",
            "Authorization header requires 'SignedHeaders' parameter.")
    if _find_substr(body, "Signature=") < 0:
        return _cognito_err("IncompleteSignatureException",
            "Authorization header requires 'Signature' parameter.")
    return None

# ====================================================================
# JWT minting + verification (REAL RS256 over a fixed synthetic RSA
# keypair; JWKS served at GET /{userPoolId}/.well-known/jwks.json)
# ====================================================================

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

# _mint_jwt builds a REAL RS256 JWT (header.payload.signature) signed with
# the fixed synthetic RSA keypair whose public half is served from the
# JWKS endpoint. token_use is "access" or "id"; the nonce (jti) keeps each
# mint unique. Claims mirror Cognito's documented access/id token set.
def _mint_jwt(sub, username, email, nonce, client_id, token_use):
    now = clock.now_unix()
    header = crypto.base64url_encode('{"alg":"RS256","kid":"' + _JWT_KID + '","typ":"JWT"}')
    if token_use == "access":
        payload_str = '{"sub":"' + sub + '","iss":"' + _JWT_ISS + '","client_id":"' + client_id + '","token_use":"access","username":"' + username + '","jti":"' + nonce + '","iat":' + str(now) + ',"exp":' + str(now + 3600) + '}'
    else:
        payload_str = '{"sub":"' + sub + '","iss":"' + _JWT_ISS + '","aud":"' + client_id + '","token_use":"id","cognito:username":"' + username + '","email":"' + email + '","email_verified":true,"auth_time":' + str(now) + ',"iat":' + str(now) + ',"exp":' + str(now + 3600) + '}'
    payload = crypto.base64url_encode(payload_str)
    signing_input = header + "." + payload
    sig = crypto.rsa_sign(_JWT_PRIVATE_KEY, signing_input, encoding="base64url")
    return signing_input + "." + sig

# _verify_jwt fully verifies an inbound JWT (access or id token):
#   - 3 dot-separated, base64url-valid segments
#   - JOSE header alg=="RS256"
#   - RSA-SHA256 signature over header.payload verified against the JWKS
#     public key (crypto.rsa_verify)
#   - exp in the future (clock.now_unix()) and iss == the pool issuer
#   - token_use == want_use ("access" or "id")
# Returns the claims dict, or None when the token fails any check.
def _verify_jwt(token, want_use):
    if token == "" or token == None:
        return None
    header = _jwt_json(token, 0)
    if header == None:
        return None
    if header.get("alg", "") != "RS256":
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
    if claims.get("token_use", "") != want_use:
        return None
    return claims

# Fixed synthetic RSA-2048 keypair used to sign Cognito tokens. The public
# half is served at GET /{userPoolId}/.well-known/jwks.json. Throwaway mock
# material — it exists nowhere but this repository.
_JWT_KID = "mock-cognito-key-1"
_JWT_ISS = "https://cognito-idp.mock-region.amazonaws.com/mock-user-pool"
_JWT_PRIVATE_KEY = """-----BEGIN RSA PRIVATE KEY-----
MIIEogIBAAKCAQEAuvUH4Lt/lv6L2u2E9qg15ZelZ7Olpmr7j9RhTr+3VybvKml5
dntEUSXKO70WVkZ36rjMecbOVU2vrCyJTpIYejqTp1c7O/67S7sdJPdLrKdL55JC
9r24Zp5gjUKW8ZoVn325oOsRlO4vezgkS93mSFGuq7yyXe36SmG/xwkFcB6eu2W1
IhuoHQkK47ja5PS1GuhctOKkYi9lqJPQYri6H3E7Cz1UHlQmdiY4T5porO1B9Dfe
9P77g6zw8jwzhkce0ORrWyvAbA5BFN5SodXaBN3G4PhjdklDPn++AXYhhahWGZy8
yVt81e+ra+jjHB7yutT5xDYzXY4JLF1FKsk4ywIDAQABAoIBAEL9Z8w8AxTcsspI
j3s+fMl+1BLbiUCfVvKLnC52fcBpwAsHbjFpK+qTyuoq7+UMLQ3bF9GOzgI86vSb
pLuVl9W8RYoRtLTjqsMREflb7y63Z3hbrUjyZC/JEjmroaCCoLrcdvZVJKCj1Dmn
vUG+CjThp9/7pkIH8sZSTkCIV/17LW03z07t3UpwPPbcwGfRb1GfYKtwPpeNyxtB
UvEM36jeYInSE+IKsP8fN6E/c+0Qn6xwZJsCkcB+y/V/QXiuaMTi9MU+TGtQ03eh
u4DMmoO5ITheuGLOGIqkhZ+OtlBxDbTnQsN1mvQ9zUiVMGqhAB1tagXnhVDpxm7I
eonU/0kCgYEAwlMpItTeGMrC79tKQl/NlDyxg6o9aqFZ5uFn905yh5AcRY8rEMS9
068HEJxh0xtmlsBhpWlaROeltHfUoPDTgI1F+4LijhfsAjMTLkCLIlm01GtkDSDw
0ZV3cNDmZMgEjSC2E5qSxNaAREltsVnMjYt1yQbGjsWI6MPqtmB0Xl8CgYEA9ks/
Nv5a9XRKqz9xsFsCZlTvV3fj63jFVP31IZS9UkZxq0alssBItEhsDpa+0s61z0ko
0Ggwl9V2Wa4Y3/79igROU1MJPbxfC7HH9+KdgANKpO7p8EHC2YsmlT9tbn+/eSbl
EqhoeculELQQzn3xbd5u7WBJqOJg0LvVhKM+ZRUCgYA9NLhGMknqASM5LRbMpSQ5
Roya7en+ReftIp3+dQT50dg1yIxF8dHgdMaC4t6lAYJkhR+8W9yEy3mTyBJ+xpu3
Z8fdGjKFkt9RKgkmjknEfgDIzzJqOC/hs3Q1YnbO03krglwW/J6xxOYNnBsiuygE
hSKKOModefZPajXpT6QXfQKBgEaqyHSK/qY2u8Xu6jvjoQijjhjWuXqyqEv+ofsE
pl2ZALxYBOsI6NNxhC+baR0rWlcjcqZ5fpfSE6cfoNuEWlLjcWXPCXPBPLQqSmoB
h5dXWm+AbXcWJ0Yr+uIP1OJDnTixxEBaOb/YgoAMalYVJNSVYdaSLhBbA9RgUJ9C
B4ERAoGAScLBJMcOVkQ/ulGbJKyjqis2aaQ0VBWPdQQ+OjBrRcG4oj7TUztxjy4F
KTSEsIsbZa2cq+a7wCImzCPikv8teaTGoOVnU1rxN61LG2G4IDwFGoj9/br0rkM7
7BVbYZiX+Z5DxA8ec/9pYP2TdF69zr8xT8Mm48gC6I/kYeRcGfo=
-----END RSA PRIVATE KEY-----"""
_JWT_PUBLIC_KEY = """-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAuvUH4Lt/lv6L2u2E9qg1
5ZelZ7Olpmr7j9RhTr+3VybvKml5dntEUSXKO70WVkZ36rjMecbOVU2vrCyJTpIY
ejqTp1c7O/67S7sdJPdLrKdL55JC9r24Zp5gjUKW8ZoVn325oOsRlO4vezgkS93m
SFGuq7yyXe36SmG/xwkFcB6eu2W1IhuoHQkK47ja5PS1GuhctOKkYi9lqJPQYri6
H3E7Cz1UHlQmdiY4T5porO1B9Dfe9P77g6zw8jwzhkce0ORrWyvAbA5BFN5SodXa
BN3G4PhjdklDPn++AXYhhahWGZy8yVt81e+ra+jjHB7yutT5xDYzXY4JLF1FKsk4
ywIDAQAB
-----END PUBLIC KEY-----"""

# ====================================================================
# String / parsing helpers
# ====================================================================

# _pad6 zero-pads a number to 6 digits.
def _pad6(n):
    s = str(n)
    while len(s) < 6:
        s = "0" + s
    return s

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

# _find_substr returns the index of the first occurrence of needle in s,
# or -1 if not found.
def _find_substr(s, needle):
    if len(needle) == 0:
        return 0
    for i in range(len(s) - len(needle) + 1):
        match = True
        for j in range(len(needle)):
            if s[i + j] != needle[j]:
                match = False
                break
        if match:
            return i
    return -1

# _strip removes leading and trailing whitespace.
def _strip(s):
    start = 0
    end = len(s)
    while start < end:
        ch = s[start]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            start = start + 1
        else:
            break
    while end > start:
        ch = s[end - 1]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            end = end - 1
        else:
            break
    return s[start:end]
