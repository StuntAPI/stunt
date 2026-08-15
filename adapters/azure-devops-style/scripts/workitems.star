# Work items handlers — Azure DevOps WIT (Work Item Tracking).
#
# GET   /{org}/{project}/_apis/wit/workitems?ids=1,2,3       → {value:[...], count}
# GET   /{org}/{project}/_apis/wit/workitems/{id}            → work item resource
# POST  /{org}/{project}/_apis/wit/workitems/{type}          → create (patch-document body)
# PATCH /{org}/{project}/_apis/wit/workitems/{id}            → update (patch-document body)
# POST  /{org}/{project}/_apis/wit/wiql                      → WIQL subset
#
# NOTE: Azure DevOps work item IDs are integers, but the stunt collection
# store requires string IDs. We store the numeric ID in "wi_id" and the
# collection "id" as a string, then return the integer in responses.

# on_get_workitem returns a single work item by id.
def on_get_workitem(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    wi_id_str = req["params"]["id"]
    wc = store_collection("workitems")
    wi = wc.get(wi_id_str)
    if wi != None:
        return respond(200, _workitem_resource(wi))

    return respond(404, {
        "$id": "1",
        "innerException": None,
        "message": "The work item " + wi_id_str + " does not exist.",
        "typeName": "Microsoft.TeamFoundation.WorkItemTracking.Client.WorkItemDoesNotExistException",
        "typeKey": "WorkItemDoesNotExistException",
        "errorCode": 0,
        "eventId": 3200,
    })

# on_list_workitems implements "Get work items" (ids= comma list). Optional
# fields=System.Title,... projects the returned fields.
def on_list_workitems(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    ids_raw = _get_query(req, "ids", "")
    if ids_raw == "":
        return _wit_fault(400, "You must pass in at least one work item id.", "ArgumentException")

    ids = {}
    for part in ids_raw.split(","):
        p = part.strip()
        if p != "":
            ids[p] = True

    fields_raw = _get_query(req, "fields", "")
    want_fields = []
    if fields_raw != "":
        for f in fields_raw.split(","):
            f = f.strip()
            if f != "":
                want_fields.append(f)

    wc = store_collection("workitems")
    value = []
    for wi in wc.list():
        if wi.get("id", "") not in ids:
            continue
        if len(want_fields) > 0:
            value.append(_workitem_resource_projection(wi, want_fields))
        else:
            value.append(_workitem_resource(wi))

    if len(value) == 0:
        return _wit_fault(404, "The work items either do not exist or you do not have permission to read them.", "WorkItemDoesNotExistException")

    return respond(200, {"count": len(value), "value": value})

# on_create_workitem implements the real create model: POST to the work item
# TYPE route with a JSON patch-document body ([{op, path, from, value}]).
def on_create_workitem(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    wi_type = _norm_work_item_type(req["params"]["type"])

    body = req["body"]
    if body == None:
        body = {}

    wi_num = store_kv_incr("azure-devops", "wi_seq") + 1
    wi_id_str = str(wi_num)

    now_iso = _now_iso()
    fields = {
        "System.AreaPath": "MyFirstProject",
        "System.TeamProject": "MyFirstProject",
        "System.IterationPath": "MyFirstProject",
        "System.WorkItemType": wi_type,
        "System.State": "New",
        "System.Reason": "New",
        "System.CreatedDate": now_iso,
        "System.CreatedBy": "Test User <test@example.com>",
        "System.ChangedDate": now_iso,
        "System.ChangedBy": "Test User <test@example.com>",
    }

    # Process the patch-document operations (JSON array bodies arrive
    # wrapped as {_batch: [...]} by the engine).
    ops = _body_ops(body)
    for op in ops:
        if op == None:
            continue
        path = op.get("path", "")
        value = op.get("value", "")
        if path[:1] == "/":
            path = path[1:]
        # Map fields/System.Title -> System.Title
        if path[:7] == "fields/":
            fields[path[7:]] = value

    new_wi = {
        "id": wi_id_str,
        "wi_id": wi_num,
        "rev": 1,
        "fields": fields,
        "relations": [],
        "url": "https://dev.azure.com/mock-org/MyFirstProject/_apis/wit/workItems/" + wi_id_str,
    }

    wc = store_collection("workitems")
    wc.insert(new_wi)

    # Emit service-hook event (workitem.created) if a subscription exists.
    title = fields.get("System.Title", "")
    _emit_service_hook(
        "workitem.created",
        "Work item " + wi_id_str + " created: " + title,
        _workitem_resource(new_wi),
    )

    return respond(200, _workitem_resource(new_wi))

# on_update_workitem applies a patch-document to an existing work item
# (add/replace/remove on /fields/* and /relations[-|/{i}]).
def on_update_workitem(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    wi_id_str = req["params"]["id"]
    wc = store_collection("workitems")
    wi = wc.get(wi_id_str)
    if wi == None:
        return _wit_fault(404, "The work item " + wi_id_str + " does not exist.", "WorkItemDoesNotExistException")

    body = req["body"]
    if body == None:
        body = {}

    fields = wi.get("fields", {})
    relations = wi.get("relations", [])
    if relations == None:
        relations = []
    changed = 0

    for op in _body_ops(body):
        if op == None:
            continue
        o = op.get("op", "")
        path = op.get("path", "")
        value = op.get("value", None)
        if path[:1] == "/":
            path = path[1:]

        if path[:7] == "fields/":
            field = path[7:]
            if o in ("add", "replace"):
                if fields.get(field, None) != value:
                    fields[field] = value
                    changed = changed + 1
            elif o == "remove":
                fields = _dict_delete(fields, field)
                changed = changed + 1
            elif o == "test":
                if fields.get(field, None) != value:
                    return _wit_fault(400, "The test operation for path '" + path + "' failed.", "PatchOperationFailedException")
            else:
                return _wit_fault(400, "Unknown patch operation '" + o + "'.", "PatchOperationFailedException")
        elif path[:9] == "relations":
            rest = path[9:]
            if o == "add" and rest in ("-", "", "/-"):
                relations.append(value)
                changed = changed + 1
            elif o == "remove" and rest[:1] != "" and rest.find("/") == 0:
                idx = _to_int(rest[1:])
                if idx < 0 or idx >= len(relations):
                    return _wit_fault(400, "The index '" + rest + "' is out of range.", "PatchOperationFailedException")
                nxt = []
                for i in range(len(relations)):
                    if i != idx:
                        nxt.append(relations[i])
                relations = nxt
                changed = changed + 1
            else:
                return _wit_fault(400, "The patch operation '" + o + "' on path '" + path + "' is not supported.", "PatchOperationFailedException")
        else:
            return _wit_fault(400, "The patch path '" + path + "' is not supported.", "PatchOperationFailedException")

    if changed > 0:
        wi["rev"] = wi.get("rev", 1) + 1
        wi["fields"] = fields
        wi["relations"] = relations
        wi["fields"]["System.ChangedDate"] = _now_iso()
        wi["fields"]["System.ChangedBy"] = "Test User <test@example.com>"
        wc.update(wi["id"], wi)
        _emit_service_hook(
            "workitem.updated",
            "Work item " + wi_id_str + " updated: " + fields.get("System.Title", ""),
            _workitem_resource(wi),
        )

    return respond(200, _workitem_resource(wi))

# on_wiql implements a WIQL subset: SELECT [System.Id] FROM WorkItems WHERE
# simple predicates AND'ed together (=/!=/</>/<=/>=/CONTAINS over field
# values, single-quoted strings or bare numbers), optional ORDER BY
# [Field] ASC|DESC. Matching runs through the typed query_select builtin.
def on_wiql(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}
    query = body.get("query", "")
    if query == None:
        query = ""

    ql = " " + query.lower() + " "
    if ql.find(" from workitems") < 0:
        return _wit_fault(400, "The query is not a supported WIQL SELECT ... FROM WorkItems statement.", "BadRequestException")

    # WHERE clause (cut at ORDER BY if present).
    where_clause = ""
    p = ql.find(" where ")
    if p >= 0:
        where_clause = query[p + 6:]
        ob = (" " + where_clause.lower() + " ").find(" order by ")
        if ob >= 0:
            where_clause = where_clause[:ob - 1]

    filt = []
    err_msg = _wiql_clauses(where_clause, filt)
    if err_msg != "":
        return _wit_fault(400, err_msg, "BadRequestException")

    # Flatten each work item's dotted field names into query_select-addressable
    # keys ("System.State" -> "fld_System_State").
    wc = store_collection("workitems")
    items = []
    for wi in wc.list():
        wid = _as_int(wi.get("wi_id", wi.get("id", 0)))
        d = {"id": wid, "fld_System_Id": wid}
        f = wi.get("fields", {})
        for k in f:
            d["fld_" + k.replace(".", "_")] = f[k]
        items.append(d)

    result = query_select(items, filt) if len(filt) > 0 else items

    order_field, order_dir = _wiql_order_by(query)
    if order_field != "":
        result = query_select(result, None, order_field, order_dir)

    work_items = []
    for it in result:
        wid = _as_int(it.get("id", 0))
        work_items.append({
            "id": wid,
            "url": "https://dev.azure.com/mock-org/MyFirstProject/_apis/wit/workItems/" + str(wid),
        })

    top = _to_int(_get_query(req, "$top", ""))
    if top > 0 and top < len(work_items):
        work_items = work_items[:top]

    return respond(200, {"count": len(work_items), "workItems": work_items})

# --- WIQL parsing helpers ---

# _wiql_clauses parses the WHERE clause into query_select filter triples.
# Returns "" on success or an error message.
def _wiql_clauses(where_clause, filt):
    if where_clause == "":
        return ""
    lower = where_clause.lower()
    if lower.find(" or ") >= 0:
        return "OR is not supported by this WIQL subset."
    parts = _split_and(where_clause)
    for pred in parts:
        msg = _wiql_predicate(pred, filt)
        if msg != "":
            return msg
    return ""

# _split_and splits a clause on case-insensitive " AND ".
def _split_and(s):
    lower = s.lower()
    parts = []
    start = 0
    i = lower.find(" and ", start)
    while i >= 0:
        parts.append(s[start:i])
        start = i + 5
        i = lower.find(" and ", start)
    parts.append(s[start:])
    return parts

# _wiql_predicate parses one "[Field] op value" predicate and appends a
# [field, op, value] triple. Returns "" on success or an error message.
def _wiql_predicate(pred, filt):
    s = pred.strip()
    if s == "":
        return ""
    if s[0] != "[":
        return "Expected a bracketed field reference in '" + s + "'."
    j = s.find("]")
    if j < 0:
        return "Unterminated field reference in '" + s + "'."
    field = s[1:j]
    rest = s[j + 1:].strip()

    low = rest.lower()
    if low.startswith("contains "):
        op = "contains"
        raw_val = rest[9:].strip()
    else:
        op = ""
        k = 0
        while k < len(rest):
            two = rest[k:k + 2]
            if two in ("<=", ">=", "<>", "!="):
                op = two
                raw_val = rest[k + 2:].strip()
                break
            ch = rest[k]
            if ch in ("=", "<", ">"):
                op = ch
                raw_val = rest[k + 1:].strip()
                break
            k = k + 1
        if op == "":
            return "No comparison operator in '" + s + "'."

    value = _wiql_value(raw_val)
    if value == None:
        return "Could not parse the value in '" + s + "'."

    # Bare numbers become ints so equality against (float-round-tripped)
    # stored field values matches numerically.
    if type(value) == "string" and value != "" and _is_digits(value):
        value = _to_int(value)

    if op == "<>":
        sel_op = "!="
    elif op == "!=":
        sel_op = "!="
    else:
        sel_op = op

    filt.append(["fld_" + field.replace(".", "_"), sel_op, value])
    return ""

# _is_digits reports whether s is a non-empty run of ASCII digits.
# (Indexes rather than iterating: strings are not iterable in Starlark.)
def _is_digits(s):
    if s == "":
        return False
    for i in range(len(s)):
        ch = s[i]
        if ch < "0" or ch > "9":
            return False
    return True

# _wiql_value parses a single-quoted string, double-quoted string, bare
# number, or bare token. Returns the value or None.
def _wiql_value(raw):
    if raw == "":
        return None
    if raw[0] == "'" or raw[0] == '"':
        q = raw[0]
        end = raw.find(q, 1)
        if end < 0:
            return None
        return raw[1:end]
    return raw

# _wiql_order_by extracts "ORDER BY [Field] ASC|DESC" → (field, dir).
def _wiql_order_by(query):
    low = query.lower()
    ob = low.find(" order by ")
    if ob < 0:
        return "", ""
    tail = query[ob + 10:].strip()
    j = tail.find("]")
    if tail[:1] != "[" or j < 0:
        return "", ""
    field = "fld_" + tail[1:j].replace(".", "_")
    rest = tail[j + 1:].strip().lower()
    if rest.startswith("desc"):
        return field, "desc"
    return field, "asc"

# --- shared helpers ---

# _body_ops returns the patch-document operation list from a parsed body
# (JSON array bodies arrive wrapped as {_batch: [...]} by the engine).
def _body_ops(body):
    ops = body
    batch = body.get("_batch", None)
    if batch != None:
        ops = batch
    if type(ops) != "list":
        return []
    return ops

# _norm_work_item_type normalizes a work item type route param: accepts
# "Task", "$Task", and the full "$Microsoft.VSTS.WorkItemTypes.Task" form.
def _norm_work_item_type(t):
    if t[:1] == "$":
        t = t[1:]
    j = t.rfind(".")
    if j >= 0:
        t = t[j + 1:]
    return t

def _wit_fault(status, message, type_key):
    return respond(status, {
        "$id": "1",
        "innerException": None,
        "message": message,
        "typeName": "Microsoft.TeamFoundation.WorkItemTracking.Client." + type_key,
        "typeKey": type_key,
        "errorCode": 0,
        "eventId": 3200,
    })

# _workitem_resource builds the API response shape for a work item.
# Returns the integer id (wi_id) in the response, matching real API.
def _workitem_resource(wi):
    wid = _as_int(wi.get("wi_id", wi.get("id", 0)))
    res = {
        "id": wid,
        "rev": wi.get("rev", 1),
        "fields": wi.get("fields", {}),
        "_links": {
            "self": {
                "href": wi.get("url", ""),
            },
            "html": {
                "href": "https://dev.azure.com/mock-org/MyFirstProject/_workitems/edit/" + str(wid),
            },
        },
        "url": wi.get("url", ""),
    }
    relations = wi.get("relations", None)
    if relations != None and len(relations) > 0:
        res["relations"] = relations
    return res

# _workitem_resource_projection is _workitem_resource with fields= projection
# (System.Id/Rev always kept, like the real API keeps identity fields).
def _workitem_resource_projection(wi, want_fields):
    full = _workitem_resource(wi)
    fields = wi.get("fields", {})
    out_fields = {}
    for f in want_fields:
        if f in fields:
            out_fields[f] = fields[f]
    full["fields"] = out_fields
    return full
