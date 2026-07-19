# Shared library for photos-style adapter scripts.
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

# _user_for_token looks up the user document bound to a Bearer token.
# Returns None if the token is absent or not found in the store.
def _user_for_token(req):
    token = _bearer(req)
    if token == "":
        return None
    c = store_collection("tokens")
    doc = c.get(token)
    if doc == None:
        return None
    return doc

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

# _to_num converts a value to int, handling JSON numbers (float) and strings.
# Returns the default if the value is None, empty, or non-numeric.
def _to_num(v, default=0):
    if v == None:
        return default
    # Starlark numbers from JSON floats are int or float depending on value.
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    # String path.
    s = v
    if s == "":
        return default
    return _to_int(s)

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0
