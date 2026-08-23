# Shared library for firebase-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ====================================================================
# Auth helpers
# ====================================================================

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if absent.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _has_auth checks for EITHER a Bearer token OR a key-based auth.
# Firebase endpoints can be authed via Bearer (OAuth2 access token) or via
# key query param / body field. Returns True if any auth is present.
def _has_auth(req):
    tok = _bearer(req)
    if tok != "":
        return True
    # Check for key in query or body.
    query_key = req["query"].get("key", "")
    if query_key != "" and query_key != None:
        return True
    body = req["body"]
    if body != None:
        body_key = body.get("key", "")
        if body_key != "" and body_key != None:
            return True
    return False

# _require_auth returns a 401 error response if no auth is present, or None.
def _require_auth(req):
    if _has_auth(req):
        return None
    return respond(401, {
        "error": {
            "code": 401,
            "message": "Request is missing required authentication credential.",
            "status": "UNAUTHENTICATED",
        },
    })

# ====================================================================
# Error / response helpers
# ====================================================================

# _err returns a Firebase error envelope.
def _err(code, status, message, error_status=""):
    err_obj = {
        "code": code,
        "message": message,
        "status": error_status,
    }
    return respond(status, {"error": err_obj})

# _pad6 zero-pads a number to 6 digits.
def _pad6(n):
    s = str(n)
    while len(s) < 6:
        s = "0" + s
    return s

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
def _to_int(s):
    if s == None or s == "":
        return 0
    neg = False
    if s[0] == "-":
        neg = True
        s = s[1:]
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return -n if neg else n

# ====================================================================
# List pagination
# ====================================================================

# _list_page applies Firebase/Firestore-style paging to a full list of
# resources. It reads the provider's pageSize (page size) and pageToken
# (cursor) query params and delegates to the pure paginate() builtin.
#
# Returns (page, next_cursor) where next_cursor is an opaque string token for
# the next page, or None when no items remain. When pageSize is absent or <= 0
# paging is disabled and the whole list is returned with next_cursor None,
# preserving the unpaginated behavior.
def _list_page(req, docs):
    q = req["query"]
    limit = _to_int(q.get("pageSize", ""))
    cursor = q.get("pageToken", "")
    if cursor == None:
        cursor = ""
    return paginate(docs, limit, cursor)

# ====================================================================
# Firestore typed-value helpers
# ====================================================================
# Firestore represents every field value as a typed wrapper:
#   {stringValue: "x"}     string
#   {integerValue: "5"}    integer (NOTE: string-encoded in the real API)
#   {booleanValue: true}   boolean
#   {doubleValue: 1.5}     float
#   {arrayValue: {values:[...]}}   array
#   {mapValue: {fields:{...}}}     map (nested)
#   {nullValue: null}      null
#   {timestampValue: "..."}        timestamp

# value wrapper (the inverse of _firestore_typed_value). ITERATIVE — the VM
# forbids recursion, and real documents nest arrays and maps arbitrarily
# deep, so containers are built with an explicit work stack of
# [parent, key, typed] items (parent is the list/dict being filled; the
# root rides in a one-element box).
def _firestore_unwrap_value(typed):
    root = [None]
    stack = [[root, 0, typed]]
    while len(stack) > 0:
        item = stack.pop()
        parent = item[0]
        key = item[1]
        tv = item[2]
        if tv == None:
            parent[key] = None
        elif "stringValue" in tv:
            parent[key] = tv["stringValue"]
        elif "integerValue" in tv:
            parent[key] = _to_int(tv["integerValue"])
        elif "booleanValue" in tv:
            parent[key] = tv["booleanValue"]
        elif "doubleValue" in tv:
            parent[key] = tv["doubleValue"]
        elif "nullValue" in tv:
            parent[key] = None
        elif "timestampValue" in tv:
            parent[key] = tv["timestampValue"]
        elif "arrayValue" in tv:
            values = tv["arrayValue"].get("values", [])
            arr = []
            parent[key] = arr
            for v in values:
                # Reserve the slot now; the popped item fills it by index.
                stack.append([arr, len(arr), v])
                arr.append(None)
        elif "mapValue" in tv:
            fields = tv["mapValue"].get("fields", {})
            m = {}
            parent[key] = m
            for k in fields:
                stack.append([m, k, fields[k]])
        else:
            parent[key] = None
    return root[0]

# _firestore_unwrap_fields converts Firestore typed fields back to raw values.
def _firestore_unwrap_fields(fields):
    result = {}
    for k in fields:
        result[k] = _firestore_unwrap_value(fields[k])
    return result

# _type_name returns a type string for a value.
def _type_name(val):
    if val == None:
        return "null"
    t = type(val)
    if t == "string":
        return "string"
    if t == "int":
        return "int"
    if t == "bool":
        return "bool"
    if t == "float":
        return "float"
    if t == "list":
        return "list"
    if t == "dict":
        return "dict"
    return "string"

# _is_dict returns True if val is a dict (map).
def _is_dict(val):
    return type(val) == "dict"

# _is_list returns True if val is a list.
def _is_list(val):
    return type(val) == "list"

# _is_int_str returns True if s is a string of all digits (possibly with
# leading minus).
def _is_int_str(s):
    if len(s) == 0:
        return False
    start = 0
    if s[0] == "-":
        start = 1
    if start >= len(s):
        return False
    for i in range(start, len(s)):
        if s[i] < "0" or s[i] > "9":
            return False
    return True

# _is_float returns True if s looks like a float (has '.' or 'e' and is
# numeric).
def _is_float(s):
    if not _contains(s, ".") and not _contains(s, "e") and not _contains(s, "E"):
        return False
    for i in range(len(s)):
        ch = s[i]
        ok = (ch >= "0" and ch <= "9") or ch == "." or ch == "-" or ch == "+" or ch == "e" or ch == "E"
        if not ok:
            return False
    return True
