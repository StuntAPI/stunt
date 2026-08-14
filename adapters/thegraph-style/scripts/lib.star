# Shared library for thegraph-style adapter scripts.
#
# This file is preloaded by stunt before each handler script. Its top-level
# definitions are available to all handlers as predeclared builtins.

# --- seeded subgraph IDs ---

# Uniswap V3 style subgraph.
SUBGRAPH_UNISWAP_V3 = "5zvR82QoaXYxfyKOCH8Qfl6pUCWd7YFXq56Y3ZSDXx2W"
# ENS style subgraph.
SUBGRAPH_ENS = "5XqPmWe6gZyrTtFjASCbxgykJ7KbAA8puFezV8vsJoEB"

# --- optional API-key validation --------------------------------------------
#
# The Graph's hosted-service subgraph endpoints (the /subgraphs/id/{id}
# shape this adapter models) are public — no auth required. The Graph
# gateway, by contrast, authenticates requests with an API key sent as
# "Authorization: Bearer <key>". This adapter mirrors both: a request
# WITHOUT an Authorization header stays anonymous/public; a request WITH
# one must present a known, unexpired key or gets a 401 GraphQL errors
# envelope. Known keys live in the "graph" KV namespace under "tok:<key>"
# with the expiry as unix seconds (far-future, computed at runtime — never
# a hardcoded epoch).

# Well-known static test API key, seeded once on first request (see
# _seed_api_keys) so clients that present a key have a working credential
# while any other key is rejected with 401.
_TEST_API_KEY = "mock-graph-api-key"

# _seed_api_keys inserts the well-known test API key into the KV store
# exactly once per instance (guarded by the "auth_seeded" flag).
def _seed_api_keys():
    if store_kv_get("graph", "auth_seeded") == "yes":
        return
    store_kv_set("graph", "auth_seeded", "yes")
    store_kv_set("graph", "tok:" + _TEST_API_KEY, str(clock.now_unix() + 3600 * 24 * 365 * 10))

# _graph_unauthorized returns a Graph-gateway-style 401 GraphQL error.
def _graph_unauthorized():
    return respond(401, {
        "errors": [{"message": "valid API key expected"}],
    })

# _auth_check returns None when the request may proceed anonymously or with
# a known key, or a 401 response when an unknown/expired key is presented.
def _auth_check(req):
    headers = req.get("headers")
    if headers == None:
        return None
    auth = headers.get("Authorization", "")
    if auth == None or auth == "":
        return None
    token = ""
    if auth.startswith("Bearer "):
        token = auth[7:]
    if token == "":
        return _graph_unauthorized()
    _seed_api_keys()
    exp = store_kv_get("graph", "tok:" + token)
    if exp != None and clock.now_unix() <= _to_int(exp):
        return None
    return _graph_unauthorized()

# --- string helpers ---

# _contains checks if a string contains a substring.
def _contains(s, sub):
    if s == None or sub == None:
        return False
    if len(sub) == 0:
        return True
    if len(sub) > len(s):
        return False
    for i in range(len(s) - len(sub) + 1):
        match = True
        for j in range(len(sub)):
            if s[i + j] != sub[j]:
                match = False
                break
        if match:
            return True
    return False

# _to_int parses a decimal string to int. Returns 0 for None, empty, or
# non-numeric input.
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

# _str converts any value to string (handles None and numbers).
def _str(v):
    if v == None:
        return ""
    if type(v) == "int" or type(v) == "float":
        # Starlark's str() works, but we use the conversion for safety.
        return _int_to_str(int(v))
    return v

# _int_to_str converts an int to a decimal string.
def _int_to_str(n):
    if n == 0:
        return "0"
    digits = "0123456789"
    out = ""
    while n > 0:
        out = digits[n % 10] + out
        n = n // 10
    return out

# --- GraphQL query parsing ---

# _extract_fields extracts the list of requested field names from a GraphQL
# query fragment like "pools(first:5, orderBy:volumeUSD){id token0{symbol} token1{symbol} totalValueLockedUSD}".
# Returns a list of top-level field names (without nesting).
def _extract_fields(fragment):
    fields = []
    i = 0
    depth = 0
    current = ""
    started = False
    while i < len(fragment):
        ch = fragment[i]
        if ch == "{":
            if depth == 0 and started and current != "":
                fields.append(_trim(current))
            depth = depth + 1
            current = ""
            started = True
        elif ch == "}":
            depth = depth - 1
            if depth == 0:
                # Next entity or end.
                pass
            current = ""
            started = False
        elif depth > 0 and started:
            if ch == "," or ch == "\n" or ch == " " or ch == "\t":
                if current != "":
                    fields.append(_trim(current))
                    current = ""
            else:
                current = current + ch
        else:
            current = current + ch
        i = i + 1
    # Catch trailing field.
    if current != "" and depth == 0:
        fields.append(_trim(current))
    return fields

# _trim removes leading/trailing whitespace from a string.
def _trim(s):
    start = 0
    end = len(s)
    while start < end:
        ch = s[start]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            start = start + 1
        else:
            break
    while end > start:
        ch = s[end - 1]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            end = end - 1
        else:
            break
    return s[start:end]

# _extract_arg_raw extracts an argument's raw string value from a GraphQL
# field header, tolerating whitespace around the colon and separators.
# e.g. from "pools(first: 5, orderBy: volumeUSD)" key="first" → "5".
def _extract_arg_raw(header, key):
    pattern = key + ":"
    idx = _find_str(header, pattern)
    if idx < 0:
        return ""
    i = idx + len(pattern)
    while i < len(header) and (header[i] == " " or header[i] == "\t" or header[i] == "\n"):
        i = i + 1
    val = ""
    while i < len(header):
        ch = header[i]
        if ch == "," or ch == ")":
            break
        val = val + ch
        i = i + 1
    return _trim(val)

# _extract_arg_int extracts an integer argument from a GraphQL field header.
# e.g. from "pools(first: 5, orderBy:volumeUSD)" with key="first" → 5.
def _extract_arg_int(header, key):
    return _to_int(_extract_arg_raw(header, key))

# _extract_arg_str extracts a string argument from a GraphQL field header.
# e.g. from "pools(orderBy: volumeUSD)" with key="orderBy" → "volumeUSD".
def _extract_arg_str(header, key):
    return _extract_arg_raw(header, key)

# _find_str finds the index of a substring, or -1.
def _find_str(s, sub):
    if len(sub) == 0:
        return 0
    for i in range(len(s) - len(sub) + 1):
        match = True
        for j in range(len(sub)):
            if s[i + j] != sub[j]:
                match = False
                break
        if match:
            return i
    return -1

# _has_field checks if a field name appears in the GraphQL query fragment.
def _has_field(query, field):
    return _contains(query, field)

# _ends_with reports whether s ends with suffix.
def _ends_with(s, suffix):
    if len(suffix) > len(s):
        return False
    return s[len(s) - len(suffix):] == suffix

# --- GraphQL where/orderBy/orderDirection/skip support ---
#
# The Graph entity queries accept collection-level args that this simulator
# maps onto the query_select builtin:
#   pools(first: 10, skip: 5, orderBy: volumeUSD, orderDirection: desc,
#         where: { feeTier: "3000", token0: "0x...", txCount_gt: 100 })

# _extract_where_block returns the raw text inside the where: { ... } block
# belonging to the given entity's field, or "". The block may contain nested
# braces in exotic queries; balanced-brace scanning handles that.
def _extract_where_block(query, entity):
    ent_idx = _find_str(query, entity)
    if ent_idx < 0:
        return ""
    idx = _find_str(query[ent_idx:], "where:")
    if idx < 0:
        return ""
    idx = ent_idx + idx
    # Find the opening brace after "where:".
    i = idx + 6
    while i < len(query):
        if query[i] == "{":
            break
        i = i + 1
    if i >= len(query):
        return ""
    depth = 1
    i = i + 1
    out = ""
    while i < len(query) and depth > 0:
        ch = query[i]
        if ch == "{":
            depth = depth + 1
        elif ch == "}":
            depth = depth - 1
            if depth == 0:
                break
        out = out + ch
        i = i + 1
    return out

# _parse_where parses a where block ("feeTier: \"3000\", txCount_gt: 100")
# into query_select [field, op, value] triples. Supported field suffixes map
# to query_select ops: _gt _gte _lt _lte _not _in (list or single value),
# _contains, _starts_with, _ends_with. A list value under _not_in (or _not)
# is tagged with the synthetic op "not_in", which _apply_graph_args resolves
# via a manual exclusion pass (query_select has no not-in op). Unquoted
# values are treated as strings (stored entity fields are strings).
def _parse_where(block, entity):
    filters = []
    i = 0
    n = len(block)
    while i < n:
        ch = block[i]
        if ch == " " or ch == "," or ch == "\n" or ch == "\t" or ch == "}":
            i = i + 1
            continue
        field = ""
        while i < n and block[i] != ":":
            field = field + block[i]
            i = i + 1
        if i >= n:
            break
        i = i + 1
        while i < n and (block[i] == " " or block[i] == "\n" or block[i] == "\t"):
            i = i + 1
        if i >= n:
            break
        value = ""
        is_list = False
        if block[i] == '"':
            i = i + 1
            while i < n and block[i] != '"':
                value = value + block[i]
                i = i + 1
            i = i + 1
        elif block[i] == "[":
            is_list = True
            i = i + 1
            items = []
            while i < n and block[i] != "]":
                if block[i] == '"':
                    i = i + 1
                    item = ""
                    while i < n and block[i] != '"':
                        item = item + block[i]
                        i = i + 1
                    i = i + 1
                    items.append(item)
                else:
                    i = i + 1
            i = i + 1
            value = items
        else:
            while i < n and block[i] != "," and block[i] != "}" and block[i] != " " and block[i] != "\n":
                value = value + block[i]
                i = i + 1

        field = _trim(field)
        f = field
        op = "="
        if _ends_with(field, "_gte"):
            f = field[:len(field) - 4]
            op = ">="
        elif _ends_with(field, "_lte"):
            f = field[:len(field) - 4]
            op = "<="
        elif _ends_with(field, "_gt"):
            f = field[:len(field) - 3]
            op = ">"
        elif _ends_with(field, "_lt"):
            f = field[:len(field) - 3]
            op = "<"
        elif _ends_with(field, "_not_in"):
            f = field[:len(field) - 7]
            op = "!="
        elif _ends_with(field, "_not"):
            f = field[:len(field) - 4]
            op = "!="
        elif _ends_with(field, "_contains"):
            f = field[:len(field) - 9]
            op = "contains"
        elif _ends_with(field, "_starts_with"):
            f = field[:len(field) - 12]
            op = "startswith"
        elif _ends_with(field, "_ends_with"):
            f = field[:len(field) - 10]
            op = "endswith"

        if is_list:
            # A list value under _not_in (or _not) means "exclude these".
            # query_select has no not-in op, so mark it and let
            # _apply_graph_args run a manual exclusion pass.
            if op == "!=":
                op = "not_in"
            else:
                op = "in"
                # Positive list filters are written as field_in: [...] —
                # strip the suffix (mirrors the scalar _in branch below,
                # which this branch otherwise shadows). _not_in was already
                # handled above, so do not re-strip it.
                if _ends_with(field, "_in") and not _ends_with(field, "_not_in"):
                    f = field[:len(field) - 3]
        elif _ends_with(field, "_in") and not _ends_with(field, "_not_in"):
            f = field[:len(field) - 3]
            op = "in"
            value = [value]

        # Stored pool docs keep referenced tokens as token0_id/token1_id.
        if entity == "pools" and (f == "token0" or f == "token1"):
            f = f + "_id"

        filters.append([f, op, value])
    return filters

# _exclude_in removes docs whose field value equals any entry in values.
# Docs missing the field are kept (a missing value is not in the list).
def _exclude_in(docs, field, values):
    out = []
    for d in docs:
        v = d.get(field, None)
        excluded = False
        if v != None:
            for j in range(len(values)):
                if _str(v) == _str(values[j]):
                    excluded = True
                    break
        if not excluded:
            out.append(d)
    return out

# _apply_graph_args applies the entity collection-level GraphQL args
# (where/orderBy/orderDirection/first/skip) to a raw stored-entity list via
# query_select, before per-field projection. Mirrors the real Graph node
# ordering: filter, sort, then slice. header is the entity's field header
# (e.g. "pools(first:5, orderBy:volumeUSD)") as extracted by the caller.
def _apply_graph_args(query, entity, docs, header):
    where = _parse_where(_extract_where_block(query, entity), entity)

    # query_select has no not-in op: apply _not_in exclusions manually
    # BEFORE query_select so filter-then-sort-then-slice ordering holds.
    select_filters = []
    for i in range(len(where)):
        if where[i][1] == "not_in":
            docs = _exclude_in(docs, where[i][0], where[i][2])
        else:
            select_filters.append(where[i])

    order_by = _extract_arg_str(header, "orderBy")
    order_dir = _extract_arg_str(header, "orderDirection")
    first = _extract_arg_int(header, "first")
    skip = _extract_arg_int(header, "skip")

    if order_by == "":
        order_by = None
    if order_dir == "":
        order_dir = ""
    if first <= 0:
        first = None
    if skip <= 0:
        skip = None

    return query_select(docs, select_filters if len(select_filters) > 0 else None, order_by, order_dir, first, skip, None)
