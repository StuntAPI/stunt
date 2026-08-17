# Event Webhook settings handlers — real SendGrid paths.
#
# POST /v3/user/webhooks/event/settings (Bearer; JSON {enabled, url, ...})
#   -> 200 {enabled, url, ...}   (enables/disables the Event Webhook)
# GET  /v3/user/webhooks/event/settings (Bearer)
#   -> 200 {enabled, url, ...}
# POST /v3/user/webhooks/event/test (Bearer)
#   -> 202 Accepted (sends a sample signed event to the configured URL)
#
# Deliveries are ECDSA-signed (X-Twilio-Email-Event-Webhook-Signature /
# -Timestamp). See scripts/lib.star for the scheme and the deviation note
# (raw r||s signature instead of ASN.1 DER).
#
# Shared helpers (_require_auth, _emit_event, _event_hook_enabled) are
# preloaded from scripts/lib.star.

# on_update_settings enables/disables the Event Webhook and sets its URL.
def on_update_settings(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    enabled = body.get("enabled", False)
    if enabled == None:
        enabled = False
    url = body.get("url") or ""
    if url == None:
        url = ""

    flag = "no"
    if enabled:
        flag = "yes"
    store_kv_set("sendgrid", "event_hook_enabled", flag)
    store_kv_set("sendgrid", "event_hook_url", url)
    if enabled and url != "":
        # Register the URL with the events emitter so events_emit delivers here.
        events_register(url)

    return respond(200, _settings_view())

# on_get_settings returns the current Event Webhook settings.
def on_get_settings(req):
    err = _require_auth(req)
    if err != None:
        return err

    return respond(200, _settings_view())

# on_send_test_event delivers a sample signed event to the configured URL,
# like SendGrid's "Send test webhook" action. Returns 202 with an empty body.
def on_send_test_event(req):
    err = _require_auth(req)
    if err != None:
        return err

    url = store_kv_get("sendgrid", "event_hook_url")
    if url == None:
        url = ""
    if not _event_hook_enabled() or url == "":
        return respond(400, {
            "errors": [
                {
                    "message": "The event webhook is not enabled or has no URL.",
                    "field": "url",
                    "help": None,
                }
            ],
        })

    _emit_event("processed", "msg_test@stunt.local", "test@example.com")
    return respond(202, "", {"X-Message-Id": "msg_test@stunt.local"})

# _settings_view renders the current settings the way SendGrid does.
def _settings_view():
    enabled = _event_hook_enabled()
    url = store_kv_get("sendgrid", "event_hook_url")
    if url == None:
        url = ""
    return {
        "enabled": enabled,
        "url": url,
        "friendly_name": "stunt event webhook",
        "group_resend": True,
        "oauth_token": "",
        "oauth_client_id": "",
        "oauth_client_secret": "",
    }
