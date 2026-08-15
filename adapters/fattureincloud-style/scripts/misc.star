# Taxes, cashbook, archive.

def on_taxes_list(req):
    err = _require_auth(req)
    if err:
        return err
    company, err = _require_company(req)
    if err:
        return err
    # The standard Italian VAT bands, synthetic but shape-accurate.
    return respond(200, {"data": [
        {"id": 1, "name": "Aliquota 22%", "percent": 22.0, "is_negative": False},
        {"id": 2, "name": "Aliquota 10%", "percent": 10.0, "is_negative": False},
        {"id": 3, "name": "Aliquota 4%", "percent": 4.0, "is_negative": False},
        {"id": 4, "name": "Non imponibile", "percent": 0.0, "is_negative": False},
        {"id": 5, "name": "Reverse charge", "percent": 0.0, "is_negative": False},
    ]})

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
    # The real endpoint is a multipart file upload; the simulator accepts JSON
    # metadata only — tests cannot assert on the bytes anyway.
    err = _require_auth(req)
    if err:
        return err
    company, err = _require_company(req)
    if err:
        return err
    body = _body_of(req)
    doc = {"id": _next_id("archive"), "name": body.get("name", ""),
           "attachment_url": "https://sim.invalid/attachment/" + _next_id("archive"),
           "company_id": str(company.get("id"))}
    store_collection("archive").insert(doc)
    return respond(201, {"data": _strip_internal(doc)})
