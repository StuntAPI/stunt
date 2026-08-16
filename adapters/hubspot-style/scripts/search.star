# Search handler — HubSpot's CRM v3 filterGroups search model.
#
# POST /crm/v3/objects/{objectType}/search
#   {
#     "filterGroups": [
#       {"filters": [
#         {"propertyName": "lastname", "operator": "EQ", "value": "Green"},
#         {"propertyName": "amount",   "operator": "BETWEEN",
#          "lowValue": 1000, "highValue": 5000}
#       ]},
#       {"filters": [{"propertyName": "email", "operator": "IN",
#                     "values": ["a@example.com", "b@example.com"]}]}
#     ],
#     "sorts":     [{"propertyName": "createdate", "direction": "DESCENDING"}],
#     "properties": ["firstname", "lastname"],
#     "limit": 10,
#     "after": 0
#   }
#   -> {"total": n, "results": [...], "paging": {"next": {"after", "link"}} | None}
#
# filterGroups semantics, like the real API: filters are AND'ed WITHIN a
# group, groups are OR'ed BETWEEN groups. Every operator maps to
# query_select filter triples; a group's triples are combined into a single
# AND'ed query_select call, and the per-group results are OR-scanned with
# first-seen de-duplication by record id.
#
# Operators: EQ, NEQ, LT, LTE, GT, GTE, CONTAINS_TOKEN, IN, NOT_IN,
# BETWEEN, HAS_PROPERTY, NOT_HAS_PROPERTY. Because CRM properties are
# stringly-typed, comparisons run against per-property shadow fields built
# once per search (see _enrich_for_search):
#
#   _f.v.<prop>    stringified value    (EQ / NEQ / IN / NOT_IN)
#   _f.tok.<prop>  lowercased value     (CONTAINS_TOKEN — case-insensitive)
#   _f.has.<prop>  True when set        (HAS_PROPERTY / NOT_HAS_PROPERTY)
#
# LT/LTE/GT/GTE/BETWEEN run against the raw properties.<prop> value so
# query_select's typed comparison handles numeric strings (amount "5000"
# vs 3000) and RFC3339 date strings (lexicographic) alike. Shadow fields
# never reach responses — _record_shape whitelists keys.
#
# Archived records are excluded, like the real search endpoint. Anything
# structurally invalid (unknown operator, IN without values, limit > 100,
# unparsable body) answers 400 with the HubSpot error envelope; an unknown
# object type answers 404 OBJECT_NOT_FOUND.

# Shared helpers from lib.star.

# _SEARCH_LIMIT_MAX caps limit per page, like the real endpoint.
_SEARCH_LIMIT_MAX = 100

def on_search(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _hs_error(404, "The requested object type was not found.", "OBJECT_NOT_FOUND")

    body, berr = _json_body_or_error(req)
    if berr != None:
        return berr

    groups, gerr = _parse_filter_groups(body)
    if gerr != None:
        return gerr
    sorts, serr = _parse_sorts(body)
    if serr != None:
        return serr
    wanted, werr = _parse_search_properties(body)
    if werr != None:
        return werr
    limit, after, perr = _parse_search_paging(body)
    if perr != None:
        return perr

    docs = col.list()
    docs = query_select(docs, [["archived", "=", False]], None, "", None, None, None)

    # Collect the referenced property names and build the shadow fields.
    props = []
    for g in groups:
        for f in g:
            name = f["propertyName"]
            if name not in props:
                props.append(name)
    if len(props) > 0:
        docs = _enrich_for_search(docs, props)

    # Group-AND (one query_select per group) then OR-scan across groups,
    # preserving first-seen order and de-duplicating by record id.
    seen = {}
    matched = []
    for g in groups:
        triples = []
        for f in g:
            _search_filter_triples(f, triples)
        sub = docs
        if len(triples) > 0:
            sub = query_select(docs, triples, "", "", None, None, None)
        for d in sub:
            key = d.get("id", "")
            if key not in seen:
                seen[key] = True
                matched.append(d)
    if len(groups) == 0:
        # No filterGroups: the real endpoint returns every record.
        matched = docs

    # sorts: applied last-to-first over query_select's stable sort so a
    # multi-key sort composes lexicographically.
    for i in range(len(sorts) - 1, -1, -1):
        field = _sort_field(sorts[i]["propertyName"])
        if field != "":
            direction = "asc"
            if sorts[i].get("direction", "ASCENDING") == "DESCENDING":
                direction = "desc"
            matched = query_select(matched, None, field, direction, None, None, None)

    total = len(matched)

    # after/limit paging via query_select slicing.
    paged = query_select(matched, None, None, None, limit, after, None)

    results = []
    for d in paged:
        results.append(_record_shape(d))
    if wanted != None and len(wanted) > 0:
        results = _project_properties(results, wanted)

    resp = {"total": total, "results": results}
    if after + limit < total:
        nxt = str(after + limit)
        resp["paging"] = {"next": {"after": nxt, "link": "/crm/v3/objects/" + obj_type + "/search"}}
    else:
        resp["paging"] = None

    return respond(200, resp)

# --- parsing / validation ---

# _parse_filter_groups validates the filterGroups body field. Returns
# (groups, None) where groups is a list of groups, each a list of filter
# dicts, or (None, 400-response) on any structural problem. A missing
# filterGroups is valid and matches everything.
def _parse_filter_groups(body):
    if "filterGroups" not in body:
        return [], None
    val = body["filterGroups"]
    if val == None:
        return [], None
    if type(val) != "list":
        return None, _hs_error(400, "The 'filterGroups' field must be an array of filter groups.", "VALIDATION")
    groups = []
    for gv in val:
        if type(gv) != "dict":
            return None, _hs_error(400, "Each filter group must be an object with a 'filters' array.", "VALIDATION")
        fs = gv.get("filters", [])
        if fs == None:
            fs = []
        if type(fs) != "list":
            return None, _hs_error(400, "The 'filters' field of a filter group must be an array.", "VALIDATION")
        group = []
        for f in fs:
            if type(f) != "dict":
                return None, _hs_error(400, "Each filter must be an object.", "VALIDATION")
            name = f.get("propertyName", "")
            if name == "" or type(name) != "string":
                return None, _hs_error(400, "Each filter requires a 'propertyName'.", "VALIDATION")
            op = f.get("operator", "")
            verr = _validate_filter_operator(f, op)
            if verr != None:
                return None, verr
            group.append(f)
        groups.append(group)
    return groups, None

# _validate_filter_operator checks that op is a known HubSpot search
# operator and that its value payload is present. Returns None or a
# 400-response.
def _validate_filter_operator(f, op):
    bad = _hs_error(400, "The search operator '" + op + "' is not recognized.", "VALIDATION")
    if op == "EQ" or op == "NEQ" or op == "LT" or op == "LTE" or op == "GT" or op == "GTE" or op == "CONTAINS_TOKEN":
        if "value" not in f:
            return _hs_error(400, "The '" + op + "' operator requires a 'value'.", "VALIDATION")
        return None
    if op == "IN" or op == "NOT_IN":
        if "values" not in f or type(f["values"]) != "list" or len(f["values"]) == 0:
            return _hs_error(400, "The '" + op + "' operator requires a non-empty 'values' array.", "VALIDATION")
        return None
    if op == "BETWEEN":
        if "lowValue" not in f or "highValue" not in f:
            return _hs_error(400, "The 'BETWEEN' operator requires 'lowValue' and 'highValue'.", "VALIDATION")
        return None
    if op == "HAS_PROPERTY" or op == "NOT_HAS_PROPERTY":
        return None
    return bad

# _parse_sorts validates the sorts body field. Returns (sorts, None) where
# each sort is {"propertyName": str, "direction": "ASCENDING"|"DESCENDING"}.
def _parse_sorts(body):
    if "sorts" not in body:
        return [], None
    val = body["sorts"]
    if val == None:
        return [], None
    if type(val) != "list":
        return None, _hs_error(400, "The 'sorts' field must be an array.", "VALIDATION")
    sorts = []
    for sv in val:
        if type(sv) != "dict":
            return None, _hs_error(400, "Each sort must be an object with a 'propertyName'.", "VALIDATION")
        name = sv.get("propertyName", "")
        if name == "" or type(name) != "string":
            return None, _hs_error(400, "Each sort requires a 'propertyName'.", "VALIDATION")
        direction = sv.get("direction", "ASCENDING")
        if direction == None:
            direction = "ASCENDING"
        sorts.append({"propertyName": name, "direction": direction})
    return sorts, None

# _parse_search_properties validates the properties projection list.
# Returns (names_or_None, None) or (None, 400-response).
def _parse_search_properties(body):
    if "properties" not in body:
        return None, None
    val = body["properties"]
    if val == None:
        return None, None
    if type(val) != "list":
        return None, _hs_error(400, "The 'properties' field must be an array of property names.", "VALIDATION")
    wanted = []
    for pv in val:
        if type(pv) != "string" or pv == "":
            return None, _hs_error(400, "The 'properties' field must be an array of property names.", "VALIDATION")
        wanted.append(pv)
    return wanted, None

# _parse_search_paging validates the body limit/after. Returns
# (limit, after, None) or (0, 0, 400-response).
def _parse_search_paging(body):
    limit = 10
    if "limit" in body:
        lv = body["limit"]
        if type(lv) == "int":
            limit = lv
        elif type(lv) == "string":
            limit = _to_int(lv)
        else:
            return 0, 0, _search_limit_error()
    if limit < 1 or limit > _SEARCH_LIMIT_MAX:
        return 0, 0, _search_limit_error()
    after = 0
    if "after" in body:
        av = body["after"]
        if type(av) == "int":
            after = av
        elif type(av) == "string":
            after = _to_int(av)
        else:
            return 0, 0, _hs_error(400, "The 'after' field must be a non-negative integer.", "VALIDATION")
        if after < 0:
            after = 0
    return limit, after, None

def _search_limit_error():
    return _hs_error(400, "The 'limit' field must be an integer between 1 and " + str(_SEARCH_LIMIT_MAX) + ".", "VALIDATION")

# --- operator -> query_select triple mapping ---

# _search_filter_triples appends the query_select [field, op, value]
# triples for one validated filter to out. See the header comment for the
# shadow-field model.
def _search_filter_triples(f, out):
    op = f["operator"]
    name = f["propertyName"]
    if op == "EQ":
        out.append(["_f.v." + name, "=", _stringify_scalar(f["value"])])
    elif op == "NEQ":
        out.append(["_f.v." + name, "!=", _stringify_scalar(f["value"])])
    elif op == "LT":
        out.append(["properties." + name, "<", f["value"]])
    elif op == "LTE":
        out.append(["properties." + name, "<=", f["value"]])
    elif op == "GT":
        out.append(["properties." + name, ">", f["value"]])
    elif op == "GTE":
        out.append(["properties." + name, ">=", f["value"]])
    elif op == "IN":
        vals = []
        for v in f["values"]:
            vals.append(_stringify_scalar(v))
        out.append(["_f.v." + name, "in", vals])
    elif op == "NOT_IN":
        # NOT_IN == AND of "!= each value" (exact complement of IN).
        for v in f["values"]:
            out.append(["_f.v." + name, "!=", _stringify_scalar(v)])
    elif op == "BETWEEN":
        out.append(["properties." + name, ">=", f["lowValue"]])
        out.append(["properties." + name, "<=", f["highValue"]])
    elif op == "CONTAINS_TOKEN":
        out.append(["_f.tok." + name, "contains", _stringify_scalar(f["value"]).lower()])
    elif op == "HAS_PROPERTY":
        out.append(["_f.has." + name, "=", True])
    elif op == "NOT_HAS_PROPERTY":
        # A missing _f.has.<prop> key fails "=" but passes "!=", which is
        # exactly HAS_PROPERTY's complement.
        out.append(["_f.has." + name, "!=", True])

# --- shadow fields ---

# _enrich_for_search copies each doc and adds the _f shadow tree for the
# referenced property names. Shadow fields are query_select-addressable via
# dotted paths ("_f.v.lastname"); internal names never collide with real
# HubSpot properties (which use underscores, not dots).
def _enrich_for_search(docs, props):
    out = []
    for d in docs:
        nd = {}
        for k, v in d.items():
            nd[k] = v
        p = d.get("properties", {})
        if p == None:
            p = {}
        vmap = {}
        tokmap = {}
        hasmap = {}
        for name in props:
            if name in p:
                s = _stringify_scalar(p[name])
                vmap[name] = s
                tokmap[name] = s.lower()
                hasmap[name] = True
        nd["_f"] = {"v": vmap, "tok": tokmap, "has": hasmap}
        out.append(nd)
    return out

# _sort_field maps a HubSpot sort propertyName to a query_select order_by
# field on the stored doc shape.
def _sort_field(name):
    if name == "createdate" or name == "hs_createdate":
        return "createdAt"
    if name == "lastmodifieddate" or name == "hs_lastmodifieddate":
        return "updatedAt"
    if name == "id":
        return "id"
    return "properties." + name
