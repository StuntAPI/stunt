# Webhook subscription handlers — company-scoped /c/{company_id}/subscriptions
# in the real v2 shape ({data:{sink,types,verification_method,config}}, ids
# SUBxxx, verified/warnings in the response). Deliveries are signed with
# X-Signature = base64(HMAC-SHA256(secret, body)) like the real provider.


def _sub_defaults():
    return {
        "types": [],
        "config": {},
    }

def on_webhooks_list(req):
    return _crud_list(req, "webhooks")

def on_webhooks_create(req):
    err = _require_auth(req)
    if err:
        return err
    company, err = _require_company(req)
    if err:
        return err
    body = _body_of(req)
    if body == None:
        return _bad_body()
    data = body.get("data", {})
    if data == None:
        data = {}
    sink = data.get("sink", "")
    if sink == None or sink == "":
        return _api_error(400, "VALIDATION_ERROR", "The sink field is required.")
    seq = store_kv_incr("fattureincloud", "webhooks_seq")
    doc = {
        "id": "SUB" + str(100 + seq),
        "sink": sink,
        "types": data.get("types", []),
        "verification_method": data.get("verification_method", "NONE"),
        "config": data.get("config", {}),
        "company_id": str(company.get("id")),
    }
    store_collection("webhooks").insert(doc)
    events_register(sink)
    out = _sub_view(doc)
    out["verified"] = True
    out["warnings"] = []
    return respond(201, {"data": out})

def on_webhook_get(req):
    return _crud_get(req, "webhooks", "Subscription")

def on_webhook_modify(req):
    return _crud_modify(req, "webhooks", "Subscription")

def on_webhook_delete(req):
    return _crud_delete(req, "webhooks", "Subscription")

# _sub_view renders the real subscription shape (sink, not url).
def _sub_view(doc):
    out = {}
    for k in doc:
        if k == "company_id" or k[:1] == "_":
            continue
        out[k] = doc[k]
    return out

