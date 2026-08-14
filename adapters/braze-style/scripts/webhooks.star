# Webhook handlers — register/list outbound webhook targets (local simulator
# extension; real Braze has NO webhook-registration REST API — webhooks are
# authored as campaign/canvas messages in the dashboard, with customer-
# configured headers and body).
#
# POST /webhooks (Bearer; JSON {url, events}) -> {message:"success", webhook:{...}}
# GET  /webhooks (Bearer) -> {message:"success", webhooks:[...]}
#
# Deliveries are UNSIGNED by design (Braze applies no provider signature to
# webhook deliveries). See scripts/lib.star for the documentation.
#
# Shared helpers (_require_auth, _emit_if_subscribed) are preloaded from
# scripts/lib.star.

# on_create_webhook registers an outbound webhook target.
def on_create_webhook(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    url = body.get("url", "")
    if url == None:
        url = ""
    events = body.get("events", [])
    if events == None:
        events = []

    webhook = {
        "id": "hook-" + str(store_kv_incr("braze", "webhook_seq") + 1),
        "url": url,
        "events": events,
        "created_at": clock.now_rfc3339(),
    }

    wc = store_collection("webhooks")
    wc.insert(webhook)

    # Register the URL with the events emitter so events_emit delivers here.
    if url != "":
        events_register(url)

    return respond(200, {
        "message": "success",
        "webhook": _webhook_view(webhook),
    })

# on_list_webhooks returns registered webhook targets.
def on_list_webhooks(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    wc = store_collection("webhooks")
    result = []
    for h in wc.list():
        result.append(_webhook_view(h))
    return respond(200, {
        "message": "success",
        "webhooks": result,
    })

# _webhook_view returns the public-facing webhook target object.
def _webhook_view(w):
    return {
        "id": w["id"],
        "url": w.get("url", ""),
        "events": w.get("events", []),
        "created_at": w.get("created_at", ""),
    }
