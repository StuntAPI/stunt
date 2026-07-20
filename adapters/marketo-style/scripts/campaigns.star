# Campaign handlers — Marketo smart campaigns.
#
# GET  /rest/v1/campaigns                          -> list campaigns
# POST /rest/v1/campaigns/{id}/trigger             -> trigger campaign for leads
#
# Marketo envelope: {success:true, requestId, result:[...], moreResult:false}

# Shared helpers from lib.star.

def _campaign_shape(doc):
    return {
        "id": doc.get("id", ""),
        "name": doc.get("name", ""),
        "description": doc.get("description", ""),
        "type": doc.get("type", ""),
        "isActive": doc.get("isActive", True),
        "createdAt": doc.get("createdAt", _now()),
        "updatedAt": doc.get("updatedAt", _now()),
    }

def on_list(req):
    ok, err = _require_auth(req)
    if not ok:
        return _marketo_unauth()
    if _check_quota():
        return _quota_err()

    col = store_collection("campaigns")
    docs = col.list()

    result = []
    for d in docs:
        result.append(_campaign_shape(d))

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": result,
        "moreResult": False,
    })

def on_trigger(req):
    ok, err = _require_auth(req)
    if not ok:
        return _marketo_unauth()
    if _check_quota():
        return _quota_err()

    campaign_id = req["params"].get("id", "")

    # Verify the campaign exists.
    col = store_collection("campaigns")
    doc = col.get(campaign_id)
    if doc == None:
        return respond(404, {
            "requestId": _request_id(),
            "success": False,
            "errors": [{"code": "604", "message": "Campaign not found"}],
            "moreResult": False,
        })

    body = _get_body(req)
    input_leads = body.get("input", [])
    if input_leads == None:
        input_leads = []

    # Record the trigger event as an activity.
    ac = store_collection("activities")
    for lead_entry in input_leads:
        lead_id = lead_entry.get("leadId", "")
        seq = store_kv_incr("marketo", "activity_seq")
        ac.insert({
            "id": str(55000 + seq),
            "leadId": str(lead_id),
            "activityDate": _now(),
            "activityTypeId": "51",
            "activityType": "Campaign Requested",
            "primaryAttributeValue": doc.get("name", ""),
            "attributes": {"campaignId": campaign_id},
        })

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": [],
        "moreResult": False,
    })
