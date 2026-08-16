# Campaign handlers — Braze REST API.
#
# POST /campaigns/trigger/send → trigger an API-triggered campaign send
#
# The campaign id is REQUIRED and validated against the campaigns store (the
# real fatal error "Invalid Campaign ID"); every dispatch is recorded in the
# dispatches collection with a real Braze dispatch id before its webhook is
# emitted.
#
# Shared helpers (_require_auth, _body_of, _bad_body, _fatal, _campaign,
# _next_dispatch_id, _record_dispatch, _emit_if_subscribed) are preloaded
# from scripts/lib.star.

_MAX_REQUEST_IDS = 50 # fatal error threshold for external ids in one request

def on_trigger_send(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body, ok = _body_of(req)
    if not ok:
        return _bad_body()

    # campaign_id is required for API-triggered delivery; validate it against
    # the campaigns store.
    campaign_id = body.get("campaign_id", None)
    if campaign_id == None or campaign_id == "":
        return _fatal("Invalid Campaign ID",
            "No Messaging API campaign was found for the campaign ID you provided.")
    campaign = _campaign(campaign_id)
    if campaign == None:
        return _fatal("Invalid Campaign ID",
            "No Messaging API campaign was found for the campaign ID you provided.")

    external_ids = body.get("external_user_ids", [])
    if external_ids == None:
        external_ids = []
    broadcast = body.get("broadcast", False)
    if broadcast == None:
        broadcast = False
    segment_id = body.get("segment_id", None)
    audience = body.get("audience", None)

    if len(external_ids) > _MAX_REQUEST_IDS:
        return _fatal("The max number of external_ids and aliases per request was exceeded",
            "Caused by calling more than " + str(_MAX_REQUEST_IDS) + " external ids.")
    if len(external_ids) == 0 and not broadcast and segment_id == None and audience == None:
        return _fatal("No Recipients",
            "There are no external IDs or segment IDs or no push tokens in the request.")

    dispatch_id = _next_dispatch_id()

    # Persist the dispatch record BEFORE emitting the webhook.
    channels = campaign.get("channels", [])
    if channels == None:
        channels = []
    _record_dispatch(dispatch_id, campaign_id, None, channels, len(external_ids), "sent")

    # Deliver an unsigned webhook event for the campaign dispatch
    # (unsigned-by-design: see lib.star).
    _emit_if_subscribed("campaign.sent", {
        "dispatch_id": dispatch_id,
        "campaign_id": campaign_id,
        "recipients": len(external_ids),
        "channels": channels,
        "timestamp": clock.now_rfc3339(),
    })

    out = {
        "message": "success",
        "dispatch_id": dispatch_id,
    }
    send_id = body.get("send_id", None)
    if send_id != None and send_id != "":
        out["send_id"] = send_id
    return respond(200, out)
