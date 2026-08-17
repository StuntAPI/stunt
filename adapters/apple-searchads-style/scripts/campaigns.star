# Campaign handlers — Apple Search Ads API.
#
# POST /api/v4/campaigns/find → {data: [campaigns], pagination}
# POST /api/v4/campaigns → create campaign
# GET  /api/v4/campaigns/{id} → campaign detail
# PUT  /api/v4/campaigns/{id} → update campaign (name/budgets/status)
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

    # Real find selector (conditions + orderBy + pagination), applied like
    # the real API: filter, sort, then slice.
    result, total, offset, limit = _apply_campaign_selector(body, result)

    return respond(200, {
        "data": result,
        "pagination": {
            "offset": offset,
            "limit": limit,
            "totalResults": total,
        },
    })

def on_create_campaign(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    body = req["body"]
    if body == None:
        body = {}

    name = body.get("name") or ""
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
        "creationTime": _asa_now_ts(),
        "modificationTime": _asa_now_ts(),
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
    doc = _asa_find_campaign(cc, campaign_id)

    if doc == None:
        return respond(404, _err("Campaign not found"))

    return respond(200, {"data": _campaign_obj(doc)})

# on_update_campaign handles PUT /api/v4/campaigns/{id} — the real API's
# Update a Campaign verb. The body may carry name, budgetAmount,
# dailyBudgetAmount, and status (ENABLED/PAUSED; servingStatus values are
# accepted too); fields absent from the body keep their stored values. The
# real API has no campaign DELETE — pausing is the supported lifecycle end.
def on_update_campaign(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    _seed_campaigns()

    campaign_id = req["params"]["campaign_id"]
    body = req["body"]
    if body == None:
        body = {}

    cc = store_collection("campaigns")
    doc = _asa_find_campaign(cc, campaign_id)
    if doc == None:
        return respond(404, _err("Campaign not found"))

    name = body.get("name", None)
    if name != None and type(name) == "string" and name != "":
        doc["name"] = name

    budget = body.get("budgetAmount", None)
    if budget != None and type(budget) == "dict":
        doc["budgetAmount"] = budget

    daily = body.get("dailyBudgetAmount", None)
    if daily != None and type(daily) == "dict":
        doc["dailyBudgetAmount"] = daily

    status = body.get("status", body.get("servingStatus", None))
    if status != None:
        serving = _asa_status_to_serving(status)
        if serving == "":
            return respond(400, _err("Invalid status; use ENABLED or PAUSED"))
        doc["servingStatus"] = serving
        if serving == "PAUSED":
            doc["servingStateReasons"] = ["USER_PAUSED"]
        else:
            doc["servingStateReasons"] = []

    doc["modificationTime"] = _asa_now_ts()

    cc.update(doc["id"], doc)
    return respond(200, {"data": _campaign_obj(doc)})

def on_create_ad(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    campaign_id = req["params"]["campaign_id"]
    _seed_campaigns()
    cc = store_collection("campaigns")
    if _asa_find_campaign(cc, campaign_id) == None:
        return respond(404, _err("Campaign not found"))
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
    return _CAMP_ID_BASE + seq

# _next_ad_num generates the next numeric ad ID.
def _next_ad_num():
    seq = store_kv_incr("searchads", "ad_num")
    return _AD_ID_BASE + seq

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

# --- find selector helpers ---

# _campaign_cond_field maps a condition field name to a (possibly dotted)
# path on the campaign response object. Returns None for unknown fields.
def _campaign_cond_field(field):
    if field == "id" or field == "campaignId":
        return "campaignId"
    if field == "name" or field == "campaignName":
        return "name"
    if field == "budgetAmount":
        return "budgetAmount.amount"
    if field == "dailyBudgetAmount":
        return "dailyBudgetAmount.amount"
    if field == "servingStatus":
        return "servingStatus"
    if field == "creationTime":
        return "creationTime"
    if field == "modificationTime":
        return "modificationTime"
    return None

# _campaign_order_field maps an orderBy field name to a response path.
def _campaign_order_field(field):
    if field == "id" or field == "campaignId":
        return "campaignId"
    if field == "name" or field == "campaignName":
        return "name"
    if field == "budgetAmount":
        return "budgetAmount.amount"
    if field == "dailyBudgetAmount":
        return "dailyBudgetAmount.amount"
    if field == "servingStatus":
        return "servingStatus"
    if field == "creationTime":
        return "creationTime"
    if field == "modificationTime":
        return "modificationTime"
    return None

# _apply_campaign_selector applies the real campaigns/find selector to the
# response objects: selector.conditions filter, selector.orderBy sorts,
# selector.pagination slices. Returns (rows, total, offset, limit) with total
# counted before slicing.
def _apply_campaign_selector(body, rows):
    sel = _asa_selector(body)

    rows = _asa_apply_conditions(
        rows,
        sel.get("conditions", None),
        _campaign_cond_field,
        ["campaignId"],
        ["budgetAmount.amount", "dailyBudgetAmount.amount"],
    )

    rows = _asa_apply_order(rows, sel.get("orderBy", None), _campaign_order_field)

    total = len(rows)
    offset, limit = _asa_pagination(sel)
    rows = query_select(rows, None, "", "", limit, offset, None)
    return rows, total, offset, limit
