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

    return respond(200, {
        "message": "success",
        "dispatch_id": "trigger-disp-" + str(store_kv_incr("braze", "trigger_seq") + 1),
        "campaign_id": campaign_id,
    })
