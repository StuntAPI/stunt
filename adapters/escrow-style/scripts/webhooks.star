# Webhook registration handlers. All data synthetic.

def on_webhook_create(req):
    err = _require_basic(req)
    if err != None:
        return err
    body, ok = body_of(req)
    if not ok:
        return bad_body()
    url = body.get("url", "")
    if url == None or url == "":
        return respond(400, {"errors": {"url": ["Url can't be blank"]}})
    hook = {
        # String id for the store; rendered as int in views (real ids are ints).
        "id": str(next_id("webhooks")),
        "url": url,
    }
    store_collection("webhooks").insert(hook)
    events_register(url)
    return respond(201, _hook_view(hook))

def on_webhook_list(req):
    err = _require_basic(req)
    if err != None:
        return err
    hooks = [_hook_view(h) for h in store_collection("webhooks").list()]
    return respond(200, {"webhooks": hooks})

def _hook_view(h):
    out = {}
    for k in h:
        if k[:1] == "_":
            continue
        out[k] = h[k]
    out["id"] = int(h.get("id", "0"))
    return out
