# Shared library for bluesky-style adapter scripts.
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

# _did_for_token looks up the DID bound to a Bearer accessJwt.
# Returns "" if the token is absent or not found in the sessions store.
def _did_for_token(req):
    token = _bearer(req)
    if token == "":
        return ""
    sc = store_collection("sessions")
    doc = sc.get(token)
    if doc == None:
        return ""
    return doc.get("did", "")

# _mint_did generates a deterministic synthetic DID (did:plc:<rkey>).
def _mint_did(seq):
    return "did:plc:" + _pad12(seq)

# _mint_cid generates a synthetic CID (bafyrei...) — a short opaque tag,
# NOT a real base32 multihash, so it passes lint.
def _mint_cid(seq):
    return "bafyrei" + _pad12(seq) + "mock"

# _mint_jwt generates a synthetic opaque access token. We deliberately do
# NOT produce a real JWT (eyJ...) shape so lint passes — it's just an opaque
# string the client echoes back as a Bearer token.
def _mint_jwt(seq):
    return "mock_access_jwt_" + str(seq)

# _mint_refresh generates a synthetic refresh token.
def _mint_refresh(seq):
    return "mock_refresh_jwt_" + str(seq)

# _pad12 left-pads n to 12 digits for a realistic-length DID rkey.
def _pad12(n):
    s = str(n)
    for i in range(12 - len(s)):
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
