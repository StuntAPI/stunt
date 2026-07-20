# Shared library for hubspot-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins.

# HubSpot CRM auth: Bearer token OR hapikey query param.

# _bearer extracts the Bearer token from the Authorization header.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _require_auth checks for Bearer token OR hapikey query param. Returns
# (ok, error_resp). If neither is present, returns (False, 401 response).
def _require_auth(req):
    token = _bearer(req)
    if token != "":
        return True, None
    # Check hapikey query param.
    q = req.get("query")
    if q != None:
        hapikey = q.get("hapikey", "")
        if hapikey != "" and hapikey != None:
            return True, None
    return False, _auth_error()

# _auth_error returns the HubSpot 401 error response.
def _auth_error():
    return respond(401, {
        "status": "error",
        "message": "The authentication credentials are missing or invalid.",
        "category": "AUTHENTICATION",
        "errors": [],
        "corrId": "synthetic-corr-id",
    })

# _hs_error returns a HubSpot-style error response.
def _hs_error(status_code, message, category):
    return respond(status_code, {
        "status": "error",
        "message": message,
        "category": category,
        "errors": [],
        "corrId": "synthetic-corr-id",
    })

# _now returns a synthetic timestamp.
def _now():
    return "2024-01-01T00:00:00Z"

# _next_id returns a monotonically-increasing numeric ID.
# Seeds use id="1" (first record); generated IDs start from 2 to avoid
# collision.
def _next_id(obj_type):
    n = store_kv_incr("hubspot", obj_type + "_seq")
    return str(n + 1)

# _collection_for maps an object type to its backing collection name.
# HubSpot object types are pluralized: contacts, companies, deals, tickets.
_COLLECTIONS = {
    "contacts": "contacts",
    "companies": "companies",
    "deals": "deals",
    "tickets": "tickets",
}

# _collection returns the store_collection for the given object type (path segment).
def _collection(obj_type):
    name = _COLLECTIONS.get(obj_type, "")
    if name == "":
        return None
    return store_collection(name)

# _obj_type_from_path extracts the object type from the URL path.
# Paths look like /crm/v3/objects/contacts or /crm/v3/objects/contacts/{id}
def _obj_type_from_path(req):
    path = req["path"]
    parts = _split(path, "/")
    # find "objects" then take next token
    for i in range(len(parts)):
        if parts[i] == "objects" and i + 1 < len(parts):
            return parts[i + 1]
    return ""

# _split splits a string on a delimiter (single-char). Returns a list.
def _split(s, delim):
    result = []
    current = ""
    for i in range(len(s)):
        ch = s[i]
        if ch == delim:
            result.append(current)
            current = ""
        else:
            current = current + ch
    result.append(current)
    return result

# _index returns the index of the first occurrence of needle in haystack, or
# -1 if not found.
def _index(haystack, needle):
    if len(needle) == 0:
        return 0
    for i in range(len(haystack) - len(needle) + 1):
        match = True
        for j in range(len(needle)):
            if haystack[i + j] != needle[j]:
                match = False
                break
        if match:
            return i
    return -1

# _contains returns True if haystack contains needle.
def _contains(haystack, needle):
    return _index(haystack, needle) >= 0

# _to_int converts a string to an int (returns 0 on failure).
def _to_int(s):
    if s == "" or s == None:
        return 0
    result = 0
    for i in range(len(s)):
        ch = s[i]
        code = ord(ch)
        if code >= 48 and code <= 57:
            result = result * 10 + (code - 48)
        else:
            return 0
    return result

# _get_query safely returns a query parameter value.
def _get_query(req, key, default_val):
    q = req.get("query")
    if q == None:
        return default_val
    val = q.get(key, default_val)
    if val == None:
        return default_val
    return val

# _get_body safely returns the request body dict.
def _get_body(req):
    body = req.get("body")
    if body == None:
        return {}
    return body

# _paginate applies cursor-based pagination to a list of docs.
# HubSpot uses "after" cursor (an integer offset) with "limit".
# Returns (paged_docs, after_cursor_or_None, total_if_available).
def _paginate(req, docs):
    limit = _to_int(_get_query(req, "limit", "10"))
    if limit <= 0:
        limit = 10
    after = _to_int(_get_query(req, "after", "0"))

    total = len(docs)
    start = after
    if start > total:
        start = total
    end = start + limit
    if end > total:
        end = total

    paged = docs[start:end]

    # Determine next cursor.
    next_after = None
    if end < total:
        next_after = str(end)

    return paged, next_after

# _record_shape builds the HubSpot record shape from a stored doc.
def _record_shape(doc):
    return {
        "id": doc.get("id", ""),
        "properties": doc.get("properties", {}),
        "createdAt": doc.get("createdAt", _now()),
        "updatedAt": doc.get("updatedAt", _now()),
        "archived": doc.get("archived", False),
    }
