# Webhook registration handlers. All data synthetic.

def on_webhook_create(req):
    body = body_of(req)
    hook = {"id": next_id("webhooks"), "url": body.get("url")}
    store_collection("webhooks").insert(hook)
    events_register(body.get("url", ""))
    return respond(201, hook)

def on_webhook_list(req):
    return respond(200, {"webhooks": store_collection("webhooks").list()})
