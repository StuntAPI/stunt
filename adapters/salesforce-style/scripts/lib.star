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

# Session TTL for issued access tokens, in seconds (real Salesforce session
# timeout default: 2 hours).
_SESSION_TTL = 3600 * 2

# _require_token validates the bearer token. Returns (token_doc, error_resp).
# If the token is absent, unknown, or expired (expires_at vs the engine
# clock), error_resp is a 401 response.
def _require_token(req):
    token = _bearer(req)
    if token == "":
        return None, _auth_error()
    c = store_collection("access_tokens")
    doc = c.get(token)
    if doc == None:
        return None, _auth_error()
    expires_at = _parse_num(doc.get("expires_at", 0))
    if expires_at != None and expires_at > 0 and clock.now_unix() > expires_at:
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

# _now returns the current time as an ISO-8601 timestamp (live clock —
# CreatedDate/LastModifiedDate reflect request time; seeded records are
# stamped once at seed time and stay stable).
def _now():
    return clock.now_rfc3339()

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
    suffix = _b62_pad(n, 15)
    return prefix + suffix

# _b62_pad encodes n in base-62, left-padded to width chars.
def _b62_pad(n, width):
    s = ""
    v = n
    while v > 0:
        s = _B62[v % 62] + s
        v = v // 62
    while len(s) < width:
        s = "0" + s
    return s

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

# _obj_type_for_id infers the object type from a record Id's 3-char key
# prefix (the same prefixes real Salesforce Ids carry). Collections delete
# requests pass bare Ids, so the prefix is the only routing signal.
def _obj_type_for_id(record_id):
    if record_id == None or len(record_id) < 3:
        return ""
    prefix = record_id[:3]
    for k, v in _KEY_PREFIXES.items():
        if v == prefix:
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

# _to_int parses a decimal string to int (0 for None/empty/non-numeric).
def _to_int(s):
    if s == None:
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return n
    return n

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
        return "", [], "", "", "", 0, 0
    from_start = from_idx + 6  # skip " from "
    entity = ""
    i = from_start
    while i < len(query_str):
        ch = query_str[i]
        if ch == " " or ch == "\n" or ch == "\t":
            break
        entity = entity + ch
        i = i + 1

    # SELECT fields (between SELECT and FROM).
    select_idx = _index(q, "select")
    if select_idx < 0:
        select_idx = 0
    select_part = query_str[select_idx:from_idx]
    normalized = []
    for f in _split_fields(select_part):
        f = _trim(f)
        if _lower(f) != "select" and f != "":
            normalized.append(f)

    # Clause keyword positions (lowercased), in order WHERE, ORDER BY, LIMIT, OFFSET.
    n = len(query_str)
    where_idx = _index(q, " where ")
    order_idx = _index(q, " order by ")
    limit_idx = _index(q, " limit ")
    offset_idx = _index(q, " offset ")

    where = ""
    if where_idx >= 0:
        where = _trim(query_str[where_idx + 7:_first_after(where_idx, [order_idx, limit_idx, offset_idx], n)])

    order_field = ""
    order_dir = "asc"
    if order_idx >= 0:
        opart = _trim(query_str[order_idx + 10:_first_after(order_idx, [limit_idx, offset_idx], n)])
        oparts = [p for p in opart.split(" ") if p != ""]
        if len(oparts) > 0:
            order_field = oparts[0]
        for p in oparts[1:]:
            if _lower(p).startswith("d"):
                order_dir = "desc"

    limit = 0
    if limit_idx >= 0:
        limit = _to_int(_trim(query_str[limit_idx + 7:_first_after(limit_idx, [offset_idx], n)]))

    offset = 0
    if offset_idx >= 0:
        offset = _to_int(_trim(query_str[offset_idx + 8:]))

    return entity, normalized, where, order_field, order_dir, limit, offset

# --- SOQL WHERE evaluator (comparators, IN, LIKE, AND/OR; no parentheses) ---
#
# The WHERE clause is COMPILED once per query (_soql_compile -> plan) and the
# resulting plan is matched against each record (_soql_match). Re-parsing the
# clause per record instead blows the engine's per-handler step budget on
# real-sized result sets (200+ rows).

# _first_after returns the smallest index in `indices` that is > target, or default.
def _first_after(target, indices, default):
    best = default
    for idx in indices:
        if idx > target and idx < best:
            best = idx
    return best

# _split_top_level splits s on " "+kw+" " (kw lowercased) at the top level,
# i.e. not inside single-quoted strings (so "or"/"and" inside a value won't split).
def _split_top_level(s, kw):
    needle = " " + kw + " "
    nlen = len(needle)
    segs = []
    cur = ""
    in_quote = False
    ls = _lower(s)
    i = 0
    while i < len(s):
        ch = s[i]
        if ch == "'":
            in_quote = not in_quote
            cur = cur + ch
            i = i + 1
            continue
        if (not in_quote) and i + nlen <= len(s) and ls[i:i + nlen] == needle:
            segs.append(cur)
            cur = ""
            i = i + nlen
            continue
        cur = cur + ch
        i = i + 1
    segs.append(cur)
    return segs

def _soql_eval(where, doc):
    if where == None or where == "":
        return True
    return _soql_match(_soql_compile(where), doc)

# _soql_compile parses a WHERE clause into a plan: an OR-list of AND-lists of
# compiled terms. Each term is a tuple ("pass"|"false"|"cmp"|"like"|"in", ...).
def _soql_compile(where):
    plan = []
    for seg in _split_top_level(where, "or"):
        ands = []
        for term in _split_top_level(seg, "and"):
            ands.append(_soql_term_plan(term))
        plan.append(ands)
    return plan

# _soql_term_plan compiles one comparison term.
def _soql_term_plan(term):
    term = _trim(term)
    if term == "":
        return ("pass",)
    lt = _lower(term)
    if _contains(lt, " in "):
        pos = _index(lt, " in ")
        field = _trim(term[:pos])
        rest = _trim(term[pos + 4:])
        if len(rest) >= 2 and rest[0] == "(":
            rest = rest[1:len(rest) - 1]
        vals = []
        for item in rest.split(","):
            vals.append(_to_cmp_str(_soql_value(_trim(item))))
        return ("in", field, vals)
    if _contains(lt, " like "):
        pos = _index(lt, " like ")
        field = _trim(term[:pos])
        lpat = _lower(str(_soql_value(_trim(term[pos + 6:]))))
        starts = False
        ends = False
        core = lpat
        if len(core) > 0 and core[0] == "%":
            starts = True
            core = core[1:]
        if len(core) > 0 and core[len(core) - 1] == "%":
            ends = True
            core = core[:len(core) - 1]
        return ("like", field, core, starts, ends)
    idx, oplen = _soql_find_op(term)
    if idx < 0:
        return ("false",)
    field = _trim(term[:idx])
    op = term[idx:idx + oplen]
    return ("cmp", field, op, _soql_value(_trim(term[idx + oplen:])))

# _soql_match evaluates a compiled plan against a record: any OR-segment
# whose AND-terms all hold matches.
def _soql_match(plan, doc):
    for ands in plan:
        ok = True
        for t in ands:
            if not _soql_term_match(t, doc):
                ok = False
                break
        if ok:
            return True
    return False

# _soql_term_match evaluates one compiled term against a record.
def _soql_term_match(t, doc):
    kind = t[0]
    if kind == "pass":
        return True
    if kind == "false":
        return False
    if kind == "cmp":
        return _soql_cmp(_soql_field(doc, t[1]), t[2], t[3])
    if kind == "like":
        val = _lower(_to_cmp_str(_soql_field(doc, t[1])))
        core = t[2]
        starts = t[3]
        ends = t[4]
        if starts and ends:
            return _contains(val, core)
        if starts:
            return val.endswith(core)
        if ends:
            return val.startswith(core)
        return val == core
    if kind == "in":
        want = _to_cmp_str(_soql_field(doc, t[1]))
        for v in t[2]:
            if v == want:
                return True
        return False
    return False

# _soql_find_op returns (index, length) of the first comparison operator outside
# quotes, checking two-char ops (<= >= !=) before single-char ones.
def _soql_find_op(term):
    in_quote = False
    i = 0
    while i < len(term):
        ch = term[i]
        if ch == "'":
            in_quote = not in_quote
            i = i + 1
            continue
        if not in_quote:
            two = term[i:i + 2]
            if two == "<=" or two == ">=" or two == "!=":
                return i, 2
            if ch == "=" or ch == "<" or ch == ">":
                return i, 1
        i = i + 1
    return -1, 0

# _soql_field looks up a field on the doc (case-insensitive).
def _soql_field(doc, field):
    if field in doc:
        return doc[field]
    lf = _lower(field)
    for k in doc:
        if _lower(k) == lf:
            return doc[k]
    return None

# _soql_value parses a SOQL literal: 'string', true, false, null, or a bare token.
def _soql_value(raw):
    raw = _trim(raw)
    if raw == "":
        return ""
    if raw[0] == "'":
        if len(raw) >= 2 and raw[len(raw) - 1] == "'":
            return raw[1:len(raw) - 1]
        return raw[1:]
    l = _lower(raw)
    if l == "true":
        return True
    if l == "false":
        return False
    if l == "null":
        return None
    return raw

def _to_cmp_str(v):
    if v == None:
        return ""
    if v == True:
        return "true"
    if v == False:
        return "false"
    return str(v)

# _parse_num parses a signed integer from a value, or None if not numeric.
def _parse_num(v):
    if v == None:
        return None
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    s = str(v)
    if s == "":
        return None
    neg = False
    i = 0
    if s[0] == "-":
        neg = True
        i = 1
    elif s[0] == "+":
        i = 1
    if i >= len(s):
        return None
    n = 0
    while i < len(s):
        ch = s[i]
        if ch < "0" or ch > "9":
            return None
        n = n * 10 + (ord(ch) - ord("0"))
        i = i + 1
    if neg:
        n = -n
    return n

def _soql_cmp(docval, op, val):
    if op == "=":
        return _to_cmp_str(docval) == _to_cmp_str(val)
    if op == "!=":
        return _to_cmp_str(docval) != _to_cmp_str(val)
    dn = _parse_num(docval)
    vn = _parse_num(val)
    if dn != None and vn != None:
        a, b = dn, vn
    else:
        a, b = _to_cmp_str(docval), _to_cmp_str(val)
    if op == "<":
        return a < b
    if op == "<=":
        return a <= b
    if op == ">":
        return a > b
    if op == ">=":
        return a >= b
    return False

# _project builds a record dict with only the requested fields. If fields
# contains "*", returns all public fields from the source doc (internal keys
# — the lowercase storage id and any underscore-prefixed key — are stripped).
def _project(doc, fields, obj_type):
    rec = {}
    # attributes block
    rec["attributes"] = {
        "type": obj_type,
        "url": "/services/data/v60.0/sobjects/" + obj_type + "/" + doc.get("Id", ""),
    }
    if len(fields) == 0 or (len(fields) == 1 and fields[0] == "*"):
        # Return all public fields.
        for k, v in doc.items():
            if k != "id" and not k.startswith("_"):
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

# --- SOQL execution pipeline (shared by /query and queryMore pages) ---
#
# /query batches results like the real API: at most `batchsize` records per
# response (default 200; Sforce-Query-Options header, range 200-2000). When
# more remain, the response carries done:false plus nextRecordsUrl pointing
# at /query/<queryLocator>, and fetching that URL continues the same query.

_QUERY_BATCH_DEFAULT = 200
_QUERY_BATCH_MIN = 200
_QUERY_BATCH_MAX = 2000

# _batch_size reads the Sforce-Query-Options header ("batchsize=500"). Values
# outside the real 200-2000 range are clamped into it; absent header -> 200.
def _batch_size(req):
    h = req["headers"].get("sforce-query-options", "")
    if h == None:
        h = ""
    pos = _index(_lower(h), "batchsize=")
    if pos < 0:
        return _QUERY_BATCH_DEFAULT
    n = _to_int(h[pos + 10:])
    if n < _QUERY_BATCH_MIN:
        return _QUERY_BATCH_MIN
    if n > _QUERY_BATCH_MAX:
        return _QUERY_BATCH_MAX
    return n

# _query_docs runs the full SOQL pipeline (entity, WHERE, ORDER BY, OFFSET,
# LIMIT, soft-delete visibility) and returns the total result set:
#   (docs, entity, fields, err_status, err_message, err_code)
# err_status is 0 on success.
def _query_docs(soql, include_deleted):
    entity, fields, where, order_field, order_dir, limit, offset = _parse_soql(soql)
    if entity == "":
        return [], "", [], 400, "Malformed query: could not determine FROM entity", "INVALID_QUERY"
    col = _collection(entity)
    if col == None:
        return [], "", [], 400, "Entity type '" + entity + "' is not accessible", "INVALID_TYPE"

    docs = col.list()

    # Records created before IsDeleted was tracked (seeds) are live rows.
    for d in docs:
        if d.get("IsDeleted", None) == None:
            d["IsDeleted"] = False

    if not include_deleted:
        docs = [d for d in docs if not d["IsDeleted"]]

    # WHERE (comparators, IN, LIKE, AND/OR) — compiled once, matched per row.
    if where != "":
        plan = _soql_compile(where)
        docs = [d for d in docs if _soql_match(plan, d)]

    # ORDER BY (decorate-sort-undecorate so a missing value sorts first; the
    # index tiebreaker avoids comparing dicts).
    if order_field != "":
        pairs = []
        idx = 0
        for d in docs:
            kv = _soql_field(d, order_field)
            if kv == None:
                kv = ""
            pairs.append((kv, idx, d))
            idx = idx + 1
        pairs = sorted(pairs)
        docs = []
        n = len(pairs)
        if order_dir == "desc":
            i = n - 1
            while i >= 0:
                docs.append(pairs[i][2])
                i = i - 1
        else:
            for p in pairs:
                docs.append(p[2])

    # OFFSET then LIMIT.
    if offset > 0 and offset < len(docs):
        docs = docs[offset:]
    elif offset >= len(docs):
        docs = []
    if limit > 0:
        docs = docs[:limit]

    return docs, entity, fields, 0, "", ""

# _new_locator mints an opaque queryLocator (real ones look like
# "01g" + base62 + "-<offset>": a query id plus the record offset) and stores
# the continuation state under it in the KV store.
def _new_locator(soql, next_index, include_deleted, batch):
    seq = store_kv_incr("salesforce", "query_locator_seq")
    locator = "01g" + _b62_pad(seq, 15) + "-" + str(next_index)
    store_kv_set("salesforce", "qloc_" + locator, json.encode({
        "soql": soql,
        "next": next_index,
        "all_rows": include_deleted,
        "batch": batch,
    }))
    return locator

# _query_page slices one batch out of the total result set and shapes the
# /query response envelope (totalSize = records in this batch, as the real
# API reports).
def _query_page(docs, entity, fields, start, batch, include_deleted, soql):
    end = start + batch
    if end > len(docs):
        end = len(docs)
    records = [_project(d, fields, entity) for d in docs[start:end]]
    out = {
        "totalSize": len(records),
        "records": records,
        "done": True,
    }
    if end < len(docs):
        locator = _new_locator(soql, end, include_deleted, batch)
        out["done"] = False
        out["nextRecordsUrl"] = "/services/data/v60.0/query/" + locator
    return respond(200, out)
