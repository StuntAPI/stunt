# Company handlers — the v2 API lists companies at /user/companies and scopes
# everything else under /c/{company_id}. There is no create-company endpoint.

def on_entities_list(req):
    err = _require_auth(req)
    if err:
        return err
    _ensure_seed_company()
    rows = [_strip_internal(c) for c in _companies().list()]
    return respond(200, {
        "data": {
            "companies": rows,
            "info": {"need_marketing_consents": False, "need_password_change": False, "need_plan_invoice": False},
        },
    })

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
    if body == None:
        return _bad_body()
    patch = {}
    for k in company:
        patch[k] = company[k]
    for k in body:
        patch[k] = body[k]
    _companies().update(company.get("id"), patch)
    return respond(200, {"data": _strip_internal(patch)})
