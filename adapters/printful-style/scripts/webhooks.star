# Webhook handlers — the store's single webhook configuration.
#
# Printful allows ONE webhook per store, set through the v1 /webhooks routes:
#
# GET    /webhooks    (Bearer) -> {result: {url, types, secret}}
# POST   /webhooks    (Bearer; JSON {url, types?, secret?}) -> {result: {...}}
# PUT    /webhooks    (Bearer; same shape as POST)          -> {result: {...}}
# DELETE /webhooks    (Bearer) -> {result: "success"}
#
# WEBHOOK SIGNATURE SCHEME:
# Every delivery carries X-Pful-Signature = hex(HMAC-SHA256(secret, raw_body))
# where `secret` is the secret configured on this webhook.
# See scripts/lib.star for the full documentation + Go verification snippet.

# Shared helpers (_require_auth, _webhook_config, _signed_emit,
# _emit_if_subscribed) are preloaded from scripts/lib.star.

# on_get_webhooks returns the current webhook configuration (or an empty
# result when none is set).
def on_get_webhooks(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc = _webhook_config()
    if doc == None:
        return respond(200, {"result": {}})
    return respond(200, {"result": _webhook_view(doc)})

# on_set_webhooks creates or replaces the webhook configuration (POST and PUT
# share this handler — the real API accepts both). The `secret` is stored on
# the configuration and used to sign every later delivery.
def on_set_webhooks(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    url = body.get("url") or ""
    if url == "":
        return respond(400, {
            "error": {"message": "url is required", "code": 400},
        })

    config = {
        "id": "config",
        "url": url,
        "types": body.get("types", []),
        "secret": body.get("secret", ""),
    }
    if config["types"] == None:
        config["types"] = []

    wc = store_collection("webhooks")
    if _webhook_config() != None:
        wc.update("config", config)
    else:
        wc.insert(config)

    # Register the webhook URL with the events emitter so that events_emit
    # will attempt delivery to this address.
    events_register(url)

    return respond(200, {"result": _webhook_view(config)})

# on_delete_webhooks removes the webhook configuration; nothing is delivered
# or signed afterwards.
def on_delete_webhooks(req):
    err = _require_auth(req)
    if err != None:
        return err

    wc = store_collection("webhooks")
    if _webhook_config() != None:
        wc.delete("config")
    return respond(200, {"result": "success"})

# _webhook_view returns the public-facing webhook configuration. The signing
# secret is masked — the client that configured the webhook already knows it.
def _webhook_view(doc):
    return {
        "url": doc.get("url", ""),
        "types": doc.get("types", []),
        "secret": "********",
    }
