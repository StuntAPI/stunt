# Shared library for zendesk-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins.

# Zendesk auth: Basic auth (email/token:secret) or Bearer token.

# _auth_header returns the Authorization header value (or "" if absent).
def _auth_header(req):
    auth = req["headers"].get("Authorization", "")
    if auth == None:
        auth = ""
    return auth

# _is_basic checks if the Authorization header is Basic auth.
def _is_basic(req):
    auth = _auth_header(req)
    return auth.startswith("Basic ")

# _is_bearer checks if the Authorization header is Bearer.
def _is_bearer(req):
    auth = _auth_header(req)
    return auth.startswith("Bearer ")

# _require_auth checks for Basic or Bearer auth. Returns (True, None) on
# success, or (False, 401 error response) on failure.
def _require_auth(req):
    if _is_basic(req) or _is_bearer(req):
        return True, None
    return False, _zd_unauth()

# _zd_error returns a Zendesk-style error response.
# Zendesk uses {error:"...", description:"..."}.
def _zd_error(status_code, error, description):
    return respond(status_code, {
        "error": error,
        "description": description,
    })

# _zd_unauth returns the 401 error for missing auth.
def _zd_unauth():
    return respond(401, {
        "error": "InvalidCredentials",
        "description": "Authentication required",
    })

# _now returns a synthetic timestamp.
def _now():
    return "2024-01-01T00:00:00Z"

# _next_id returns a monotonically-increasing numeric ID.
def _next_id(obj_type):
    n = store_kv_incr("zendesk", obj_type + "_seq")
    return str(10000 + n)

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

# _ticket_shape builds the Zendesk ticket shape from a stored doc.
def _ticket_shape(doc):
    assignee = doc.get("assignee_id", None)
    if assignee == None:
        assignee = None
    return {
        "id": doc.get("id", ""),
        "subject": doc.get("subject", ""),
        "status": doc.get("status", "open"),
        "requester_id": doc.get("requester_id", ""),
        "assignee_id": assignee,
        "created_at": doc.get("created_at", _now()),
        "updated_at": doc.get("updated_at", _now()),
        "description": doc.get("description", ""),
    }

# _has_more returns True if there are more results (Zendesk uses meta.has_more).
def _has_more(total, page_size, offset):
    return offset + page_size < total

# _next_link builds the links.next URL for cursor pagination. The cursor
# passed in next_offset is the opaque token returned by paginate() and is
# round-tripped via the page query param.
def _next_link(path, next_offset, page_size):
    return path + "?page=" + str(next_offset) + "&per_page=" + str(page_size)

# _list_page applies builtin cursor pagination to a full list of docs.
# Reads the Zendesk page-size (per_page) and cursor (page) query params
# from the request and delegates to the paginate() builtin. Returns
# (page_docs, next_cursor). When per_page is absent it defaults to 100;
# when per_page <= 0 paging is disabled and all docs are returned with
# next_cursor None. Apply this AFTER any handler-side filtering.
def _list_page(req, docs):
    page_size = _to_int(_get_query(req, "per_page", "100"))
    cursor = _get_query(req, "page", "")
    if cursor == None:
        cursor = ""
    page_docs, next_cursor = paginate(docs, page_size, cursor)
    return page_docs, next_cursor

# _zd_sorted applies the Zendesk sort/sort_order query params (the search
# endpoint names its param sort_by) via query_select, BEFORE paging like the
# real API. Returns the list unchanged when no sort field is given.
def _zd_sorted(req, docs, sort_param):
    sort = _get_query(req, sort_param, "")
    if sort == "":
        return docs
    order_dir = _get_query(req, "sort_order", "asc")
    return query_select(docs, None, sort, order_dir, None, None, None)
