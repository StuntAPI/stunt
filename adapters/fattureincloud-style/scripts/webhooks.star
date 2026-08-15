# Webhook handlers — account-level configuration of delivery URLs.

def on_webhooks_list(req):
    err = _require_auth(req)
    if err:
        return err
    rows = [_strip_internal(w) for w in _rows("webhooks", "account")]
    return respond(200, _paginate(rows, req, "/webhooks"))

def on_webhooks_create(req):
    err = _require_auth(req)
    if err:
        return err
    body = _body_of(req)
    doc = {"id": _next_id("webhooks"),
           "url": body.get("url", ""),
           "types": body.get("types", []),
           "enabled": body.get("enabled", True),
           "company_id": "account"}
    store_collection("webhooks").insert(doc)
    return respond(201, {"data": _strip_internal(doc)})

def on_webhook_get(req):
    err = _require_auth(req)
    if err:
        return err
    doc_id = str(req.get("params", {}).get("id", ""))
    for d in _rows("webhooks", "account"):
        if str(d.get("id")) == doc_id:
            return respond(200, {"data": _strip_internal(d)})
    return _api_error(404, "not_found", "Webhook not found.")

def on_webhook_modify(req):
    err = _require_auth(req)
    if err:
        return err
    doc_id = str(req.get("params", {}).get("id", ""))
    body = _body_of(req)
    coll = store_collection("webhooks")
    for d in _rows("webhooks", "account"):
        if str(d.get("id")) == doc_id:
            patch = {}
            for k in d:
                patch[k] = d[k]
            for k in body:
                patch[k] = body[k]
            coll.update(d.get("id"), patch)
            return respond(200, {"data": _strip_internal(coll.get(d.get("id")))})
    return _api_error(404, "not_found", "Webhook not found.")

def on_webhook_delete(req):
    err = _require_auth(req)
    if err:
        return err
    doc_id = str(req.get("params", {}).get("id", ""))
    for d in _rows("webhooks", "account"):
        if str(d.get("id")) == doc_id:
            store_collection("webhooks").delete(d.get("id"))
            return respond(200, {})
    return _api_error(404, "not_found", "Webhook not found.")
