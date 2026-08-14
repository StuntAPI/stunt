# Webhook handlers — register, list, and delete shop webhooks.
#
# GET    /v1/shops/{shop_id}/webhooks.json         (Bearer) -> {data, total}
# POST   /v1/shops/{shop_id}/webhooks.json         (Bearer; JSON {url, topic, secret?})
#        -> 201 webhook object (secret masked)
# GET    /v1/shops/{shop_id}/webhooks/{webhook_id}.json
# DELETE /v1/shops/{shop_id}/webhooks/{webhook_id}.json
#        -> 200 {id, status: "deleted"}
#
# WEBHOOK SIGNATURE SCHEME:
# Every delivery carries X-Potify-Signature = hex(HMAC-SHA256(secret, raw_body))
# where `secret` is the PER-HOOK secret supplied at registration.
# See scripts/lib.star for the full documentation + Go verification snippet.

# Shared helpers (_require_auth, _to_int, _strip_json, _signed_emit,
# _emit_if_subscribed) are preloaded from scripts/lib.star.

# on_list_webhooks returns the shop's registered webhooks.
def on_list_webhooks(req):
    err = _require_auth(req)
    if err != None:
        return err

    shop_id = _to_int(req["params"].get("shop_id", ""))
    wc = store_collection("webhooks")
    result = []
    for w in wc.list():
        if w.get("shop_id") == shop_id:
            result.append(_webhook_view(w))
    return respond(200, {
        "data": result,
        "total": len(result),
    })

# on_create_webhook registers a webhook subscription for the shop. The
# per-hook `secret` is stored at registration and used to sign every later
# delivery of this topic (Printify's model — there is no global secret).
def on_create_webhook(req):
    err = _require_auth(req)
    if err != None:
        return err

    shop_id = _to_int(req["params"].get("shop_id", ""))
    body = req["body"]
    if body == None:
        body = {}

    url = body.get("address", body.get("url", ""))
    topic = body.get("topic", "order:created")
    if url == "":
        return respond(400, {"status": 400, "message": "url is required"})

    wid = str(store_kv_incr("printify", "webhook_seq"))
    webhook = {
        "id": wid,
        "shop_id": shop_id,
        "url": url,
        "topic": topic,
        "secret": body.get("secret", ""),
        "status": "active",
        "created_at": clock.now_unix(),
    }

    wc = store_collection("webhooks")
    wc.insert(webhook)

    # Register the webhook URL with the events emitter so that events_emit
    # will attempt delivery to this address.
    events_register(url)

    return respond(201, _webhook_view(webhook))

# on_get_webhook returns a single webhook subscription.
def on_get_webhook(req):
    err = _require_auth(req)
    if err != None:
        return err

    wid = _strip_json(req["params"].get("webhook_id", ""))
    wc = store_collection("webhooks")
    doc = wc.get(wid)
    if doc == None:
        return respond(404, {"status": 404, "message": "webhook not found"})
    return respond(200, doc)

# on_delete_webhook removes a webhook subscription; its topic is no longer
# delivered or signed.
def on_delete_webhook(req):
    err = _require_auth(req)
    if err != None:
        return err

    wid = _strip_json(req["params"].get("webhook_id", ""))
    wc = store_collection("webhooks")
    doc = wc.get(wid)
    if doc == None:
        return respond(404, {"status": 404, "message": "webhook not found"})

    wc.delete(wid)
    return respond(200, {
        "id": wid,
        "status": "deleted",
    })

# _webhook_view returns the public-facing webhook object. The signing secret
# is masked — clients that registered the hook already know their secret.
def _webhook_view(w):
    return {
        "id": w["id"],
        "shop_id": w.get("shop_id"),
        "url": w.get("url", ""),
        "topic": w.get("topic", ""),
        "secret": "********",
        "status": w.get("status", "active"),
        "created_at": w.get("created_at"),
    }
