# Leads handlers — Marketo lead CRUD + sync + describe.
#
# GET  /rest/v1/leads?filterType=email&filterValues=... -> filtered leads
# POST /rest/v1/leads                                    -> create/update lead
# GET  /rest/v1/leads/describe                           -> lead field metadata
# GET  /rest/v1/leads/{id}                               -> get single lead
# GET  /rest/v1/leads/{id}.json                          -> get single lead (.json)
# POST /rest/v1/leads.json                               -> sync leads (bulk upsert)
#
# Marketo envelope: {success:true, requestId, result:[...], moreResult:false}

# Shared helpers from lib.star.

# The built-in lead fields every lead carries. Anything else on an input
# record is a CUSTOM field and is preserved verbatim through upserts.
_LEAD_BASE_FIELDS = ["id", "email", "firstName", "lastName", "createdAt", "updatedAt"]

# _lead_shape builds the Marketo lead shape from a stored doc. Custom fields
# stored on the doc (anything outside the base fields that is not a leading-
# underscore simulator key) are returned too, like the real API.
def _lead_shape(doc):
    out = {
        "id": doc.get("id", ""),
        "email": doc.get("email", ""),
        "firstName": doc.get("firstName", ""),
        "lastName": doc.get("lastName", ""),
        "createdAt": doc.get("createdAt", _now()),
        "updatedAt": doc.get("updatedAt", _now()),
    }
    for k in doc:
        if k in _LEAD_BASE_FIELDS:
            continue
        if k[:1] == "_":
            continue
        out[k] = doc[k]
    return out

# on_describe returns the lead field metadata shape (GET /rest/v1/leads/describe):
# displayName, name, dataType, length, and the rest/soap/multipart bindings
# the real describe endpoint returns.
def on_describe(req):
    ok, err = _require_auth(req)
    if not ok:
        return err
    if _check_quota():
        return _quota_err()

    fields = _lead_describe_fields()
    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": fields,
        "moreResult": False,
    })

# _lead_describe_fields returns the metadata entries for the fields this
# simulator supports (built-ins plus the customary custom fields that are
# preserved on upsert).
def _lead_describe_fields():
    specs = [
        # name, displayName, dataType, length, readOnly
        ["id", "Marketo Id", "integer", 0, True],
        ["email", "Email Address", "email", 255, False],
        ["firstName", "First Name", "string", 255, False],
        ["lastName", "Last Name", "string", 255, False],
        ["createdAt", "Created At", "datetime", 0, True],
        ["updatedAt", "Updated At", "datetime", 0, True],
        ["company", "Company Name", "string", 255, False],
        ["phone", "Phone Number", "string", 255, False],
        ["leadSource", "Lead Source", "string", 255, False],
        ["leadScore", "Lead Score", "integer", 0, False],
        ["unsubscribed", "Unsubscribed", "boolean", 0, False],
    ]
    out = []
    for s in specs:
        name = s[0]
        out.append({
            "displayName": s[1],
            "name": name,
            "dataType": s[2],
            "length": s[3],
            "rest": {"name": name, "readOnly": s[4]},
            "soap": {"name": name, "readOnly": s[4]},
            "multipart": {"name": name, "readOnly": s[4]},
        })
    return out

def on_list_leads(req):
    ok, err = _require_auth(req)
    if not ok:
        return err
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
        return err
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
        return err
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
        return err
    if _check_quota():
        return _quota_err()

    lead_id = req["params"].get("id", "")
    # The {id}.json route captures the ".json" suffix in the param — strip
    # it so lookups hit the stored id (same convention as shopify-style).
    if lead_id.endswith(".json"):
        lead_id = lead_id[:-5]
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

# _upsert_lead inserts or updates a lead, returning the per-record Marketo
# sync result shape: {id, status} where status is "created" or "updated", or
# {"status": "skipped", "reasons": [...]} when the action cannot be applied
# (updateOnly on a lead that does not exist -> skipped, code 1013, NEVER a
# silent create). Custom fields on the input record are preserved through
# both creates and updates.
def _upsert_lead(inp, action):
    col = store_collection("leads")
    email = inp.get("email", "")
    if email == None:
        email = ""

    if action == "createOnly":
        doc = _insert_lead(inp, col)
        return {"id": doc.get("id", ""), "status": "created"}

    # updateOnly / createOrUpdate: dedupe against an existing lead by id
    # first (explicit), then by email (the default lookupField).
    existing = None
    lead_id = inp.get("id", "")
    if lead_id != "" and lead_id != None:
        d = col.get(lead_id)
        if d != None:
            existing = d
    if existing == None and email != "":
        for d in col.list():
            if d.get("email", "") == email:
                existing = d
                break

    if existing != None:
        # Merge the input onto the existing lead: every input field (base
        # and custom) overwrites, fields absent from the input (including
        # pre-existing CUSTOM fields) are preserved.
        updated = _lead_shape(existing)
        for k in inp:
            if k == "id":
                continue
            updated[k] = inp[k]
        updated["id"] = existing.get("id", "")
        updated["createdAt"] = existing.get("createdAt", _now())
        updated["updatedAt"] = _now()
        col.update(existing.get("id", ""), updated)
        return {"id": updated.get("id", ""), "status": "updated"}

    if action == "updateOnly":
        return {
            "status": "skipped",
            "reasons": [{"code": "1013", "message": "Lead not found"}],
        }

    # No existing lead and creating is allowed (createOrUpdate /
    # createDuplicateStandard).
    doc = _insert_lead(inp, col)
    return {"id": doc.get("id", ""), "status": "created"}

# _insert_lead stores a brand-new lead built from an input record, carrying
# every custom field from the input onto the stored doc.
def _insert_lead(inp, col):
    lead_id = _next_id("lead")
    doc = {
        "id": lead_id,
        "email": inp.get("email", ""),
        "firstName": inp.get("firstName", ""),
        "lastName": inp.get("lastName", ""),
        "createdAt": _now(),
        "updatedAt": _now(),
    }
    for k in inp:
        if k in _LEAD_BASE_FIELDS:
            continue
        doc[k] = inp[k]
    col.insert(doc)
    return doc

