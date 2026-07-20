# Shared library for onfido-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _token extracts the token from an "Authorization: Token <t>" header.
# Onfido uses the "Token" prefix (not "Bearer").
def _token(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:6] == "Token ":
        return auth[6:]
    return None

# _require_auth checks for a valid Token auth header.
def _require_auth(req):
    tok = _token(req)
    if tok == None or tok == "":
        return False
    return True

# _err returns an Onfido-style error response body.
def _err(error_type, message, fields):
    e = {"type": error_type, "message": message}
    if fields != None:
        e["fields"] = fields
    return {"error": e}

# _gen_id generates a synthetic ID with a given prefix.
def _gen_id(prefix, seq):
    s = str(seq)
    while len(s) < 6:
        s = "0" + s
    return prefix + "-" + s

# _advance_check_status advances a check through in_progress→complete.
def _advance_check_status(current):
    if current == "in_progress":
        return "complete"
    return "complete"
