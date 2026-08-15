# Taxes (F24 documents), cashbook, archive.

# _tax_defaults seeds a synthetic F24 (the v2 "taxes" resource is the F24
# tax-payment document collection, not VAT bands).
def _tax_defaults():
    return {
        "amount": "0.00",
        "due_date": clock.now_rfc3339()[:10],
        "status": "not_paid",
        "attachment_url": "",
        "attachment_token": "",
    }

def on_taxes_list(req):
    return _crud_list(req, "taxes")

def on_taxes_create(req):
    return _crud_create(req, "taxes", _tax_defaults())

def on_tax_get(req):
    return _crud_get(req, "taxes", "F24")

def on_tax_modify(req):
    return _crud_modify(req, "taxes", "F24")

def on_tax_delete(req):
    return _crud_delete(req, "taxes", "F24")

def on_cashbook_month(req):
    err = _require_auth(req)
    if err:
        return err
    company, err = _require_company(req)
    if err:
        return err
    params = req.get("params", {})
    year = str(params.get("year", ""))
    month = str(params.get("month", ""))
    rows = []
    for d in _rows("cashbook", company.get("id")):
        if str(d.get("year", "")) == year and str(d.get("month", "")) == month:
            rows.append(_strip_internal(d))
    return respond(200, {"data": {"year": _to_int(year, 0),
                                  "month": _to_int(month, 0),
                                  "cashbook": rows}})

def on_archive_upload(req):
    # Real shape: a multipart/form-data attachment upload. The file part is
    # stored in the blob store; the response points back at it.
    err = _require_auth(req)
    if err:
        return err
    company, err = _require_company(req)
    if err:
        return err
    ct = req["headers"].get("content-type", "")
    filename = "attachment"
    data = None
    if ct[:10] == "multipart/":
        parts, perr = parse_multipart(ct, req["raw_body"])
        if perr != None:
            return _api_error(400, "VALIDATION_ERROR", "Malformed multipart body.")
        for p in parts:
            if p["filename"] != None:
                data = p["data"]
                fn = p["filename"]
                dot = fn.rfind(".")
                filename = "att_" + _next_id("archive") + (fn[dot:] if dot >= 0 else "")
    if data == None:
        # JSON fallback (metadata-only): accepted for simple tests.
        body = _body_of(req)
        if body == None:
            return _bad_body()
        filename = "att_" + _next_id("archive")
        data = ""
    doc = {"id": _next_id("archive"), "name": filename,
           "attachment_url": "https://sim.invalid/attachment/" + filename,
           "company_id": str(company.get("id"))}
    if data != "":
        store_blob("fic-archive").put(filename, data, "application/octet-stream")
    store_collection("archive").insert(doc)
    return respond(201, {"data": _strip_internal(doc)})
