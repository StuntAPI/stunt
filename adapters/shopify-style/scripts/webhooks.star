# Webhook handlers — register + list webhook subscriptions.
#
# GET  /admin/api/2024-10/webhooks.json -> {webhooks:[...]}
# POST /admin/api/2024-10/webhooks.json -> {webhook:{...}}   (201)
#
# Requires X-Shopify-Access-Token.
#
# WEBHOOK SIGNATURE SCHEME:
# When Shopify delivers a webhook to your `address`, it includes:
#   X-Shopify-Hmac-SHA256: base64(HMAC-SHA256(api_secret_key, raw_body))
# Your handler must verify this signature and respond with 200 OK + empty body.
# See scripts/lib.star for the full documentation + Go verification snippet.

# Shared helpers (_require_token, _shopify_err, _next_id, _seed, _now) are
# preloaded from scripts/lib.star.

# on_list_webhooks returns registered webhook subscriptions as {webhooks:[...]}.
# Shopify REST pages via the `limit` (page size) and `page_info` (opaque
# cursor) query params; the next cursor round-trips through a
# 'Link: <url>; rel="next"' header. When `limit` is missing paging is disabled
# and the whole list is returned.
def on_list_webhooks(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    wc = store_collection("webhooks")
    hooks = wc.list()
    result = []
    for h in hooks:
        result.append(_webhook_view(h))

    page, next_cursor = _list_page(req, result)
    headers = None
    link = _next_link(req, next_cursor, _to_int(_get_query(req, "limit")))
    if link != None:
        headers = {"Link": link}
    return respond(200, {"webhooks": page}, headers)

# on_create_webhook registers a webhook subscription.
def on_create_webhook(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    body = req["body"]
    if body == None:
        body = {}
    input_wh = body.get("webhook", {})
    if input_wh == None:
        input_wh = {}

    wid = _next_id("webhooks")
    webhook = {
        "id": wid,
        "topic": input_wh.get("topic", ""),
        "address": input_wh.get("address", ""),
        "format": input_wh.get("format", "json"),
        "created_at": _now(),
        "updated_at": _now(),
    }

    wc = store_collection("webhooks")
    wc.insert(webhook)

    # Register the webhook URL with the events emitter so that events_emit
    # will attempt delivery to this address.
    addr = webhook["address"]
    if addr != "":
        events_register(addr)

    return respond(201, {"webhook": _webhook_view(webhook)})

# _webhook_view returns the public-facing webhook subscription object.
def _webhook_view(w):
    return {
        "id": w["id"],
        "topic": w.get("topic", ""),
        "address": w.get("address", ""),
        "format": w.get("format", "json"),
        "created_at": w.get("created_at", _now()),
        "updated_at": w.get("updated_at", _now()),
    }
