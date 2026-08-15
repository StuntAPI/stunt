# Shared library for gtasks-style adapter scripts.

# _bearer extracts the token from "Authorization: Bearer <t>".
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _require_bearer returns None if OK, or a 401 response if missing.
def _require_bearer(req):
    if _bearer(req) == "":
        return respond(401, {
            "error": {
                "code": 401,
                "message": "The request does not have valid authentication credentials.",
                "status": "UNAUTHENTICATED",
            },
        })
    return None

# _g_err returns a Google-style error response.
def _g_err(code, message, status):
    return respond(code, {
        "error": {
            "code": code,
            "message": message,
            "status": status,
        },
    })

# _to_int parses a decimal string to int.
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

# _gen_id generates a task/tasklist ID.
def _gen_id(prefix, seq):
    return prefix + "-" + str(seq)

# _now_ms returns the current time in the Google Tasks timestamp format
# (RFC3339 with milliseconds, e.g. "...T00:00:00.000Z"). Derived from the
# engine clock, so created/updated stamps track real elapsed time.
def _now_ms():
    rfc = clock.now_rfc3339()
    return rfc[:-1] + ".000Z"

# _seed creates a default task list.
def _seed():
    if store_kv_get("gtasks", "seeded") == "yes":
        return
    store_kv_set("gtasks", "seeded", "yes")

    lc = store_collection("tasklists")
    lc.insert({
        "id": "MTA0NTQwNDI0NDQ4NDI1MDQ4OjIxMDI4NjYwNTpkZWZhdWx0",
        "title": "My Tasks",
        "updated": _now_ms(),
        "selfLink": "https://www.googleapis.com/tasks/v1/users/@me/lists/MTA0NTQwNDI0NDQ4NDI1MDQ4OjIxMDI4NjYwNTpkZWZhdWx0",
    })

# _find_tasklist returns the tasklist by id, or None.
def _find_tasklist(list_id):
    lc = store_collection("tasklists")
    for tl in lc.list():
        if tl.get("id") == list_id:
            return tl
    return None

# === Pagination ===

# _list_page applies Google Tasks pagination (maxResults + pageToken) to a full
# list and returns (page, next_cursor). Delegates to the builtin
# paginate(items, limit, cursor): limit None/<=0 disables paging (returns all
# items, next_cursor None); cursor is the opaque token from a prior call.
def _list_page(req, items):
    limit = _to_int(req["query"].get("maxResults", ""))
    cursor = req["query"].get("pageToken", "")
    if cursor == None:
        cursor = ""
    return paginate(items, limit, cursor)
