# Webhook handlers — register + list + delete webhook subscriptions, and the
# webhook-signature verification endpoint.
#
#   POST   /v1/notifications/webhooks      {url, event_types:[{name}]} -> {webhook:{...}} (201)
#   GET    /v1/notifications/webhooks      -> {webhooks:[...]}
#   DELETE /v1/notifications/webhooks/{id} -> 204
#   POST   /v1/notifications/verify-webhook-signature -> {verification_status}
#
# SIGNATURE MODEL: real PayPal signs deliveries with a certificate chain
# (PayPal-Transmission-Sig etc.) that receivers verify by calling the verify
# API below with the transmitted headers + webhook id — there is no shared
# HMAC. The simulator emits unsigned deliveries with the real event envelope
# (see scripts/lib.star) and always answers SUCCESS from the verify endpoint
# so client verification flows can be exercised.

# Shared helpers (_require_auth, _pp_err_simple, _emit_event) are preloaded
# from scripts/lib.star.

# on_create_webhook registers a webhook subscription. Body:
#   {"url": "...", "event_types": [{"name": "PAYMENT.CAPTURE.COMPLETED"}]}
def on_create_webhook(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    url = body.get("url", "")
    if url == None or url == "":
        return _pp_err_simple(400, "INVALID_REQUEST", "url is required")

    event_types = body.get("event_types", [])
    if event_types == None:
        event_types = []

    wid = "WEBHOOK-" + str(store_kv_incr("paypal", "webhook_seq"))
    webhook = {
        "id": wid,
        "url": url,
        "event_types": event_types,
        "status": "ENABLED",
        "create_time": clock.now_rfc3339(),
    }

    wc = store_collection("webhooks")
    wc.insert(webhook)

    # Register the delivery target with the events emitter.
    events_register(url)

    return respond(201, {"webhook": webhook})

# on_list_webhooks returns registered webhook subscriptions.
def on_list_webhooks(req):
    err = _require_auth(req)
    if err != None:
        return err

    wc = store_collection("webhooks")
    out = []
    for w in wc.list():
        out.append(w)
    return respond(200, {"webhooks": out})

# on_delete_webhook deletes a webhook subscription (204, like real PayPal).
def on_delete_webhook(req):
    err = _require_auth(req)
    if err != None:
        return err

    wid = req["params"].get("id", "")
    wc = store_collection("webhooks")
    if wc.get(wid) == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Webhook not found.")

    wc.delete(wid)
    return respond(204, {})

# on_verify_webhook_signature mirrors the real verification endpoint: PayPal
# receivers POST the transmission headers + webhook id and PayPal answers
# SUCCESS/FAILURE. The simulator does not cryptographically enforce (its
# deliveries are unsigned), so any well-formed request returns SUCCESS.
def on_verify_webhook_signature(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    webhook_id = body.get("webhook_id", "")
    if webhook_id == None or webhook_id == "":
        return _pp_err_simple(400, "INVALID_REQUEST", "webhook_id is required")

    wc = store_collection("webhooks")
    if wc.get(webhook_id) == None:
        return respond(200, {"verification_status": "FAILURE"})

    return respond(200, {"verification_status": "SUCCESS"})
