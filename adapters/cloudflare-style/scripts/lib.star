# Shared library for cloudflare-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ====================================================================
# Auth validation (structural)
# ====================================================================
# Cloudflare accepts two auth schemes:
#
#   1. Scoped API token — Authorization: Bearer <api_token>
#      Tokens can be scoped to specific resources/permissions. For v1 we
#      accept any non-empty bearer token (structural check only).
#
#   2. Global API key   — X-Auth-Email + X-Auth-Key headers
#      The X-Auth-Key is the account's Global API Key. We validate both
#      headers are present and non-empty.

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        return ""
    if _has_prefix(auth, "Bearer "):
        return auth[7:]
    return ""

# _cf_err returns a Cloudflare-style error response envelope.
# Cloudflare always wraps responses in {success, errors, messages, result}.
def _cf_err(status_code, error_code, message):
    return respond(status_code, {
        "success": False,
        "errors": [{"code": error_code, "message": message}],
        "messages": [],
        "result": None,
    })

# _cf_ok returns a Cloudflare-style success response envelope.
def _cf_ok(result):
    return respond(200, {
        "success": True,
        "errors": [],
        "messages": [],
        "result": result,
    })

# _cf_ok_with_info returns a success response with result_info (pagination).
# next_cursor is the opaque Cloudflare cursor for the next page, or None when
# there are no more pages. When present it is emitted as result_info.cursors.after
# (the Cloudflare v4 cursor-based pagination envelope).
def _cf_ok_with_info(result_list, count, next_cursor):
    result_info = {
        "page": 1,
        "per_page": 20,
        "total_count": count,
    }
    if next_cursor != None and next_cursor != "":
        result_info["cursors"] = {"after": next_cursor}
    return respond(200, {
        "success": True,
        "errors": [],
        "messages": [],
        "result": result_list,
        "result_info": result_info,
    })

# _get_query safely returns a query parameter value.
def _get_query(req, key, default_val):
    q = req.get("query")
    if q == None:
        return default_val
    val = q.get(key, default_val)
    if val == None:
        return default_val
    return val

# _list_page slices a list of docs by the Cloudflare pagination query params
# (per_page = page size, cursor = opaque cursor token) via the paginate()
# builtin and returns (page, next_cursor). A missing/empty per_page disables
# paging (returns all docs, next_cursor None). next_cursor is None when no
# items remain.
def _list_page(req, docs):
    limit = _to_int(_get_query(req, "per_page", ""))
    cursor = _get_query(req, "cursor", "")
    return paginate(docs, limit, cursor)

# _require_auth returns None if authorized, or a Cloudflare-style 401 error.
def _require_auth(req):
    # Check for Bearer token
    token = _bearer(req)
    if token != "":
        return None

    # Check for X-Auth-Email + X-Auth-Key
    headers = req.get("headers")
    if headers != None:
        email = headers.get("X-Auth-Email", "")
        key = headers.get("X-Auth-Key", "")
        if (email != None and email != "") and (key != None and key != ""):
            return None

    return _cf_err(401, 10000, "Authentication error")

# ====================================================================
# Helpers
# ====================================================================

# _has_prefix returns True if s starts with prefix.
def _has_prefix(s, prefix):
    if len(s) < len(prefix):
        return False
    return s[:len(prefix)] == prefix

# _find_substr returns the index of the first occurrence of needle in s,
# or -1 if not found.
def _find_substr(s, needle):
    if len(needle) == 0:
        return 0
    for i in range(len(s) - len(needle) + 1):
        match = True
        for j in range(len(needle)):
            if s[i+j] != needle[j]:
                match = False
                break
        if match:
            return i
    return -1

# _to_int parses a decimal string to int. Returns 0 on failure.
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

# ====================================================================
# ID generators
# ====================================================================

# _gen_id generates a synthetic 32-char hex ID (like Cloudflare zone/account IDs).
def _gen_id(ns):
    n = store_kv_incr("cf", ns + "_id_seq")
    hex = ""
    v = n * 2654435761 + 0xCF000000
    for i in range(32):
        rem = v % 16
        if rem < 10:
            hex = chr(ord("0") + rem) + hex
        else:
            hex = chr(ord("a") + rem - 10) + hex
        v = v // 16
        if v == 0:
            v = n * 17 + i + 7
    # Pad to 32 chars
    while len(hex) < 32:
        hex = "0" + hex
    return hex[:32]

# _gen_uuid generates a synthetic UUID-like string (for D1 databases).
def _gen_uuid():
    n = store_kv_incr("cf", "uuid_seq")
    hex = ""
    v = n * 2654435761 + 0xABCD0000
    for i in range(32):
        rem = v % 16
        if rem < 10:
            hex = chr(ord("0") + rem) + hex
        else:
            hex = chr(ord("a") + rem - 10) + hex
        v = v // 16
        if v == 0:
            v = n * 31 + i + 3
    while len(hex) < 32:
        hex = "0" + hex
    # Insert UUID dashes: 8-4-4-4-12
    h = hex[:32]
    return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]

# _iso8601 returns the current time as an ISO 8601 (RFC 3339 UTC)
# timestamp — created_on/modified_on reflect request time; seeded docs are
# stamped once at seed time and stay stable.
def _iso8601():
    return clock.now_rfc3339()

# ====================================================================
# Zone helpers (shared by zones.star, dns.star and rules.star)
# ====================================================================

# _default_account_id returns a fixed synthetic account ID.
def _default_account_id():
    return "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"

# _ensure_seed_zones seeds the zones collection with a default zone if empty.
def _ensure_seed_zones():
    zc = store_collection("zones")
    if len(zc.list()) > 0:
        return
    seeded = store_kv_get("cf", "zones_seeded")
    if seeded == "1":
        return
    zc.insert({
        "zone_id": "023e105f4ecef8ad9ca31a8372d0c353",
        "name": "stunt.dev",
        "status": "active",
        "account": {
            "id": _default_account_id(),
            "name": "stunt-account",
        },
        "name_servers": ["stunt.dev.ns1.stunt.dev", "stunt.dev.ns2.stunt.dev"],
        "type": "full",
        "created_on": _iso8601(),
        "modified_on": _iso8601(),
    })
    store_kv_set("cf", "zones_seeded", "1")

# _find_zone returns the zone doc for zone_id (seeding first), or None.
def _find_zone(zone_id):
    _ensure_seed_zones()
    zc = store_collection("zones")
    for z in zc.list():
        if z.get("zone_id", "") == zone_id:
            return z
    return None
