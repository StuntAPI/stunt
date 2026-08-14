# Webhook handlers — register, list, delete Adyen-style standard webhooks.
#
# POST   /v68/webhooks               -> {webhook:{...}}  (201)
# GET    /v68/webhooks               -> {webhooks:[...]}
# DELETE /v68/webhooks/{webhookId}   -> 200 {}
#
# Real Adyen configures standard webhooks through the Customer Area or the
# Management API (merchants/{merchantId}/webhooks). This simulator exposes a
# registration surface at /v68/webhooks modelled on that webhook resource:
#
#   {
#     "type": "standard",
#     "url": "https://example.test/adyen",
#     "communicationFormat": "json",
#     "active": true,
#     "hmacKey": "adyen_stunt_mock_hmac_B7dQ",     # local extension
#     "events": ["AUTHORISATION", "CAPTURE", ...]  # local extension
#   }
#
# Real Adyen generates the HMAC key when the webhook is created and never
# returns it, and event filtering lives in additionalSettings; both
# extensions exist so client code can test per-hook signature verification
# and event filtering locally. Registered webhooks receive signed standard
# notifications (see scripts/lib.star for the hmacSignature scheme) whenever
# a payment is authorised, refused, captured, refunded, reversed or cancelled.

# Shared helpers (_require_apikey, _signed_emit) are preloaded from
# scripts/lib.star.

# on_create_webhook registers a standard webhook subscription.
def on_create_webhook(req):
    err = _require_apikey(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    wid = "wh_" + str(store_kv_incr("adyen", "webhook_seq"))
    webhook = {
        "id": wid,
        "type": body.get("type", "standard"),
        "url": body.get("url", ""),
        "communicationFormat": body.get("communicationFormat", "json"),
        "active": body.get("active", True),
        "hmacKey": body.get("hmacKey", ""),
        "events": body.get("events", []),
        "created_at": clock.now_rfc3339(),
    }

    wc = store_collection("webhooks")
    wc.insert(webhook)

    url = webhook["url"]
    if url != "":
        events_register(url)

    return respond(201, {"webhook": _webhook_view(webhook)})

# on_list_webhooks returns registered webhook subscriptions.
def on_list_webhooks(req):
    err = _require_apikey(req)
    if err != None:
        return err

    wc = store_collection("webhooks")
    result = []
    for h in wc.list():
        result.append(_webhook_view(h))
    return respond(200, {"webhooks": result})

# on_delete_webhook deletes a webhook subscription.
def on_delete_webhook(req):
    err = _require_apikey(req)
    if err != None:
        return err

    wid = req["params"]["webhookId"]
    wc = store_collection("webhooks")
    if wc.get(wid) == None:
        return _adyen_err(404, "010", "Webhook not found", "validation")
    wc.delete(wid)
    return respond(200, {})

# _webhook_view returns the public-facing webhook object. The HMAC key is
# never echoed (real Adyen generates it server-side and does not return it);
# hmacKeySet just reports whether one is configured.
def _webhook_view(w):
    return {
        "id": w["id"],
        "type": w.get("type", "standard"),
        "url": w.get("url", ""),
        "communicationFormat": w.get("communicationFormat", "json"),
        "active": w.get("active", True),
        "hmacKeySet": w.get("hmacKey", "") != "",
        "events": w.get("events", []),
        "created_at": w.get("created_at", ""),
    }
