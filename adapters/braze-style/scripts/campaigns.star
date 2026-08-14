# Campaign handlers — Braze REST API.
#
# POST /campaigns/trigger/send → trigger a campaign send

def on_trigger_send(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    campaign_id = body.get("campaign_id", "")
    if campaign_id == None:
        campaign_id = ""

    dispatch_id = "trigger-disp-" + str(store_kv_incr("braze", "trigger_seq") + 1)

    # Deliver an unsigned webhook event for the campaign dispatch
    # (unsigned-by-design: see lib.star).
    _emit_if_subscribed("campaign.sent", {
        "dispatch_id": dispatch_id,
        "campaign_id": campaign_id,
        "timestamp": clock.now_rfc3339(),
    })

    return respond(200, {
        "message": "success",
        "dispatch_id": dispatch_id,
        "campaign_id": campaign_id,
    })
