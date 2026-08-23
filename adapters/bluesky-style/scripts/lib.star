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

# _mint_cid cycles real CIDv1 strings (dag-json + sha-256 of fixed
# payloads) — official clients CID.parse() every cid they receive and
# reject anything that is not a structurally valid multibase CID.
_CIDS = [
    "bafybeiadg54dhhbxmhjzor74gbedkw57wc7277ito7dahav6d7igxbg3oe",
    "bafybeibev6enzuab6i6632ylktg2bqkucyskw4ehzc2kemnmd2zmqi3uou",
    "bafybeidqfyjtzpxp43kosc3xixocpk5dm7fysch6fpkwflctynpfe5ncf4",
    "bafybeigw4s3iwsx5gvslquhcbvoggdhffqvpsspmn2xxyxzwa4mscqkwhy",
    "bafybeickeoht3cj3nvwhh3cjw2u2neyji3fuaevfjutqcodmjy5qzzpste",
    "bafybeiea54nla6g6fk55lxn3i6e47ug44r7r4zdu4irxhhtpfejij2rhsq",
    "bafybeienxokurrjqnhv7j3t3co5cfkltafyhunmrdls2xvj2l4keobctxa",
    "bafybeibwf7ex4yicnrfz2ofgmsgwacbd5tmjxoogclj6ii5ozhwa6nlpru",
]

def _mint_cid(seq):
    return _CIDS[seq % len(_CIDS)]

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
