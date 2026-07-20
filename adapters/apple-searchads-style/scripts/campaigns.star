# Campaign handlers — Apple Search Ads API.
#
# POST /api/v4/campaigns/find → {data: [campaigns], pagination}
# POST /api/v4/campaigns → create campaign
# GET  /api/v4/campaigns/{id} → campaign detail
# POST /api/v4/campaigns/{id}/ads → create ad

# Shared helpers (_require_auth, _err, _seed_campaigns, _gen_campaign_id,
# _pad6) are preloaded.

def on_find_campaigns(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    _seed_campaigns()

    body = req["body"]
    if body == None:
        body = {}

    cc = store_collection("campaigns")
    all_camps = cc.list()

    result = []
    for c in all_camps:
        result.append(_campaign_obj(c))

    return respond(200, {
        "data": result,
        "pagination": {
            "offset": 0,
            "limit": 1000,
            "totalResults": len(result),
        },
    })

def on_create_campaign(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    body = req["body"]
    if body == None:
        body = {}

    name = body.get("name", "")
    if name == "":
        return respond(400, _err("Campaign name is required"))

    campaign_id_num = _next_campaign_num()
    internal_id = _gen_campaign_id()

    cc = store_collection("campaigns")
    cc.insert({
        "id": internal_id,
        "campaignId": campaign_id_num,
        "name": name,
        "budgetAmount": body.get("budgetAmount", {"amount": "1000", "currency": "USD"}),
        "dailyBudgetAmount": body.get("dailyBudgetAmount", {"amount": "100", "currency": "USD"}),
        "servingStatus": body.get("servingStatus", "PAUSED"),
        "servingStateReasons": [],
        "creationTime": "2024-01-15T10:00:00.000",
        "modificationTime": "2024-01-15T10:00:00.000",
    })

    doc = cc.get(internal_id)
    return respond(200, {"data": _campaign_obj(doc)})

def on_get_campaign(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    _seed_campaigns()

    campaign_id = req["params"]["campaign_id"]
    cc = store_collection("campaigns")

    # Try numeric campaignId match first, then internal id.
    doc = None
    for c in cc.list():
        cid = c.get("campaignId", 0)
        # JSON numbers arrive as floats; convert to int string.
        cid_str = str(int(cid)) if type(cid) == "float" else str(cid)
        if cid_str == campaign_id or c.get("id", "") == campaign_id:
            doc = c
            break

    if doc == None:
        return respond(404, _err("Campaign not found"))

    return respond(200, {"data": _campaign_obj(doc)})

def on_create_ad(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    campaign_id = req["params"]["campaign_id"]
    body = req["body"]
    if body == None:
        body = {}

    seq = store_kv_incr("searchads", "ad_seq")
    ad_id = _next_ad_num()

    ac = store_collection("ads")
    ad = {
        "id": "ad_" + _pad6(seq),
        "adId": ad_id,
        "campaignId": _to_int(campaign_id),
        "name": body.get("name", "Ad Group " + str(ad_id)),
        "servingStatus": body.get("servingStatus", "PAUSED"),
        "servingStateReasons": [],
    }
    ac.insert(ad)

    return respond(200, {"data": ad})

# _campaign_obj formats a campaign for the API response.
def _campaign_obj(c):
    return {
        "campaignId": c.get("campaignId", 0),
        "name": c.get("name", ""),
        "budgetAmount": c.get("budgetAmount", {"amount": "0", "currency": "USD"}),
        "dailyBudgetAmount": c.get("dailyBudgetAmount", {"amount": "0", "currency": "USD"}),
        "servingStatus": c.get("servingStatus", "PAUSED"),
        "servingStateReasons": c.get("servingStateReasons", []),
        "creationTime": c.get("creationTime", ""),
        "modificationTime": c.get("modificationTime", ""),
    }

# _next_campaign_num generates the next numeric campaign ID.
def _next_campaign_num():
    seq = store_kv_incr("searchads", "campaign_num")
    return 543210000 + seq

# _next_ad_num generates the next numeric ad ID.
def _next_ad_num():
    seq = store_kv_incr("searchads", "ad_num")
    return 1000000 + seq

# _to_int parses a string to int.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return n
    return n
