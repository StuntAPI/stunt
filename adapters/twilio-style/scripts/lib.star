# Shared library for twilio-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# Twilio uses HTTP Basic auth with AccountSid as the username and AuthToken
# as the password. These are well-known synthetic test credentials for the
# local simulator.
ACCOUNT_SID = "AC" + "0123456789abcdef0123456789abcdef"
AUTH_TOKEN = "twilio_auth_token"

# _basic_auth extracts and validates HTTP Basic credentials.
#
# Returns the decoded "sid:token" pair as a list [sid, token], or None if
# the Authorization header is missing or not Basic auth.
def _basic_auth(req):
    headers = req.get("headers")
    if headers == None:
        return None
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth[:6] != "Basic ":
        return None
    encoded = auth[6:]
    # Decode base64 manually (Starlark has no built-in base64).
    decoded = _b64decode(encoded)
    if decoded == None:
        return None
    # Split on the first colon.
    idx = -1
    for i in range(len(decoded)):
        if decoded[i] == ":":
            idx = i
            break
    if idx < 0:
        return None
    sid = decoded[:idx]
    token = decoded[idx + 1:]
    return [sid, token]

# _require_auth validates HTTP Basic auth credentials (AccountSid:AuthToken).
#
# Returns None if authorized, or an error-response dict to return from the
# handler if not. Accepts the exact synthetic test credentials defined above.
def _require_auth(req):
    creds = _basic_auth(req)
    if creds == None:
        return respond(401, {
            "code": 20003,
            "message": "Missing or invalid Basic Auth credentials",
            "more_info": "https://www.twilio.com/docs/errors/20003",
            "status": 401,
        })
    sid = creds[0]
    token = creds[1]
    if sid != ACCOUNT_SID or token != AUTH_TOKEN:
        return respond(401, {
            "code": 20003,
            "message": "Invalid AccountSid or AuthToken",
            "more_info": "https://www.twilio.com/docs/errors/20003",
            "status": 401,
        })
    return None

# _next_sid generates a Twilio-style resource SID with a given prefix.
# Twilio SIDs are 34-char hex strings. We use a KV-backed sequence counter.
def _next_sid(prefix):
    seq = store_kv_incr("twilio", prefix + "_seq")
    # Pad to a realistic-looking 32-char hex suffix.
    return prefix + _pad_hex(seq)

# _pad_hex converts a number to a zero-padded lowercase hex string of
# at least 32 characters (to match Twilio's 34-char SID = 2-char prefix
# + 32-char hex body).
def _pad_hex(n):
    s = _to_hex(n)
    while len(s) < 32:
        s = "0" + s
    return s

# _to_hex converts a non-negative integer to a lowercase hex string.
def _to_hex(n):
    if n == 0:
        return "0"
    digits = "0123456789abcdef"
    s = ""
    while n > 0:
        s = digits[n % 16] + s
        n = n // 16
    return s

# _b64decode decodes a standard Base64 string to ASCII text.
# Starlark has no built-in base64, so we implement it.
def _b64decode(s):
    alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
    # Build reverse lookup: char -> index.
    lookup = {}
    for i in range(len(alphabet)):
        lookup[alphabet[i]] = i

    # Strip padding.
    s = s.replace("=", "")
    s = s.replace("\n", "")
    s = s.replace("\r", "")

    result = ""
    i = 0
    while i < len(s):
        # Each group of 4 base64 chars → 3 bytes.
        chunk = [0, 0, 0, 0]
        pad = 0
        for j in range(4):
            if i + j < len(s):
                ch = s[i + j]
                if ch not in lookup:
                    return None
                chunk[j] = lookup[ch]
            else:
                pad = pad + 1
                chunk[j] = 0

        b0 = (chunk[0] << 2) | (chunk[1] >> 4)
        b1 = ((chunk[1] & 0xF) << 4) | (chunk[2] >> 2)
        b2 = ((chunk[2] & 0x3) << 6) | chunk[3]

        result = result + chr(b0)
        if pad < 2:
            result = result + chr(b1)
        if pad < 1:
            result = result + chr(b2)
        i = i + 4

    return result

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
            return n
    return n
