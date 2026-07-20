# Shared library for sendgrid-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        return ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _require_auth validates that a non-empty bearer key is present.
# SendGrid API keys have the format "SG.<...>".
# Returns None if authorized, or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return respond(401, {
            "errors": [
                {
                    "message": "The provided authorization grant is invalid, expired, or revoked.",
                    "field": None,
                    "help": None,
                }
            ],
        })
    return None

# _next_msg_id returns a monotonically-increasing SendGrid-style message ID
# using the KV store as a sequence counter.
def _next_msg_id():
    n = store_kv_incr("sendgrid", "msg_seq")
    return "msg_" + str(n) + "@stunt.local"

# _now_iso returns a synthetic ISO-8601 timestamp (stable for determinism).
def _now_iso():
    return "2024-01-15T12:00:00Z"

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input.
def _to_int(s):
    if s == None:
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return n
    return n

# _to_int_str converts a value (possibly float from JSON round-trip) to
# an integer string.
def _to_int_str(val):
    if val == None:
        return "0"
    s = str(val)
    dot = _find_dot(s)
    if dot > 0:
        return s[:dot]
    return s

def _find_dot(s):
    for i in range(len(s)):
        if s[i] == ".":
            return i
    return -1

# _extract_emails extracts email addresses from a personalizations structure.
# Returns a flat list of {email: "..."} dicts.
def _extract_emails(personalization_list):
    result = []
    if personalization_list == None:
        return result
    for p in personalization_list:
        to_list = p.get("to", [])
        if to_list == None:
            to_list = []
        for addr in to_list:
            email = addr.get("email", "")
            if email != None and email != "":
                result.append({"email": email})
    return result
