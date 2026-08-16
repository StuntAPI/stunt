# Tax rate handler — manual tax rates applied to invoices and subscriptions
# (docs.stripe.com/api/tax_rates).
#
# {id txr_*, object "tax_rate", active, display_name, inclusive,
#  jurisdiction, percentage float, description, metadata} (TAX RATE DOC
# CONTRACT). Tax cents per rate over a line amount are computed by the
# billing files as int(amount * percentage / 100.0 + 0.5); exclusive rates
# add to the invoice total, inclusive rates do not.
#
# Update accepts ONLY active (+ metadata); delete is a soft delete that
# keeps the object retrievable with deleted: true, exactly like real Stripe.
# Shared helpers (_require_auth, _next_id, _now, _not_found, _list_page,
# _newest_first, _created_filters, _created_check, _signed_emit,
# _idempotent_lookup, _idempotent_remember) are in lib.star.

_TXR_COLLECTION = "tax_rates"

# _txr_err builds the real Stripe 400 envelope.
def _txr_err(msg, param):
    e = {"type": "invalid_request_error", "message": msg}
    if param != None:
        e["param"] = param
    return respond(400, {"error": e})

def _txr_missing(param):
    return _txr_err("Missing required param: " + param + ".", param)

# _txr_percentage parses a percentage into a float: numbers pass through,
# numeric strings ("16", "8.875") are split manually (Starlark float()
# raises on bad input and there is no try/except). Returns None when the
# value is not a non-negative number.
def _txr_percentage(v):
    if v == None:
        return None
    if type(v) == "int":
        return float(v)
    if type(v) == "float":
        return v
    if type(v) != "string":
        return None
    s = v.strip()
    if s == "":
        return None
    whole = ""
    frac = ""
    seen_dot = False
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            if seen_dot:
                frac = frac + ch
            else:
                whole = whole + ch
        elif ch == "." and not seen_dot:
            seen_dot = True
        else:
            return None
    if whole == "" and frac == "":
        return None
    out = float(_to_int(whole))
    scale = 1.0
    for i in range(len(frac)):
        scale = scale * 10.0
    out = out + float(_to_int(frac)) / scale
    return out

# _txr_public renders a stored tax rate (internal keys stripped).
def _txr_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    return out

# POST /v1/tax_rates — create a tax rate (display_name, inclusive and
# percentage required).
def on_create_tax_rate(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, _TXR_COLLECTION)
    if cached != None:
        return respond(cached["status"], _txr_public(cached["doc"]))

    if _bad_body(req):
        return _txr_err("Invalid request body: could not parse as JSON.", None)
    body = req["body"]
    if body == None:
        body = {}

    display_name = body.get("display_name", None)
    if display_name == None or display_name == "":
        return _txr_missing("display_name")

    inclusive = body.get("inclusive", None)
    if inclusive == None or type(inclusive) != "bool":
        return _txr_missing("inclusive")

    pct = _txr_percentage(body.get("percentage", None))
    if pct == None or pct < 0:
        return respond(400, {"error": {"code": "parameter_invalid_integer", "type": "invalid_request_error", "message": "Invalid integer: " + str(body.get("percentage", None)), "param": "percentage"}})

    active = body.get("active", True)
    if active == None:
        active = True

    metadata = body.get("metadata", {})
    if metadata == None or type(metadata) != "dict":
        metadata = {}

    doc = {
        "id": _next_id("txr"),
        "object": "tax_rate",
        "active": active == True,
        "display_name": display_name,
        "inclusive": inclusive,
        "jurisdiction": body.get("jurisdiction", None),
        "percentage": pct,
        "description": body.get("description", None),
        "metadata": metadata,
        "livemode": False,
        "created": _now(),
        "deleted": False,
    }
    store_collection(_TXR_COLLECTION).insert(doc)
    _signed_emit("tax_rate.created", _txr_public(doc))
    _idempotent_remember(req, _TXR_COLLECTION, 201, doc["id"])
    return respond(201, _txr_public(doc))

# GET /v1/tax_rates/{id} — retrieve a tax rate (deleted rates stay readable
# with deleted: true).
def on_retrieve_tax_rate(req):
    err = _require_auth(req)
    if err != None:
        return err
    doc = store_collection(_TXR_COLLECTION).get(req["params"]["id"])
    if doc == None:
        return _not_found("tax_rate", req["params"]["id"])
    return respond(200, _txr_public(doc))

# GET /v1/tax_rates — list tax rates (active + created filters, like the
# real API).
def on_list_tax_rates(req):
    err = _require_auth(req)
    if err != None:
        return err
    bad = _created_check(req)
    if bad != None:
        return bad
    f = []
    active = _get_query(req, "active")
    if active == "true":
        f.append(["active", "=", True])
    elif active == "false":
        f.append(["active", "=", False])
    _created_filters(req, f)
    docs = store_collection(_TXR_COLLECTION).list()
    if len(f) > 0:
        docs = query_select(docs, f)
    docs = _newest_first(docs)
    page, has_more, e = _list_page(req, docs, "tax_rate")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_txr_public(d) for d in page], "has_more": has_more, "url": "/v1/tax_rates"})

# POST /v1/tax_rates/{id} — update a tax rate. The real API accepts ONLY
# active (plus metadata); archived rates keep applying to existing invoices
# and subscriptions but not new ones.
def on_update_tax_rate(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = store_collection(_TXR_COLLECTION).get(id)
    if doc == None:
        return _not_found("tax_rate", id)
    if doc.get("deleted", False) == True:
        return _txr_err("This tax rate has been deleted and can no longer be updated.", None)

    if _bad_body(req):
        return _txr_err("Invalid request body: could not parse as JSON.", None)
    body = req["body"]
    if body == None:
        body = {}

    changed = False
    if body.get("active", None) != None:
        doc["active"] = body["active"] == True
        changed = True
    if body.get("metadata", None) != None and type(body["metadata"]) == "dict":
        meta = doc.get("metadata", {})
        if meta == None or type(meta) != "dict":
            meta = {}
        for k in body["metadata"]:
            meta[k] = body["metadata"][k]
        doc["metadata"] = meta
        changed = True
    if not changed:
        return _txr_err("This tax rate cannot be updated: only the active flag and metadata may be set.", None)

    store_collection(_TXR_COLLECTION).update(id, doc)
    _signed_emit("tax_rate.updated", _txr_public(doc))
    return respond(200, _txr_public(doc))

# DELETE /v1/tax_rates/{id} — soft delete: the rate stays retrievable
# (deleted: true) and keeps applying where already set.
def on_delete_tax_rate(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = store_collection(_TXR_COLLECTION).get(id)
    if doc == None:
        return _not_found("tax_rate", id)

    if doc.get("deleted", False) != True:
        doc["deleted"] = True
        doc["active"] = False
        store_collection(_TXR_COLLECTION).update(id, doc)
    return respond(200, {"id": id, "object": "tax_rate", "deleted": True})
