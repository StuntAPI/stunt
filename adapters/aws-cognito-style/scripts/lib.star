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
# JWT minting (synthetic, for access_token / id_token)
# ====================================================================

# _b64url mimics a base64url-encode of a string (synthetic — not real
# base64, but deterministic and URL-safe). Used to produce JWT-shaped
# token segments.
def _b64url(s):
    out = ""
    for i in range(len(s)):
        ch = s[i]
        n = ord(ch)
        v = ((n - 32) * 3 + 1) % 63
        if v < 26:
            out += chr(v + 65)
        elif v < 52:
            out += chr(v - 26 + 97)
        elif v < 62:
            out += chr(v - 52 + 48)
        else:
            out += "_"
    return out

# _mint_jwt builds a synthetic JWT-shaped token (header.payload.sig).
# The payload encodes the username and sub. The nonce ensures uniqueness.
def _mint_jwt(sub, username, email, nonce="0"):
    header = _b64url('{"alg":"RS256","typ":"JWT","kid":"mock-key-id"}')
    payload_str = '{"sub":"' + sub + '","username":"' + username + '","email":"' + email + '","nonce":"' + nonce + '","iss":"https://cognito-idp.mock-region.amazonaws.com/mock-user-pool","token_use":"id","aud":"mock-client-id","auth_time":1718448000}'
    payload = _b64url(payload_str)
    sig = _b64url("mock-signature-" + sub + "-" + nonce)
    return header + "." + payload + "." + sig

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
