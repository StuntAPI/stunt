# Shared library for walletconnect-style adapter scripts.
#
# This file is preloaded by stunt before each handler script. Its top-level
# definitions are available to all handlers as predeclared builtins.

# --- auth helper ---

# _require_project_id checks for a valid projectId in the request body,
# query, or Authorization header. Returns the projectId if present, or None
# if absent. The real WC relay uses the projectId for rate-limiting and
# access control; we validate that it is present (non-empty).
def _require_project_id(req):
    # Check body first (most WC SDKs pass it in the JSON body).
    if req.get("body") != None:
        pid = req["body"].get("projectId", "")
        if pid != None and pid != "":
            return pid
    # Check query params.
    pid = req["query"].get("projectId", "")
    if pid != None and pid != "":
        return pid
    # Check Authorization header (some SDKs send it as a bearer).
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer " and len(auth) > 7:
        return auth[7:]
    return None

# --- deterministic helpers ---

# _hex64 returns a 64-char zero string (for hex placeholders).
def _hex64():
    return "0" * 64

# _deterministic_hash produces a 64-char hex string ("0x" + 64 hex chars)
# from an input string. Deterministic pseudo-hash, NOT real keccak256.
def _deterministic_hash(input_str):
    lanes = [
        0x811c9dc5,
        0x1000193,
        0xdeadbeef,
        0xcafebabe,
        0x12345678,
        0x9e3779b9,
        0xabcdef01,
        0x0badf00d,
    ]
    for i in range(len(input_str)):
        ch = ord(input_str[i])
        for li in range(len(lanes)):
            lanes[li] = (lanes[li] ^ ch)
            lanes[li] = (lanes[li] * 0x01000193) & 0xffffffff
            lanes[li] = ((lanes[li] << 7) | (lanes[li] >> 25)) & 0xffffffff
    hex_str = ""
    for li in range(len(lanes)):
        hex_str = hex_str + _hex32(lanes[li])
    return "0x" + hex_str

# _hex32 converts a 32-bit int to an 8-char hex string (zero-padded).
def _hex32(n):
    digits = "0123456789abcdef"
    out_chars = []
    for _ in range(8):
        out_chars.append(digits[n % 16])
        n = n // 16
    result = ""
    for i in range(len(out_chars) - 1, -1, -1):
        result = result + out_chars[i]
    return result

# _topic generates a WC-style topic string (hex, 64 chars without 0x).
def _topic(seed_str):
    h = _deterministic_hash(seed_str)
    return h[2:]  # strip 0x prefix, raw hex

# --- relay constants ---

# Default expiry for pairings (30 days, matching WC SDK default).
PAIRING_EXPIRY = 2592000
# Default expiry for sessions (7 days, matching WC SDK default).
SESSION_EXPIRY = 604800

# Mock wallet address used for auto-approved sessions.
WALLET_ADDRESS = "0x1234567890abcdef1234567890abcdef12345678"

# --- WC URI parsing ---

# _parse_wc_uri parses a "wc:<topic>@2?relay-protocol=irn&symKey=<key>" URI.
# Returns {"topic":..., "relayProtocol":..., "symKey":...} or None if invalid.
def _parse_wc_uri(uri):
    if uri == None or uri == "":
        return None
    if uri[:3] != "wc:":
        return None
    rest = uri[3:]
    at_idx = _find_char(rest, "@")
    if at_idx < 0:
        return None
    topic = rest[:at_idx]
    after_at = rest[at_idx + 1:]
    q_idx = _find_char(after_at, "?")
    if q_idx < 0:
        return {"topic": topic, "relayProtocol": "irn", "symKey": ""}
    relay_proto = "irn"
    sym_key = ""
    params = after_at[q_idx + 1:]
    pairs = _split_str(params, "&")
    for pair in pairs:
        eq = _find_char(pair, "=")
        if eq < 0:
            continue
        key = pair[:eq]
        val = pair[eq + 1:]
        if key == "relay-protocol":
            relay_proto = val
        elif key == "symKey":
            sym_key = val
    return {"topic": topic, "relayProtocol": relay_proto, "symKey": sym_key}

# --- string helpers (Starlark has limited string methods) ---

# _find_char finds the index of a character in a string, or -1.
def _find_char(s, ch):
    for i in range(len(s)):
        if s[i] == ch:
            return i
    return -1

# _split_str splits a string by a single character delimiter.
def _split_str(s, delim):
    result = []
    current = ""
    for i in range(len(s)):
        if s[i] == delim:
            result.append(current)
            current = ""
        else:
            current = current + s[i]
    result.append(current)
    return result

# --- JSON-RPC helpers ---

# _rpc_ok builds a successful JSON-RPC 2.0 response object.
def _rpc_ok(id, result):
    return {"jsonrpc": "2.0", "id": id, "result": result}

# _rpc_err builds an error JSON-RPC 2.0 response object.
def _rpc_err(id, code, message):
    return {"jsonrpc": "2.0", "id": id, "error": {"code": code, "message": message}}
