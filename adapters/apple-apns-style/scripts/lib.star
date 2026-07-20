# Shared library for apple-apns-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.
#
# JWT validation here is STRUCTURAL only: we decode the JOSE header from
# base64url and confirm alg=="ES256". We do NOT verify the ECDSA signature
# (documented stretch goal). See README for details.

# --- base64url decode (pure Starlark, no builtins) ---

# _CHARS maps byte value 0..127 to its ASCII character, used as a chr()
# substitute (Starlark has no chr() builtin).
_CHARS = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x20\x21\x22\x23\x24\x25\x26\x27\x28\x29\x2a\x2b\x2c\x2d\x2e\x2f\x30\x31\x32\x33\x34\x35\x36\x37\x38\x39\x3a\x3b\x3c\x3d\x3e\x3f\x40\x41\x42\x43\x44\x45\x46\x47\x48\x49\x4a\x4b\x4c\x4d\x4e\x4f\x50\x51\x52\x53\x54\x55\x56\x57\x58\x59\x5a\x5b\x5c\x5d\x5e\x5f\x60\x61\x62\x63\x64\x65\x66\x67\x68\x69\x6a\x6b\x6c\x6d\x6e\x6f\x70\x71\x72\x73\x74\x75\x76\x77\x78\x79\x7a\x7b\x7c\x7d\x7e\x7f"

# _B64URL is the base64url alphabet (- and _ replace + and /).
_B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

# _b64url_val maps a single base64url character to its 6-bit value (0..63).
# Returns -1 for invalid characters.
def _b64url_val(ch):
    idx = _B64URL.find(ch)
    return idx

# _b64url_decode decodes a base64url string (no padding) into a plaintext
# string. Only handles bytes 0..127 (sufficient for ASCII JSON in JWT
# segments). Returns "" on any decode error.
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

# _b64url_encode encodes a string into base64url (no padding). Only handles
# ASCII input.
def _b64url_encode(text):
    result = ""
    i = 0
    n = len(text)
    while i < n:
        b1 = ord(text[i])
        if i + 1 < n:
            b2 = ord(text[i + 1])
        else:
            b2 = -1
        if i + 2 < n:
            b3 = ord(text[i + 2])
        else:
            b3 = -1
        c1 = b1 // 4
        result = result + _B64URL[c1]
        c2 = (b1 % 4) * 16
        if b2 >= 0:
            c2 = c2 + b2 // 16
        result = result + _B64URL[c2]
        if b2 >= 0:
            c3 = (b2 % 16) * 4
            if b3 >= 0:
                c3 = c3 + b3 // 64
            result = result + _B64URL[c3]
        if b3 >= 0:
            c4 = b3 % 64
            result = result + _B64URL[c4]
        i = i + 3
    return result

# --- JWT helpers ---

# _jose_header decodes the JOSE header (segment 0) of a JWT string and
# returns the decoded JSON text. Returns "" if the token is malformed.
def _jose_header(token):
    parts = token.split(".")
    if len(parts) != 3:
        return ""
    if parts[0] == "" or parts[1] == "" or parts[2] == "":
        return ""
    return _b64url_decode(parts[0])

# _jwt_payload decodes the payload (segment 1) of a JWT string.
def _jwt_payload(token):
    parts = token.split(".")
    if len(parts) != 3:
        return ""
    return _b64url_decode(parts[1])

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _check_jwt_bearer validates the authorization: bearer <jwt> header for
# APNs requests. Header name is case-insensitive in real APNs (HTTP/2);
# we check both "authorization" and "Authorization".
#
# Structural validation only:
#   - authorization header must be "bearer <jwt>"
#   - JWT must have 3 dot-separated segments
#   - JOSE header (base64url-decoded) must contain "ES256"
# Signature crypto is NOT verified (documented stretch goal).
def _check_jwt_bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth == "":
        auth = req["headers"].get("authorization", "")
    if auth[:7] != "Bearer " and auth[:7] != "bearer ":
        return None
    token = auth[7:]
    parts = token.split(".")
    if len(parts) != 3:
        return None
    header = _jose_header(token)
    if header == "":
        return None
    if not _contains(header, "ES256"):
        return None
    return token

# _require_jwt returns the token if valid, or an error response if not.
def _require_jwt(req):
    token = _check_jwt_bearer(req)
    if token == None:
        return None, respond(403, {"reason": "BadDeviceToken"})
    return token, None

# _mint_jwt creates a plausible JWT string with an ES256 JOSE header.
# Used for internal token minting (not for signature verification).
def _mint_jwt(header_json, payload_json):
    h = _b64url_encode(header_json)
    p = _b64url_encode(payload_json)
    sig = "c3ludGhldGljLXNpZ25hdHVyZS1wbGFjZWhvbGRlcg"
    return h + "." + p + "." + sig

# --- APNs helpers ---

# _generate_apns_id creates a synthetic APNs ID (UUID-like).
def _generate_apns_id():
    seq = store_kv_incr("apns", "apns_id_seq")
    # Format as a UUID-like string.
    s = str(0x10000000 + seq)
    return s + "-0000-0000-0000-0000000000" + str(seq)[-3:]

# _seed populates default device tokens on first access.
def _seed():
    if store_kv_get("apns", "seeded") == "yes":
        return
    store_kv_set("apns", "seeded", "yes")
    c = store_collection("devices")
    c.insert({
        "id": "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
        "registered": True,
        "last_sent": None,
    })

# _find_device looks up a device by token. Returns the doc or None.
def _find_device(token):
    c = store_collection("devices")
    return c.get(token)
