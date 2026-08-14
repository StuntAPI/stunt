# Webhook handlers — create/list/delete webhook endpoints (Resend).
#
# POST   /webhooks      (Bearer; JSON {endpoint, events, secret?}) -> {id, url, events}
# GET    /webhooks      (Bearer) -> {data: [...]}
# DELETE /webhooks/{id} (Bearer) -> 200
#
# Real Resend manages webhook endpoints via POST/GET/DELETE /webhooks with a
# request body of {endpoint: "https://...", events: ["email.sent", ...]}.
# The signing secret normally comes from the dashboard; this simulator also
# accepts an optional "secret" field so a per-hook secret can be configured
# at registration (deliveries then sign with that secret — the default is
# the fixed synthetic key in scripts/lib.star).
#
# Shared helpers (_require_auth, _signed_emit) are preloaded from
# scripts/lib.star.

# on_create_webhook registers a webhook endpoint.
def on_create_webhook(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    endpoint = body.get("endpoint", "")
    if endpoint == None:
        endpoint = ""
    events = body.get("events", [])
    if events == None:
        events = []
    secret = body.get("secret", "")
    if secret == None:
        secret = ""

    wid = "wh_" + str(store_kv_incr("resend", "webhook_seq") + 1)
    webhook = {
        "id": wid,
        "url": endpoint,
        "events": events,
        "secret": secret,
        "created_at": clock.now_rfc3339(),
    }

    wc = store_collection("webhooks")
    wc.insert(webhook)

    # Register the URL with the events emitter so events_emit delivers here.
    if endpoint != "":
        events_register(endpoint)

    return respond(200, _webhook_view(webhook))

# on_list_webhooks returns registered webhook endpoints as {data: [...]}.
def on_list_webhooks(req):
    err = _require_auth(req)
    if err != None:
        return err

    wc = store_collection("webhooks")
    result = []
    for h in wc.list():
        result.append(_webhook_view(h))
    return respond(200, {"data": result})

# on_delete_webhook deletes a webhook endpoint. Real Resend returns 200 with
# an empty body.
def on_delete_webhook(req):
    err = _require_auth(req)
    if err != None:
        return err

    wid = req["params"]["id"]
    wc = store_collection("webhooks")
    if wc.get(wid) == None:
        return respond(404, {
            "statusCode": 404,
            "message": "Webhook not found",
            "name": "not_found",
        })

    wc.delete(wid)
    return respond(200, "")

# _webhook_view returns the public-facing webhook endpoint object (the
# stored per-hook secret is never echoed — like Resend, the secret is only
# shown once at configuration time).
def _webhook_view(w):
    return {
        "id": w["id"],
        "url": w.get("url", ""),
        "events": w.get("events", []),
        "created_at": w.get("created_at", ""),
    }
