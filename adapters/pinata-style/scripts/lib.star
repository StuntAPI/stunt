# Shared library for pinata-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# Pinata auth: either the header pair pinata_api_key + pinata_secret_api_key,
# OR an Authorization: Bearer <JWT>. We check presence of either scheme.

# _header fetches a header from the request (case-insensitive), returning "" if missing.
# HTTP headers are case-insensitive but the engine preserves Go's canonical form.
def _header(req, name):
    headers = req.get("headers")
    if headers == None:
        return ""
    # Direct match.
    v = headers.get(name)
    if v != None:
        return v
    # Case-insensitive match.
    target = name.lower()
    for k in headers:
        if k.lower() == target:
            return headers[k]
    return ""

# _bearer extracts the Bearer token from the Authorization header.
def _bearer(req):
    auth = _header(req, "Authorization")
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _require_auth checks that the request carries either the Pinata API key
# pair OR a Bearer JWT. Returns None if authorized, or an error-response dict
# if not.
def _require_auth(req):
    api_key = _header(req, "pinata_api_key")
    secret = _header(req, "pinata_secret_api_key")
    jwt = _bearer(req)
    if (api_key != "" and secret != "") or jwt != "":
        return None
    return _p_err(401, "UNAUTHORIZED", "Missing or invalid authentication. Please provide a valid API key pair or Bearer JWT.")

# _p_err returns a Pinata-style error response.
# Shape: { error: { reason, details } }
def _p_err(status, reason, details):
    return respond(status, {
        "error": {
            "reason": reason,
            "details": details,
        },
    })

# _hex_val maps one lowercase hex digit to its value (find == -1 never
# happens: crypto.sha256's default encoding is lowercase hex).
def _hex_val(ch):
    return "0123456789abcdef".find(ch)

# _base58 encodes a non-negative int in the Bitcoin/IPFS alphabet.
def _base58(n):
    alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
    out = ""
    while n > 0:
        out = alphabet[n % 58] + out
        n = n // 58
    return out

# _cid_for returns a real CIDv0 for content bytes: base58 of the sha2-256
# multihash (0x12 0x20 || digest), which is why it always starts "Qm".
# Content addressing means identical bytes pin to the identical CID, so
# re-pins are detectable (isDuplicate), like the real API.
def _cid_for(data):
    n = 0x1220  # multihash prefix: sha2-256, 32-byte digest
    digest = crypto.sha256(data)
    for i in range(len(digest)):
        n = n * 16 + _hex_val(digest[i])
    return _base58(n)

# _pin_id generates a Pinata pin row id.
def _pin_id():
    n = store_kv_incr("pinata", "pin_seq")
    return str(7000000000 + n)

# _timestamp returns the pin time in Pinata's ISO-8601 millisecond form
# (real API stamps each pin at request time; now_rfc3339 stops at seconds).
def _timestamp():
    s = clock.now_rfc3339()
    if s.endswith("Z"):
        return s[:-1] + ".000Z"
    return s

# _pin_for returns the stored pin doc with the given CID, or None.
def _pin_for(cid):
    for doc in store_collection("pins").list():
        if doc.get("ipfs_pin_hash", "") == cid:
            return doc
    return None

# _pin_public returns the Pinata-shaped pin list row.
def _pin_row(doc):
    return {
        "id": doc.get("id", ""),
        "ipfs_pin_hash": doc.get("ipfs_pin_hash", ""),
        "size": doc.get("size", 0),
        "date_pinned": doc.get("date_pinned", ""),
        "metadata": doc.get("metadata", {"name": ""}),
    }

# --- query-param helpers ---

# _get_query reads a query param, returning "" when absent (never None).
def _get_query(req, key):
    q = req.get("query")
    if q == None:
        return ""
    v = q.get(key, "")
    if v == None:
        return ""
    return v

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

# _pin_result returns the Pinata-shaped pin result (from pinFileToIPFS /
# pinJSONToIPFS). is_duplicate is True only on the re-pin response.
def _pin_result(doc, is_duplicate):
    return {
        "IpfsHash": doc.get("ipfs_pin_hash", ""),
        "PinSize": doc.get("size", 0),
        "Timestamp": doc.get("timestamp", ""),
        "isDuplicate": is_duplicate,
    }
