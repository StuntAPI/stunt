# Shared library for salesforce-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# Salesforce uses OAuth2 bearer tokens. Access tokens are session IDs
# (00D-prefixed for the org). API calls require Authorization: Bearer <token>.

# _bearer extracts the Bearer token from the Authorization header. Returns
# "" if absent.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _require_token validates the bearer token. Returns (token_doc, error_resp).
# If the token is absent or invalid, error_resp is a 401 response.
def _require_token(req):
    token = _bearer(req)
    if token == "":
        return None, _auth_error()
    c = store_collection("access_tokens")
    doc = c.get(token)
    if doc == None:
        return None, _auth_error()
    return doc, None

# _auth_error returns the Salesforce 401 error response (array envelope).
def _auth_error():
    return respond(401, [{
        "message": "Session expired or invalid",
        "errorCode": "INVALID_SESSION_ID",
        "fields": [],
    }])

# _sf_error returns a Salesforce-style error array response.
def _sf_error(status, message, code):
    return respond(status, [{
        "message": message,
        "errorCode": code,
        "fields": [],
    }])

# _now returns a synthetic timestamp.
def _now():
    return "2024-01-01T00:00:00.000+0000"

# _next_id generates a Salesforce-style ID: 3-char key prefix + a 15-char
# alphanumeric suffix. Uses the KV counter to ensure uniqueness.
_KEY_PREFIXES = {
    "Account": "001",
    "Contact": "003",
    "Opportunity": "006",
    "Lead": "00Q",
    "User": "005",
}

# Base-62 alphabet for the ID suffix.
_B62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

def _next_id(obj_type):
    prefix = _KEY_PREFIXES.get(obj_type, "000")
    n = store_kv_incr("salesforce", obj_type + "_seq")
    # Encode n as base-62, left-padded to 15 chars (synthetic).
    suffix = ""
    v = n
    for _ in range(15):
        suffix = _B62[v % 62] + suffix
        v = v // 62
    return prefix + suffix

# _collection_for maps an object type to its backing collection name.
_COLLECTIONS = {
    "Account": "accounts",
    "Contact": "contacts",
    "Opportunity": "opportunities",
    "Lead": "leads",
    "User": "users",
}

# _collection returns the store_collection for the given object type.
def _collection(obj_type):
    name = _COLLECTIONS.get(obj_type, "")
    if name == "":
        return None
    return store_collection(name)

# _obj_type_from_path extracts the object type from the URL path.
# Paths look like /services/data/v60.0/sobjects/Account/{id}
def _obj_type_from_path(req):
    path = req["path"]
    # /services/data/v60.0/sobjects/<Type>[/id]
    parts = _split(path, "/")
    # find "sobjects" then take next token
    for i in range(len(parts)):
        if parts[i] == "sobjects" and i + 1 < len(parts):
            return parts[i + 1]
    return ""

# _object_name_from_collection returns the object type name (e.g. "Account")
# from a collection name (e.g. "accounts"). Falls back to "" if not found.
def _obj_type_from_collection(name):
    for k, v in _COLLECTIONS.items():
        if v == name:
            return k
    return ""

# _lower returns a lowercased copy of the string.
def _lower(s):
    out = ""
    for i in range(len(s)):
        ch = s[i]
        code = ord(ch)
        if code >= 65 and code <= 90:
            code = code + 32
        out += chr(code)
    return out

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

# _split_fields parses the SELECT field list from a SOQL query. Returns a
# list of field names (stripped of whitespace). Handles "SELECT Id, Name FROM"
# and "SELECT * FROM".
def _split_fields(select_part):
    fields_str = _replace_ci(select_part, "SELECT")
    # Split on commas
    raw = _split(fields_str, ",")
    result = []
    for f in raw:
        f = _trim(f)
        if f != "":
            result.append(f)
    return result

# _trim strips leading/trailing spaces from a string.
def _trim(s):
    start = 0
    end = len(s)
    while start < end and s[start] == " ":
        start = start + 1
    while end > start and s[end - 1] == " ":
        end = end - 1
    return s[start:end]

# _replace replaces the first occurrence of needle in haystack (case-insensitive).
def _replace_ci(haystack, needle):
    lower_h = _lower(haystack)
    lower_n = _lower(needle)
    idx = _index(lower_h, lower_n)
    if idx < 0:
        return haystack
    return haystack[:idx] + haystack[idx + len(needle):]

# _replace replaces first occurrence of substring (exact match).
def _replace(haystack, needle):
    idx = _index(haystack, needle)
    if idx < 0:
        return haystack
    return haystack[:idx] + haystack[idx + len(needle):]

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

# _parse_soql extracts the FROM entity and SELECT fields from a SOQL query.
# Returns (entity_name, [fields], where_id). where_id is the Id value from
# "WHERE Id = '...'" or "" if not present. No full SOQL parsing — just
# pattern matching.
def _parse_soql(query_str):
    q = _lower(query_str)

    # Extract FROM entity.
    from_idx = _index(q, " from ")
    if from_idx < 0:
        from_idx = _index(q, "\nfrom ")
    if from_idx < 0:
        return "", [], ""
    from_start = from_idx + 6  # skip " from "
    # Read the entity name token.
    entity = ""
    i = from_start
    while i < len(query_str):
        ch = query_str[i]
        if ch == " " or ch == "\n" or ch == "\t":
            break
        entity = entity + ch
        i = i + 1

    # Extract SELECT fields (everything between SELECT and FROM).
    select_idx = _index(q, "select")
    if select_idx < 0:
        select_idx = 0
    select_end = from_idx
    select_part = query_str[select_idx:select_end]
    fields = _split_fields(select_part)
    # Normalize field names (strip quotes, brackets).
    normalized = []
    for f in fields:
        f = _trim(f)
        if _lower(f) != "select" and f != "":
            normalized.append(f)

    # Extract WHERE Id = 'value' for single-record queries.
    where_id = ""
    where_idx = _index(q, " where ")
    if where_idx >= 0:
        rest = query_str[where_idx:]
        id_idx = _index(_lower(rest), "id")
        if id_idx >= 0:
            # Find the quoted value after "Id = '"
            eq_idx = _index(rest[id_idx:], "=")
            if eq_idx >= 0:
                after_eq = rest[id_idx + eq_idx:]
                # Find first single quote.
                q1 = _index(after_eq, "'")
                if q1 >= 0:
                    after_q1 = after_eq[q1 + 1:]
                    q2 = _index(after_q1, "'")
                    if q2 >= 0:
                        where_id = after_q1[:q2]

    return entity, normalized, where_id

# _project builds a record dict with only the requested fields. If fields
# contains "*", returns all fields from the source doc.
def _project(doc, fields, obj_type):
    rec = {}
    # attributes block
    rec["attributes"] = {
        "type": obj_type,
        "url": "/services/data/v60.0/sobjects/" + obj_type + "/" + doc.get("Id", ""),
    }
    if len(fields) == 0 or (len(fields) == 1 and fields[0] == "*"):
        # Return all fields.
        for k, v in doc.items():
            rec[k] = v
        return rec
    for f in fields:
        if doc.get(f) != None:
            rec[f] = doc[f]
    return rec

# _get_body safely returns the request body dict.
def _get_body(req):
    body = req.get("body")
    if body == None:
        return {}
    return body
