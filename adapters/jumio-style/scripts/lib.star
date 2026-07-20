# Shared library for jumio-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from "Authorization: Bearer <t>".
# Jumio uses HTTP Basic Auth in production, but we accept Bearer for
# simplicity in local testing.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return None

# _require_auth checks for a valid Bearer header.
def _require_auth(req):
    tok = _bearer(req)
    if tok == None or tok == "":
        return False
    return True

# _err returns a Jumio-style error body.
def _err(http_status, message):
    return {"httpStatus": http_status, "message": message}

# _gen_scan_ref generates a synthetic Jumio scan reference (UUID-like).
def _gen_scan_ref(seq):
    s = str(0x10000000 + seq)
    return s + "-0000-4000-8000-" + str(seq * 1000 + 100000000000)
