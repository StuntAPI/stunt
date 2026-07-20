# Shared library for cloudflare-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ====================================================================
# Auth validation (structural)
# ====================================================================
# Cloudflare accepts two auth schemes:
#
#   1. Scoped API token — Authorization: Bearer <api_token>
#      Tokens can be scoped to specific resources/permissions. For v1 we
#      accept any non-empty bearer token (structural check only).
#
#   2. Global API key   — X-Auth-Email + X-Auth-Key headers
#      The X-Auth-Key is the account's Global API Key. We validate both
#      headers are present and non-empty.

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        return ""
    if _has_prefix(auth, "Bearer "):
        return auth[7:]
    return ""

# _cf_err returns a Cloudflare-style error response envelope.
# Cloudflare always wraps responses in {success, errors, messages, result}.
def _cf_err(status_code, error_code, message):
    return respond(status_code, {
        "success": False,
        "errors": [{"code": error_code, "message": message}],
        "messages": [],
        "result": None,
    })

# _cf_ok returns a Cloudflare-style success response envelope.
def _cf_ok(result):
    return respond(200, {
        "success": True,
        "errors": [],
        "messages": [],
        "result": result,
    })

# _cf_ok_with_info returns a success response with result_info (pagination).
def _cf_ok_with_info(result_list, count):
    return respond(200, {
        "success": True,
        "errors": [],
        "messages": [],
        "result": result_list,
        "result_info": {
            "page": 1,
            "per_page": 20,
            "total_count": count,
        },
    })

# _require_auth returns None if authorized, or a Cloudflare-style 401 error.
def _require_auth(req):
    # Check for Bearer token
    token = _bearer(req)
    if token != "":
        return None

    # Check for X-Auth-Email + X-Auth-Key
    headers = req.get("headers")
    if headers != None:
        email = headers.get("X-Auth-Email", "")
        key = headers.get("X-Auth-Key", "")
        if (email != None and email != "") and (key != None and key != ""):
            return None

    return _cf_err(401, 10000, "Authentication error")

# ====================================================================
# Helpers
# ====================================================================

# _has_prefix returns True if s starts with prefix.
def _has_prefix(s, prefix):
    if len(s) < len(prefix):
        return False
    return s[:len(prefix)] == prefix

# _find_substr returns the index of the first occurrence of needle in s,
# or -1 if not found.
def _find_substr(s, needle):
    if len(needle) == 0:
        return 0
    for i in range(len(s) - len(needle) + 1):
        match = True
        for j in range(len(needle)):
            if s[i+j] != needle[j]:
                match = False
                break
        if match:
            return i
    return -1

# _to_int parses a decimal string to int. Returns 0 on failure.
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

# ====================================================================
# ID generators
# ====================================================================

# _gen_id generates a synthetic 32-char hex ID (like Cloudflare zone/account IDs).
def _gen_id(ns):
    n = store_kv_incr("cf", ns + "_id_seq")
    hex = ""
    v = n * 2654435761 + 0xCF000000
    for i in range(32):
        rem = v % 16
        if rem < 10:
            hex = chr(ord("0") + rem) + hex
        else:
            hex = chr(ord("a") + rem - 10) + hex
        v = v // 16
        if v == 0:
            v = n * 17 + i + 7
    # Pad to 32 chars
    while len(hex) < 32:
        hex = "0" + hex
    return hex[:32]

# _gen_uuid generates a synthetic UUID-like string (for D1 databases).
def _gen_uuid():
    n = store_kv_incr("cf", "uuid_seq")
    hex = ""
    v = n * 2654435761 + 0xABCD0000
    for i in range(32):
        rem = v % 16
        if rem < 10:
            hex = chr(ord("0") + rem) + hex
        else:
            hex = chr(ord("a") + rem - 10) + hex
        v = v // 16
        if v == 0:
            v = n * 31 + i + 3
    while len(hex) < 32:
        hex = "0" + hex
    # Insert UUID dashes: 8-4-4-4-12
    h = hex[:32]
    return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]

# _iso8601 returns a synthetic ISO 8601 timestamp.
def _iso8601():
    return "2024-01-01T00:00:00.000000Z"
