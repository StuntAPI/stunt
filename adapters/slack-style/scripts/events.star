# Events API configuration handler — set the app's Request URL.
#
# POST /api/apps.events.url
#   JSON {url, signing_secret?, events?} -> {ok:true, url, events: [...]}
#
# Real Slack has NO Web API endpoint for this: the Events API Request URL is
# configured in the app dashboard (Settings > Event Subscriptions), where you
# also supply the Signing Secret. When you save a Request URL, Slack performs
# the url_verification handshake — POSTing {type:"url_verification",
# challenge, token} to it, expecting the challenge echoed back.
#
# This local analog stores the Request URL (+ optional per-app signing secret
# and subscribed event types) and immediately delivers the same
# url_verification challenge to the URL, signed exactly like a real Events
# API delivery.
#
# Shared helpers (_require_auth, _ok, _err, _signed_emit) are preloaded from
# scripts/lib.star.

def on_set_events_url(req):
    err = _require_auth(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    url = body.get("url", "")
    if url == None:
        url = ""
    if url == "":
        return _err("invalid_url")

    signing_secret = body.get("signing_secret", "")
    if signing_secret == None:
        signing_secret = ""
    events = body.get("events", [])
    if events == None:
        events = []

    hook = {
        "id": "hook_" + str(store_kv_incr("slack", "hook_seq") + 1),
        "url": url,
        "signing_secret": signing_secret,
        "events": events,
        "created_at": clock.now_rfc3339(),
    }
    wc = store_collection("webhooks")
    wc.insert(hook)

    # Register the URL with the events emitter so events_emit delivers here.
    events_register(url)

    # Slack's handshake: deliver a signed url_verification challenge.
    _signed_emit("url_verification", {
        "type": "url_verification",
        "token": "stunt-verification-token",
        "challenge": "stunt_challenge_" + str(store_kv_incr("slack", "challenge_seq") + 1),
    })

    return _ok({"url": url, "events": events})
