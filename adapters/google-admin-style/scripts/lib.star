# Shared library for google-admin-style adapter scripts.
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

# _require_bearer returns a dummy user dict for a valid Bearer token, or a
# 401 response if missing. Google Workspace requires a super-admin OAuth2
# token — we accept any non-empty bearer (the mock models the gate).
def _require_bearer(req):
    token = _bearer(req)
    if token == "":
        return None, respond(401, {
            "error": {
                "code": 401,
                "message": "Login Required.",
                "errors": [{
                    "message": "Login Required.",
                    "domain": "global",
                    "reason": "required",
                }],
            },
        })
    return {"token": token}, None

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

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

# _pad10 zero-pads a number to 10+ digits (Google user IDs are large ints).
def _pad10(n):
    s = str(n)
    while len(s) < 11:
        s = "0" + s
    return s

# _not_found returns a Google-style 404 error response body.
def _not_found(kind, key):
    return {
        "error": {
            "code": 404,
            "message": kind + " not found: " + key,
            "errors": [{
                "message": kind + " not found: " + key,
                "domain": "global",
                "reason": "notFound",
            }],
        },
    }
