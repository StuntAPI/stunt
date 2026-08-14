# Leads handlers — Marketo lead CRUD + sync.
#
# GET  /rest/v1/leads?filterType=email&filterValues=... -> filtered leads
# POST /rest/v1/leads                                    -> create/update lead
# GET  /rest/v1/leads/{id}                               -> get single lead
# GET  /rest/v1/leads/{id}.json                          -> get single lead (.json)
# POST /rest/v1/leads.json                               -> sync leads (bulk upsert)
#
# Marketo envelope: {success:true, requestId, result:[...], moreResult:false}

# Shared helpers from lib.star.

# _lead_shape builds the Marketo lead shape from a stored doc.
def _lead_shape(doc):
    return {
        "id": doc.get("id", ""),
        "email": doc.get("email", ""),
        "firstName": doc.get("firstName", ""),
        "lastName": doc.get("lastName", ""),
        "createdAt": doc.get("createdAt", _now()),
        "updatedAt": doc.get("updatedAt", _now()),
    }

def on_list_leads(req):
    ok, err = _require_auth(req)
    if not ok:
        return _marketo_unauth()
    if _check_quota():
        return _quota_err()

    col = store_collection("leads")
    docs = col.list()

    result = []
    for d in docs:
        result.append(_lead_shape(d))

    # Real Get Leads params (filterType/filterValues, fields) applied before
    # paging, like the real API.
    result = _apply_lead_filters(req, result)

    # Apply Marketo paging (batchSize + nextPageToken) after filtering.
    page, next_cursor, more = _list_page(req, result)

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": page,
        "nextPageToken": next_cursor,
        "moreResult": more,
    })

# _apply_lead_filters maps the real Get Multiple Leads query params to
# query_select: filterType + filterValues (match ANY of the comma-separated
# values against the named field) and the fields projection (comma-separated
# list of lead fields to return; id is always included).
def _apply_lead_filters(req, leads):
    f = []
    filter_type = _get_query(req, "filterType", "")
    filter_values = _get_query(req, "filterValues", "")
    if filter_type != "" and filter_values != "":
        values = []
        for v in _split(filter_values, ","):
            v = _trim(v)
            if v != "":
                values.append(v)
        if len(values) > 0:
            f.append([filter_type, "in", values])

    flt = None
    if len(f) > 0:
        flt = f

    fields_param = _get_query(req, "fields", "")
    fields = None
    if fields_param != "":
        wanted = []
        for part in _split(fields_param, ","):
            part = _trim(part)
            if part != "" and part != "id":
                wanted.append(part)
        if len(wanted) > 0 or _contains(fields_param, "id"):
            # Marketo always returns id plus the requested fields.
            fields = ["id"]
            for w in wanted:
                fields.append(w)

    return query_select(leads, flt, None, "", None, None, fields)

def on_create_lead(req):
    ok, err = _require_auth(req)
    if not ok:
        return _marketo_unauth()
    if _check_quota():
        return _quota_err()

    body = _get_body(req)
    action = body.get("action", "createOnly")
    lead_data = body

    # Marketo create can have the fields directly on the body or inside an
    # "input" array.
    if "input" in lead_data:
        inputs = lead_data.get("input", [])
        results = []
        for inp in inputs:
            results.append(_upsert_lead(inp, action))
        return respond(200, {
            "requestId": _request_id(),
            "success": True,
            "result": results,
            "moreResult": False,
        })

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": [_upsert_lead(lead_data, action)],
        "moreResult": False,
    })

# on_sync_leads handles POST /rest/v1/leads.json (the Marketo sync leads endpoint).
def on_sync_leads(req):
    ok, err = _require_auth(req)
    if not ok:
        return _marketo_unauth()
    if _check_quota():
        return _quota_err()

    body = _get_body(req)
    action = body.get("action", "createOrUpdate")
    inputs = body.get("input", [])
    if inputs == None:
        inputs = []

    results = []
    for inp in inputs:
        results.append(_upsert_lead(inp, action))

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": results,
        "moreResult": False,
    })

def on_get_lead(req):
    return _get_lead(req)

def on_get_lead_json(req):
    return _get_lead(req)

def _get_lead(req):
    ok, err = _require_auth(req)
    if not ok:
        return _marketo_unauth()
    if _check_quota():
        return _quota_err()

    lead_id = req["params"].get("id", "")
    col = store_collection("leads")
    doc = col.get(lead_id)
    if doc == None:
        return respond(404, {
            "requestId": _request_id(),
            "success": False,
            "errors": [{"code": "604", "message": "Lead not found"}],
            "moreResult": False,
        })

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": [_lead_shape(doc)],
        "moreResult": False,
    })

# _upsert_lead inserts or updates a lead, returning the Marketo result shape.
def _upsert_lead(inp, action):
    col = store_collection("leads")
    email = inp.get("email", "")

    if action == "createOnly":
        lead_id = _next_id("lead")
        doc = {
            "id": lead_id,
            "email": email,
            "firstName": inp.get("firstName", ""),
            "lastName": inp.get("lastName", ""),
            "createdAt": _now(),
            "updatedAt": _now(),
        }
        col.insert(doc)
        return _lead_shape(doc)

    if action == "updateOnly" or action == "createOrUpdate":
        # Try to find existing by email.
        if email != "":
            docs = col.list()
            for d in docs:
                if d.get("email", "") == email:
                    updated = {
                        "id": d.get("id", ""),
                        "email": email,
                        "firstName": inp.get("firstName", d.get("firstName", "")),
                        "lastName": inp.get("lastName", d.get("lastName", "")),
                        "createdAt": d.get("createdAt", _now()),
                        "updatedAt": _now(),
                    }
                    col.update(d.get("id", ""), updated)
                    return _lead_shape(updated)

    # Create new (createOrUpdate or createDuplicateStandard).
    lead_id = _next_id("lead")
    doc = {
        "id": lead_id,
        "email": email,
        "firstName": inp.get("firstName", ""),
        "lastName": inp.get("lastName", ""),
        "createdAt": _now(),
        "updatedAt": _now(),
    }
    col.insert(doc)
    return _lead_shape(doc)

