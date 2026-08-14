# Dataverse handler — Microsoft Power Platform Dataverse entities (accounts).
#
# GET    .../accounts              → OData {value:[...]} ($select, $count, paged)
# GET    .../accounts({accountid}) → single account
# POST   .../accounts              → create (201 + Location)
# PATCH  .../accounts({accountid}) → update (204)
# DELETE .../accounts({accountid}) → delete (204)

# _accounts returns the accounts collection, seeded once from _ACCOUNTS. The
# accountid is the storage key, so CRUD targets it directly.
def _accounts():
    c = store_collection("dataverse_accounts")
    if store_kv_get("pp", "accounts_seeded") == None:
        for a in _ACCOUNTS:
            seed = {"id": a["accountid"]}
            for k in a:
                seed[k] = a[k]
            c.insert(seed)
        store_kv_set("pp", "accounts_seeded", "1")
    return c

# _select_fields projects each doc to the OData $select comma-separated fields.
def _select_fields(docs, sel):
    if sel == None or sel == "":
        return docs
    fields = [f.strip() for f in sel.split(",")]
    out = []
    for d in docs:
        proj = {}
        for f in fields:
            if f in d:
                proj[f] = d[f]
        out.append(proj)
    return out

def on_list_accounts(req):
    err = _require_bearer(req)
    if err != None:
        return err

    docs = _accounts().list()
    base_path = "/v2/environments/" + req["params"]["env"] + "/api/data/v9.2/accounts"

    # Real Dataverse OData query options, applied before paging:
    # $filter, $orderby, $skip (paging uses $top/$skipToken).
    docs = _apply_odata_filters(req, docs)
    docs = _apply_odata_orderby(req, docs)
    skip = _to_int(_get_query(req, "$skip", ""))
    if skip > 0:
        docs = docs[skip:]

    total = len(docs)
    page, next_link = _list_page(req, docs, base_path)

    q = req.get("query")
    sel = ""
    if q != None:
        sel = q.get("$select", "")

    resp = {
        "@odata.context": "https://example.api.crm.dynamics.com/api/data/v9.2/$metadata#accounts",
        "value": _select_fields(page, sel),
    }
    if q != None and q.get("$count") == "true":
        resp["@odata.count"] = total
    if next_link != None:
        resp["@odata.nextLink"] = next_link
    return respond(200, resp)

# --- OData query-option helpers ---

# _apply_odata_filters maps a Dataverse $filter (subset: field eq/ne/gt/ge/
# lt/le 'value' or number, contains/startswith/endswith functions, AND'ed
# clauses) to query_select triples, applied before paging.
def _apply_odata_filters(req, docs):
    flt = _get_query(req, "$filter", "")
    if flt == None or flt == "":
        return docs
    f = []
    for clause in _split_and(flt):
        trip = _parse_odata_clause(clause)
        if trip != None:
            f.append(trip)
    if len(f) == 0:
        return docs
    return query_select(docs, f, None, "", None, None, None)

# _apply_odata_orderby maps $orderby ("name desc, revenue") to successive
# query_select sorts (clauses applied right-to-left; stable sorting preserves
# earlier keys).
def _apply_odata_orderby(req, docs):
    ob = _get_query(req, "$orderby", "")
    if ob == None or ob == "":
        return docs
    clauses = []
    for part in ob.split(","):
        part = part.strip()
        if part == "":
            continue
        bits = part.split(" ")
        field = bits[0]
        dir = "asc"
        if len(bits) > 1 and bits[1].lower() == "desc":
            dir = "desc"
        clauses.append([field, dir])
    i = len(clauses) - 1
    while i >= 0:
        docs = query_select(docs, None, clauses[i][0], clauses[i][1], None, None, None)
        i = i - 1
    return docs

# _split_and splits an OData $filter on top-level " and " separators.
def _split_and(flt):
    parts = []
    current = ""
    in_quote = False
    i = 0
    while i < len(flt):
        ch = flt[i]
        if ch == "'":
            in_quote = not in_quote
        if not in_quote and (i + 5 <= len(flt)) and flt[i:i + 5].lower() == " and ":
            parts.append(current)
            current = ""
            i = i + 5
            continue
        current = current + ch
        i = i + 1
    parts.append(current)
    return parts

# _parse_odata_clause parses one OData filter clause into a query_select
# [field, op, value] triple, or None when unsupported.
def _parse_odata_clause(clause):
    clause = clause.strip()
    if clause == "":
        return None

    # Function calls: contains(field,'v') / startswith / endswith.
    for fn, op in [["contains(", "contains"], ["startswith(", "startswith"], ["endswith(", "endswith"]]:
        if clause.lower().startswith(fn):
            inner = clause[len(fn):]
            if inner.endswith(")"):
                inner = inner[:len(inner) - 1]
            comma = inner.find(",")
            if comma < 0:
                return None
            field = inner[:comma].strip()
            val = inner[comma + 1:].strip()
            if len(val) >= 2 and val.startswith("'") and val.endswith("'"):
                val = val[1:len(val) - 1]
            return [field, op, val]

    # Relational: field op value.
    for op_text, op in [[" eq ", "="], [" ne ", "!="], [" gt ", ">"], [" ge ", ">="], [" lt ", "<"], [" le ", "<="]]:
        idx = clause.find(op_text)
        if idx >= 0:
            field = clause[:idx].strip()
            val = clause[idx + len(op_text):].strip()
            quoted = len(val) >= 2 and val.startswith("'") and val.endswith("'")
            if quoted:
                val = val[1:len(val) - 1]
            elif op == "=" or op == "!=" or _all_digits(val):
                # Unquoted numbers must compare as ints against numeric fields
                # (cross-type = is false in query_select); for ordering ops a
                # numeric string already compares numerically, but converting
                # is harmless and exact.
                val = _to_int(val)
            return [field, op, val]
    return None

# _all_digits reports whether s is non-empty and all decimal digits.
def _all_digits(s):
    if s == "":
        return False
    for i in range(len(s)):
        ch = s[i]
        if ch < "0" or ch > "9":
            return False
    return True

# GET .../accounts({accountid})
def on_retrieve_account(req):
    err = _require_bearer(req)
    if err != None:
        return err

    id = req["params"]["accountid"]
    a = _accounts().get(id)
    if a == None:
        return respond(404, {"error": {"code": "0x80040217", "message": "account With Id = " + id + " Does Not Exist"}})
    return respond(200, a)

# POST .../accounts
def on_create_account(req):
    err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}
    if body.get("accountid", "") == "":
        body["accountid"] = "acc-" + str(store_kv_incr("pp", "account_seq"))
    body["id"] = body["accountid"]
    _accounts().insert(body)
    return respond(201, body, {
        "Location": "/v2/environments/" + req["params"]["env"] + "/api/data/v9.2/accounts(" + body["accountid"] + ")",
        "OData-Version": "4.0",
    })

# PATCH .../accounts({accountid})
def on_update_account(req):
    err = _require_bearer(req)
    if err != None:
        return err

    id = req["params"]["accountid"]
    c = _accounts()
    a = c.get(id)
    if a == None:
        return respond(404, {"error": {"code": "0x80040217", "message": "account With Id = " + id + " Does Not Exist"}})

    body = req["body"]
    if body != None:
        for k in body:
            a[k] = body[k]
        c.update(id, a)
    return respond(204, None)

# DELETE .../accounts({accountid})
def on_delete_account(req):
    err = _require_bearer(req)
    if err != None:
        return err

    id = req["params"]["accountid"]
    c = _accounts()
    if c.get(id) == None:
        return respond(404, {"error": {"code": "0x80040217", "message": "account With Id = " + id + " Does Not Exist"}})
    c.delete(id)
    return respond(204, None)
