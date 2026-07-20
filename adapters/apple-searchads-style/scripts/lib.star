# Shared library for apple-searchads-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.
#
# Apple Search Ads uses OAuth2 with a client-secret JWT signed using
# ES256. The JWT is exchanged for a bearer access token. Here we do
# STRUCTURAL validation only — decode the JOSE header and verify it
# has 3 segments. We do NOT verify the ECDSA signature.

# --- base64url decode (pure Starlark) ---

_CHARS = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x20\x21\x22\x23\x24\x25\x26\x27\x28\x29\x2a\x2b\x2c\x2d\x2e\x2f\x30\x31\x32\x33\x34\x35\x36\x37\x38\x39\x3a\x3b\x3c\x3d\x3e\x3f\x40\x41\x42\x43\x44\x45\x46\x47\x48\x49\x4a\x4b\x4c\x4d\x4e\x4f\x50\x51\x52\x53\x54\x55\x56\x57\x58\x59\x5a\x5b\x5c\x5d\x5e\x5f\x60\x61\x62\x63\x64\x65\x66\x67\x68\x69\x6a\x6b\x6c\x6d\x6e\x6f\x70\x71\x72\x73\x74\x75\x76\x77\x78\x79\x7a\x7b\x7c\x7d\x7e\x7f"

_B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

# _b64url_val maps a single base64url char to its 6-bit value. Returns -1 for invalid.
def _b64url_val(ch):
    return _B64URL.find(ch)

# _b64url_decode decodes a base64url string (no padding) into plaintext.
def _b64url_decode(seg):
    seg = seg.replace("=", "")
    vals = []
    for i in range(len(seg)):
        v = _b64url_val(seg[i])
        if v < 0:
            return ""
        vals.append(v)
    while len(vals) % 4 != 0:
        vals.append(0)
    result = ""
    num_vals = len(vals)
    i = 0
    orig_len = len(seg)
    while i < num_vals:
        v1 = vals[i]
        v2 = vals[i + 1]
        v3 = vals[i + 2]
        v4 = vals[i + 3]
        b1 = v1 * 4 + v2 // 16
        result = result + _CHARS[b1]
        if orig_len > i + 2:
            b2 = (v2 % 16) * 16 + v3 // 4
            result = result + _CHARS[b2]
        if orig_len > i + 3:
            b3 = (v3 % 4) * 64 + v4
            result = result + _CHARS[b3]
        i = i + 4
    return result

# _jose_header decodes the JOSE header from a JWT. Returns "" if malformed.
def _jose_header(token):
    parts = token.split(".")
    if len(parts) != 3:
        return ""
    return _b64url_decode(parts[0])

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _check_bearer validates an Authorization: Bearer <token> header.
# Structural only: checks for 3 segments. We accept any non-empty bearer
# token as valid for local testing (the real API uses OAuth2 access tokens
# obtained from a JWT exchange).
def _check_bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth == "":
        auth = req["headers"].get("authorization", "")
    if auth[:7] != "Bearer " and auth[:7] != "bearer ":
        return None
    token = auth[7:]
    if token == "":
        return None
    return token

# _require_auth checks for a valid Bearer header. Returns True/False.
def _require_auth(req):
    tok = _check_bearer(req)
    if tok == None:
        return False
    return True

# _err returns a Search Ads error body.
def _err(message):
    return {"data": {"status": "ERROR", "message": message}}

# _seed_campaigns populates default campaigns on first access.
def _seed_campaigns():
    if store_kv_get("searchads", "seeded") == "yes":
        return
    store_kv_set("searchads", "seeded", "yes")

    cc = store_collection("campaigns")
    cc.insert({
        "id": _gen_campaign_id(),
        "campaignId": 543210001,
        "name": "Brand Campaign - Spring",
        "budgetAmount": {"amount": "10000", "currency": "USD"},
        "dailyBudgetAmount": {"amount": "500", "currency": "USD"},
        "servingStatus": "RUNNING",
        "servingStateReasons": [],
        "creationTime": "2024-01-15T10:00:00.000",
        "modificationTime": "2024-01-15T10:00:00.000",
    })
    cc.insert({
        "id": _gen_campaign_id(),
        "campaignId": 543210002,
        "name": "Competitor Campaign",
        "budgetAmount": {"amount": "5000", "currency": "USD"},
        "dailyBudgetAmount": {"amount": "250", "currency": "USD"},
        "servingStatus": "PAUSED",
        "servingStateReasons": ["USER_PAUSED"],
        "creationTime": "2024-01-10T08:00:00.000",
        "modificationTime": "2024-01-12T14:30:00.000",
    })

# _gen_campaign_id generates a sequential internal ID.
def _gen_campaign_id():
    seq = store_kv_incr("searchads", "campaign_seq")
    return "cmp_" + _pad6(seq)

# _pad6 zero-pads to 6 digits.
def _pad6(n):
    s = str(n)
    while len(s) < 6:
        s = "0" + s
    return s
