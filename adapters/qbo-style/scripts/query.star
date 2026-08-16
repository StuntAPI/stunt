# Query handler — SQL-like query endpoint.
#
# GET/POST /v3/company/{realmId}/query?query=SELECT * FROM Customer
# -> { QueryResponse: { Customer: [...] / Invoice: [...] }, time }
#
# We pattern-match the entity name (Customer/Invoice/etc) and return the
# appropriate collection. The documented SQL clauses are honored where they
# map onto query_select:
#   WHERE <field> <op> <value> [AND ...]   (ops = != > >= < <= LIKE IN)
#   ORDER BY <field> [ASC|DESC]            (QBO accepts ORDERBY too)
#   MAXRESULTS <n>
# OR expressions and other advanced SQL are ignored (unfiltered superset),
# preserving the prior behavior.

# Shared helpers (_bearer, _require_token, _realm_matches, _detect_entity,
# _get_query, _fault, _now, _index, _contains, _trim, _split, _lower) from
# lib.star.

def on_query(req):
    token_doc, err = _require_token(req)
    if err != None:
        return err

    realm_id = req["params"]["realmId"]
    if not _realm_matches(token_doc, realm_id):
        return _auth_fault()

    query_str = _get_query(req)
    entity = _detect_entity(query_str)
    if entity == "":
        return _fault(400, "400", "Invalid query", "Could not determine entity from query: " + query_str)

    parsed = _q_parse(query_str)

    if entity == "Customer":
        c = store_collection("customers")
        # Default reads exclude deactivated customers (Active=false), like the
        # bare read endpoint; an explicit WHERE on Active widens the view so
        # `WHERE Active = False` surfaces deactivated customers.
        docs = _q_apply(_q_default_active(parsed), c.list())
        return respond(200, {"QueryResponse": {"Customer": docs, "maxResults": len(docs)}, "time": _now()})

    if entity == "Invoice":
        c = store_collection("invoices")
        docs = _q_apply(parsed, c.list())
        return respond(200, {"QueryResponse": {"Invoice": docs, "maxResults": len(docs)}, "time": _now()})

    # Unsupported entity → empty response.
    return respond(200, {"QueryResponse": {"maxResults": 0}, "time": _now()})

# _q_default_active adds an implicit Active = True clause to a parsed query
# UNLESS its WHERE already constrains Active (an explicit filter replaces the
# default, mirroring how QBO clients opt into deactivated rows). Returns the
# (possibly amended) parsed dict; a None filter means "no WHERE" and becomes
# the single Active clause.
def _q_default_active(parsed):
    if parsed["filter"] != None:
        for t in parsed["filter"]:
            if _lower(t[0]) == "active":
                return parsed
    amended = {"filter": [["Active", "=", True]], "order_by": parsed["order_by"], "order_dir": parsed["order_dir"], "limit": parsed["limit"]}
    if parsed["filter"] != None:
        clauses = [["Active", "=", True]]
        for t in parsed["filter"]:
            clauses.append(t)
        amended["filter"] = clauses
    return amended

# --- SQL clause parsing (QBO v3 query grammar subset) ---

# _q_parse splits the query into filter/order/limit pieces. It returns a
# dict with keys "filter" (list of triples or None), "order_by", "order_dir"
# and "limit" (int or None).
def _q_parse(query_str):
    parsed = {"filter": None, "order_by": "", "order_dir": "", "limit": None}
    if query_str == "":
        return parsed
    low = _lower(query_str)

    where_i = _index(low, " where ")
    order_i = _q_clause_index(low, [" order by ", " orderby "])
    max_i = _index(low, " maxresults ")

    # WHERE body spans from after " where " to the next clause keyword.
    if where_i >= 0:
        start = where_i + 7
        end = len(query_str)
        if order_i > where_i:
            end = order_i
        if max_i > where_i and max_i < end:
            end = max_i
        parsed["filter"] = _q_where(_trim(query_str[start:end]))

    # ORDER BY body.
    if order_i >= 0:
        ostart = order_i
        olow = low[ostart:ostart + 11]
        if olow == " order by ":
            ostart = ostart + 11
        else:
            ostart = ostart + 9
        oend = len(query_str)
        if max_i > order_i:
            oend = max_i
        obody = _trim(query_str[ostart:oend])
        parts = _split(obody, " ")
        if len(parts) >= 1 and parts[0] != "":
            # Multiple sort keys are not supported; only the first is used.
            parsed["order_by"] = _trim(parts[0])
        if len(parts) >= 2:
            d = _lower(parts[1])
            if d == "desc":
                parsed["order_dir"] = "desc"
            elif d == "asc":
                parsed["order_dir"] = "asc"

    # MAXRESULTS body (a bare integer).
    if max_i >= 0:
        mstart = max_i + 12
        mbody = _trim(query_str[mstart:])
        mparts = _split(mbody, " ")
        if len(mparts) >= 1:
            n = _q_int(mparts[0])
            if n > 0:
                parsed["limit"] = n

    return parsed

# _q_clause_index returns the earliest index of any of the needles, or -1.
def _q_clause_index(low, needles):
    best = -1
    for n in needles:
        i = _index(low, n)
        if i >= 0 and (best < 0 or i < best):
            best = i
    return best

# _q_where parses the WHERE body into query_select triples. Conditions are
# AND'ed; a body containing OR is left unparsed (None) so the handler returns
# the unfiltered superset rather than a wrong subset.
def _q_where(body):
    if body == "":
        return None
    if _contains(_lower(body), " or "):
        return None
    triples = []
    for cond in _q_split_and(body):
        t = _q_cond(cond)
        if t != None:
            triples.append(t)
    if len(triples) == 0:
        return None
    return triples

# _q_split_and splits a WHERE body on the AND keyword (case-insensitive),
# skipping single-quoted literals.
def _q_split_and(body):
    parts = []
    current = ""
    in_q = False
    i = 0
    n = len(body)
    low = _lower(body)
    while i < n:
        ch = body[i]
        if in_q:
            current = current + ch
            if ch == "'":
                in_q = False
            i = i + 1
            continue
        if ch == "'":
            in_q = True
            current = current + ch
            i = i + 1
            continue
        if i + 5 <= n and low[i:i + 5] == " and ":
            parts.append(current)
            current = ""
            i = i + 5
            continue
        current = current + ch
        i = i + 1
    parts.append(current)
    return parts

# _q_cond parses one condition ("field op value") into a triple, or None.
def _q_cond(cond):
    cond = _trim(cond)
    if cond == "":
        return None
    sym = _q_sym(cond)
    field = ""
    raw = ""
    op = ""
    if sym[0] >= 0:
        field = _trim(cond[:sym[0]])
        op = sym[1]
        raw = cond[sym[0] + len(sym[1]):]
    else:
        sp = _index(cond, " ")
        if sp < 0:
            return None
        field = _trim(cond[:sp])
        rest = _trim(cond[sp + 1:])
        sp2 = _index(rest, " ")
        if sp2 < 0:
            return None
        word = _lower(_trim(rest[:sp2]))
        raw = rest[sp2 + 1:]
        if word == "like":
            op = "like"
        elif word == "in":
            op = "in"
        elif word == "eq":
            op = "="
        elif word == "ne":
            op = "!="
        else:
            return None
    if field == "":
        return None
    if op == "in":
        vals = _q_in_list(raw)
        if len(vals) == 0:
            return None
        return [field, "in", vals]
    return [field, op, _q_value(raw)]

# _q_sym finds the earliest symbolic comparison operator, preferring
# two-character forms at the same index.
def _q_sym(cond):
    best_i = -1
    best_op = ""
    for sym in ["<=", ">=", "!=", "<", ">", "="]:
        i = _index(cond, sym)
        if i < 0:
            continue
        if best_i < 0 or i < best_i:
            best_i = i
            best_op = sym
    return [best_i, best_op]

# _q_in_list parses "(v1, v2, ...)" into a list of typed values.
def _q_in_list(raw):
    raw = _trim(raw)
    if len(raw) >= 2 and raw[0] == "(" and raw[len(raw) - 1] == ")":
        raw = raw[1:len(raw) - 1]
    vals = []
    for part in _split(raw, ","):
        part = _trim(part)
        if part != "":
            vals.append(_q_value(part))
    return vals

# _q_value converts a SQL literal to its stored-field type: quoted values
# stay strings, true/false become bools, null becomes None, bare numerics
# become ints/floats (Balance/TotalAmt are stored as numbers).
def _q_value(raw):
    raw = _trim(raw)
    if len(raw) >= 2 and raw[0] == "'" and raw[len(raw) - 1] == "'":
        return raw[1:len(raw) - 1]
    if len(raw) >= 2 and raw[0] == '"' and raw[len(raw) - 1] == '"':
        return raw[1:len(raw) - 1]
    low = _lower(raw)
    if low == "true":
        return True
    if low == "false":
        return False
    if low == "null":
        return None
    if _q_is_int(raw):
        return _q_int(raw)
    if _q_is_float(raw):
        return _q_float(raw)
    return raw

# _q_is_int reports whether s is a (possibly negative) decimal integer.
def _q_is_int(s):
    if s == "":
        return False
    i = 0
    if s[0] == "-":
        i = 1
    if i >= len(s):
        return False
    for j in range(i, len(s)):
        if s[j] < "0" or s[j] > "9":
            return False
    return True

# _q_is_float reports whether s is a (possibly negative) decimal float.
def _q_is_float(s):
    if s == "":
        return False
    i = 0
    if s[0] == "-":
        i = 1
    if i >= len(s):
        return False
    seen_dot = False
    seen_digit = False
    for j in range(i, len(s)):
        ch = s[j]
        if ch == ".":
            if seen_dot:
                return False
            seen_dot = True
        elif ch >= "0" and ch <= "9":
            seen_digit = True
        else:
            return False
    return seen_digit

# _q_int parses a decimal integer string.
def _q_int(s):
    n = 0
    neg = False
    i = 0
    if i < len(s) and s[i] == "-":
        neg = True
        i = 1
    while i < len(s):
        n = n * 10 + (ord(s[i]) - ord("0"))
        i = i + 1
    if neg:
        return -n
    return n

# _q_float parses a decimal float string.
def _q_float(s):
    whole = 0
    frac = 0.0
    scale = 0.1
    neg = False
    i = 0
    if i < len(s) and s[i] == "-":
        neg = True
        i = 1
    while i < len(s) and s[i] != ".":
        whole = whole * 10 + (ord(s[i]) - ord("0"))
        i = i + 1
    i = i + 1
    while i < len(s):
        frac = frac + (ord(s[i]) - ord("0")) * scale
        scale = scale / 10.0
        i = i + 1
    v = whole + frac
    if neg:
        return -v
    return v

# _q_apply runs the parsed clauses over the docs via query_select.
def _q_apply(parsed, docs):
    filt = parsed["filter"]
    if filt != None:
        filt = _q_coerce(filt, docs)
    return query_select(docs, filt, parsed["order_by"], parsed["order_dir"], parsed["limit"], None, None)

# _q_coerce retypes filter values against the stored field type so bare
# numerics match string-typed ids (stored as strings) and quoted numerics
# match numeric fields (Balance/TotalAmt are stored as numbers).
def _q_coerce(triples, docs):
    for t in triples:
        ft = _q_field_type(docs, t[0])
        if ft == None:
            continue
        if t[1] == "in":
            vals = t[2]
            out = []
            for v in vals:
                out.append(_q_coerce_value(v, ft))
            t[2] = out
        elif t[1] == "=" or t[1] == "!=":
            t[2] = _q_coerce_value(t[2], ft)
    return triples

# _q_field_type returns the type of a field from the first doc that has it.
def _q_field_type(docs, field):
    for d in docs:
        if field in d:
            return type(d[field])
    return None

# _q_coerce_value converts v to match the stored field type where the
# conversion is unambiguous.
def _q_coerce_value(v, ft):
    if v == None:
        return v
    if ft == type(0) or ft == type(1.0):
        if type(v) == type(""):
            if _q_is_int(v):
                return _q_int(v)
            if _q_is_float(v):
                return _q_float(v)
        return v
    if ft == type(""):
        if type(v) == type(0):
            return str(v)
        return v
    return v
