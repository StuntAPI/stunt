# Shared library for apple-apns-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.
#
# Provider-token verification is cryptographically REAL: the inbound
# authorization: bearer <jwt> is checked for an ES256 JOSE header, its
# signature is verified with crypto.ecdsa_verify_p256 against a fixed
# synthetic EC P-256 public key, and the claims are expired-checked
# (exp, or iat + 1h per real APNs provider-token lifetime). See README.

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

# _b64url_ok reports whether seg is a syntactically valid unpadded
# base64url segment (alphabet chars only, length not == 1 mod 4). Guards the
# crypto.base64url_decode / crypto.ecdsa_verify_p256 builtins, which error
# (surfacing as a 500) on malformed input.
def _b64url_ok(seg):
    if seg == "":
        return False
    if len(seg) % 4 == 1:
        return False
    for i in range(len(seg)):
        if _B64URL.find(seg[i]) < 0:
            return False
    return True

# _jwt_json decodes a JWT segment (0=header, 1=payload) into a Starlark
# dict via crypto.base64url_decode + json.decode, or None when malformed.
# Shape guards keep json.decode from erroring on garbage.
def _jwt_json(token, seg):
    parts = token.split(".")
    if len(parts) != 3:
        return None
    if not _b64url_ok(parts[0]) or not _b64url_ok(parts[1]) or not _b64url_ok(parts[2]):
        return None
    txt = crypto.base64url_decode(parts[seg])
    if txt == "" or txt[:1] != "{" or txt[-1:] != "}" or txt.find('"') < 0:
        return None
    for i in range(len(txt)):
        if ord(txt[i]) < 0x20:
            return None
    return json.decode(txt)

# _claim_int coerces a claim value to int (JSON numbers decode as int).
# Returns None when absent/None.
def _claim_int(v):
    if v == None:
        return None
    if type(v) == "int":
        return v
    return None

# _provider_token_expired reports whether the provider-token claims are
# stale: exp when present, otherwise iat + 3600 (APNs caps provider-token
# lifetime at one hour). Tokens without either claim never read as fresh.
def _provider_token_expired(claims):
    now = clock.now_unix()
    exp = _claim_int(claims.get("exp", None))
    if exp != None:
        return now >= exp
    iat = _claim_int(claims.get("iat", None))
    if iat != None:
        return now >= iat + 3600
    return True

# _require_jwt returns the token if valid, or an error response if not.
# Distinct reasons per real APNs: a present-but-expired provider token is
# 403 ExpiredProviderToken; anything else unusable is 403 (bad/missing
# provider auth).
def _require_jwt(req):
    auth = req["headers"].get("Authorization", "")
    if auth == "":
        auth = req["headers"].get("authorization", "")
    token, expired = _verify_provider_token(auth)
    if token == None:
        if expired:
            return None, respond(403, {"reason": "ExpiredProviderToken"})
        return None, respond(403, {"reason": "BadDeviceToken"})
    return token, None

# _verify_provider_token validates a bearer token value end-to-end.
# Returns (token, expired_flag); token is None when invalid, with
# expired_flag True only for the expired case.
def _verify_provider_token(auth):
    if auth[:7] != "Bearer " and auth[:7] != "bearer ":
        return None, False
    token = auth[7:]
    header = _jwt_json(token, 0)
    if header == None:
        return None, False
    if header.get("alg", "") != "ES256":
        return None, False
    parts = token.split(".")
    if not crypto.ecdsa_verify_p256(_JWT_PUBLIC_KEY, parts[0] + "." + parts[1], parts[2], encoding="base64url"):
        return None, False
    claims = _jwt_json(token, 1)
    if claims == None:
        return None, False
    if _provider_token_expired(claims):
        return None, True
    return token, False

# Fixed synthetic EC P-256 public key used to verify provider JWTs (the
# locally "registered" provider key). Throwaway mock material — it exists
# nowhere but this repository.
_JWT_PUBLIC_KEY = """-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEujjAWCmCOpqAjKNBXkNtwgL0DE+b
MjGa3+4zCpr4ZiP2jCbqHTVquHi0quv1+KFq4mmO0bCsSK0ORf0JfsulyQ==
-----END PUBLIC KEY-----"""

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
