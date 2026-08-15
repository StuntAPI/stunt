# Webhook handlers — register + list webhook subscriptions.
#
# Real RevenueCat webhooks are configured in the dashboard (no public REST
# registration endpoint, no v1 signature — see scripts/lib.star), so stunt
# exposes its own registration surface:
#
#   POST /v1/webhooks      {url, events} -> {webhook: {...}}  (201)
#   GET  /v1/webhooks      -> {webhooks: [...]}
#
# Registering immediately delivers a TEST event (the analog of RevenueCat's
# dashboard "send test webhook" button).

# Shared helpers (_require_auth, _emit_event) are preloaded from lib.star.

# on_create_webhook registers a webhook subscription.
def on_create_webhook(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    url = body.get("url", "")
    if url == None or url == "":
        return respond(400, {
            "code": 400,
            "message": "url is required",
        })

    events = body.get("events", [])
    if events == None:
        events = []

    wid = "wh_" + str(store_kv_incr("revenuecat", "webhook_seq"))
    webhook = {
        "id": wid,
        "url": url,
        "events": events,
        "created_at": clock.now_rfc3339(),
    }

    wc = store_collection("webhooks")
    wc.insert(webhook)

    # Register the delivery target with the events emitter.
    events_register(url)

    # Send a TEST notification so the receiver can check its wiring.
    _emit_event({
        "type": "TEST",
        "app_user_id": "$RCAnonymousID:stunt-test-user",
        "product_id": "test_product",
        "store": "stunt",
        "period_type": "NORMAL",
        "purchased_at_ms": clock.now_unix() * 1000,
    })

    return respond(201, {"webhook": _webhook_view(webhook)})

# on_list_webhooks returns registered webhook subscriptions.
def on_list_webhooks(req):
    err = _require_auth(req)
    if err != None:
        return err

    wc = store_collection("webhooks")
    out = []
    for w in wc.list():
        out.append(_webhook_view(w))
    return respond(200, {"webhooks": out})

# _webhook_view returns the public-facing webhook object.
def _webhook_view(w):
    return {
        "id": w["id"],
        "url": w.get("url", ""),
        "events": w.get("events", []),
        "created_at": w.get("created_at", clock.now_rfc3339()),
    }
