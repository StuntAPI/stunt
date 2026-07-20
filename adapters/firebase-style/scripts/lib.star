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
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

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

# _firestore_typed_value wraps a raw Go/JSON value into a Firestore typed
# value wrapper. This is the core of the Firestore pain — every field is
# wrapped in a type-keyed object.
def _firestore_typed_value(val):
    t = _type_name(val)
    if t == "string":
        return {"stringValue": val}
    if t == "int":
        return {"integerValue": str(val)}
    if t == "bool":
        return {"booleanValue": val}
    if t == "float":
        return {"doubleValue": val}
    if t == "null":
        return {"nullValue": None}
    if t == "list":
        values = []
        for item in val:
            values.append(_firestore_typed_value(item))
        return {"arrayValue": {"values": values}}
    if t == "dict":
        return {"mapValue": {"fields": _firestore_typed_fields(val)}}
    # Fallback: treat as string.
    return {"stringValue": str(val)}

# _firestore_typed_fields converts a dict of raw values into Firestore
# typed fields (each value wrapped).
def _firestore_typed_fields(fields):
    result = {}
    for k in fields:
        result[k] = _firestore_typed_value(fields[k])
    return result

# _firestore_unwrap_value extracts the raw value from a Firestore typed
# value wrapper (the inverse of _firestore_typed_value).
def _firestore_unwrap_value(typed):
    if typed == None:
        return None
    if "stringValue" in typed:
        return typed["stringValue"]
    if "integerValue" in typed:
        return _to_int(typed["integerValue"])
    if "booleanValue" in typed:
        return typed["booleanValue"]
    if "doubleValue" in typed:
        return typed["doubleValue"]
    if "nullValue" in typed:
        return None
    if "arrayValue" in typed:
        arr = typed["arrayValue"]
        values = arr.get("values", [])
        result = []
        for v in values:
            result.append(_firestore_unwrap_value(v))
        return result
    if "mapValue" in typed:
        mv = typed["mapValue"]
        return _firestore_unwrap_fields(mv.get("fields", {}))
    if "timestampValue" in typed:
        return typed["timestampValue"]
    return None

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
