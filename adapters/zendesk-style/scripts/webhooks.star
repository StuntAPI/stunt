# Webhook handlers — Zendesk webhooks with HMAC-SHA256 signature.
#
# GET    /api/v2/webhooks     -> list webhooks
# POST   /api/v2/webhooks     -> create webhook (registers the delivery URL)
# DELETE /api/v2/webhooks/{id}-> delete webhook (204)
#
# Zendesk webhooks are signed with:
#   X-Zendesk-Webhook-Signature           = base64(HMAC-SHA256(secret, TIMESTAMP + BODY))
#   X-Zendesk-Webhook-Signature-Timestamp = unix timestamp
#
# The secret is per-webhook: real Zendesk auto-generates one (revealed in Admin
# Center); the simulator accepts "signing_secret" in the create payload and
# stores it on the webhook doc — later deliveries are MACed with THAT secret.
# See scripts/lib.star for the full documentation + Go verification snippet.

# Shared helpers from lib.star.

def on_list_webhooks(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("webhooks")
    docs = col.list()

    webhooks = []
    for d in docs:
        webhooks.append({
            "id": d.get("id", ""),
            "name": d.get("name", ""),
            "endpoint": d.get("endpoint", ""),
            "status": d.get("status", "active"),
            "method": d.get("method", "POST"),
        })

    page_size = _to_int(_get_query(req, "per_page", "100"))
    paged, next_cursor = _list_page(req, webhooks)

    resp = {
        "webhooks": paged,
        "meta": {"has_more": next_cursor != None},
    }
    if next_cursor != None:
        resp["links"] = {"next": _next_link("/api/v2/webhooks", next_cursor, page_size)}
    else:
        resp["links"] = {"next": None}
    return respond(200, resp)

def on_create_webhook(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)
    webhook = body.get("webhook", body)

    name = webhook.get("name", "")
    endpoint = webhook.get("endpoint", "")
    if endpoint == None:
        endpoint = ""

    subscriptions = webhook.get("subscriptions", [])
    if subscriptions == None:
        subscriptions = []

    webhook_id = _next_id("webhook")

    doc = {
        "id": webhook_id,
        "name": name,
        "endpoint": endpoint,
        "status": "active",
        "method": webhook.get("method", "POST"),
        "signing_secret": webhook.get("signing_secret", ""),
        "subscriptions": subscriptions,
    }

    col = store_collection("webhooks")
    col.insert(doc)

    # Register the delivery URL with the events emitter.
    if endpoint != "":
        events_register(endpoint)

    # Real Zendesk never returns the signing secret on create; mask it here
    # (retrieve it from the stored doc / your create payload).
    view = {}
    for k in doc:
        view[k] = doc[k]
    view["signing_secret"] = "********"
    return respond(201, {"webhook": view})

def on_delete_webhook(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    webhook_id = req["params"].get("id", "")
    col = store_collection("webhooks")
    doc = col.get(webhook_id)
    if doc == None:
        return _zd_error(404, "RecordNotFound", "Webhook not found")

    col.delete(webhook_id)
    return respond(204)
