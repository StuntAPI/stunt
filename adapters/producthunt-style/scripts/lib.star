# Shared library for producthunt-style adapter scripts.
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

# --- credential store ------------------------------------------------------
#
# Known tokens live in the "producthunt" KV namespace under "tok:<token>"
# with the expiry as unix seconds. Product Hunt's server-to-server tokens
# have no fixed short TTL, so the seeded test token gets a far-future
# expiry computed at runtime (never a hardcoded epoch).

# Well-known static test token, seeded once on first request (see
# _seed_tokens) so existing clients/tests that use it keep working while
# any other token is rejected with 401.
_TEST_TOKEN = "mock-token-1"

# _seed_tokens inserts the well-known static test token into the KV store
# exactly once per instance (guarded by the "auth_seeded" flag).
def _seed_tokens():
    if store_kv_get("producthunt", "auth_seeded") == "yes":
        return
    store_kv_set("producthunt", "auth_seeded", "yes")
    store_kv_set("producthunt", "tok:" + _TEST_TOKEN, str(clock.now_unix() + 3600 * 24 * 365 * 10))

# _bearer_valid reports whether the request carries a known, unexpired
# Bearer token.
def _bearer_valid(req):
    token = _bearer(req)
    if token == "":
        return False
    _seed_tokens()
    exp = store_kv_get("producthunt", "tok:" + token)
    return exp != None and clock.now_unix() <= _to_int(exp)

# _to_int parses a decimal string to int. Returns 0 for None or empty.
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
