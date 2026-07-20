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

# _cid_gen generates a deterministic-looking CIDv0 string (Qm + 44 base58 chars).
# Uses a monotonic counter so each pin gets a unique CID.
def _cid_gen():
    n = store_kv_incr("pinata", "cid_seq")
    # Base58 alphabet (Bitcoin / IPFS flavour)
    alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
    seed = n + 1
    suffix = ""
    for _ in range(44):
        suffix = alphabet[seed % 58] + suffix
        seed = seed // 58 + 1
    return "Qm" + suffix

# _pin_id generates a Pinata pin row id.
def _pin_id():
    n = store_kv_incr("pinata", "pin_seq")
    return str(7000000000 + n)

# _timestamp generates a synthetic ISO-8601 timestamp.
def _timestamp():
    return "2024-06-15T12:30:00.000Z"

# _pin_public returns the Pinata-shaped pin list row.
def _pin_row(doc):
    return {
        "id": doc.get("id", ""),
        "ipfs_pin_hash": doc.get("ipfs_pin_hash", ""),
        "size": doc.get("size", 0),
        "date_pinned": doc.get("date_pinned", ""),
        "metadata": doc.get("metadata", {"name": ""}),
    }

# _pin_result returns the Pinata-shaped pin result (from pinFileToIPFS / pinJSONToIPFS).
def _pin_result(doc):
    return {
        "IpfsHash": doc.get("ipfs_pin_hash", ""),
        "PinSize": doc.get("size", 0),
        "Timestamp": doc.get("timestamp", ""),
        "isDuplicate": doc.get("is_duplicate", False),
    }
