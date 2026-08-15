# Company (entity) handlers — everything in the v2 API is scoped under
# /entities/{company_id}.

def on_entities_list(req):
    err = _require_auth(req)
    if err:
        return err
    rows = [_strip_internal(c) for c in _companies().list()]
    return respond(200, {"current_page": 1, "data": rows, "from": 1 if rows else None,
                         "last_page": 1, "per_page": 50,
                         "to": len(rows) if rows else None, "total": len(rows)})

def on_entities_create(req):
    err = _require_auth(req)
    if err:
        return err
    body = _body_of(req)
    doc = {
        "id": _next_id("companies"),
        "name": body.get("name", ""),
        "type": body.get("type", "company"),
        "country": body.get("country", "IT"),
        "vat_number": body.get("vat_number", ""),
        "tax_code": body.get("tax_code", ""),
    }
    _companies().insert(doc)
    return respond(201, {"data": _strip_internal(doc)})

def on_entity_info_get(req):
    err = _require_auth(req)
    if err:
        return err
    company, err = _require_company(req)
    if err:
        return err
    info = _strip_internal(company)
    info["currency"] = info.get("currency", {"symbol": "EUR", "precision": 2})
    return respond(200, {"data": info})

def on_entity_info_put(req):
    err = _require_auth(req)
    if err:
        return err
    company, err = _require_company(req)
    if err:
        return err
    body = _body_of(req)
    patch = {}
    for k in company:
        patch[k] = company[k]
    for k in body:
        patch[k] = body[k]
    _companies().update(company.get("id"), patch)
    return respond(200, {"data": _strip_internal(patch)})
