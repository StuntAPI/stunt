# Keywords handlers — Apple Search Ads API.
#
# Targeting keywords are stored in the `keywords` collection, scoped to the
# route's campaign, and seeded with defaults the first time the campaign's
# keywords are touched.
#
# POST   /api/v4/campaigns/{campaign_id}/keywords/targeting/find
#   → {data: [{id, text, matchType, bidAmount, status}], pagination}
# POST   /api/v4/campaigns/{campaign_id}/keywords/targeting
#   → {data: <keyword>} (create; a list body creates many → {data: [...]})
# PUT    /api/v4/campaigns/{campaign_id}/keywords/targeting/bulk
#   → {data: [<keyword>]} (bulk update: [{id, bidAmount?, status?, ...}])
# PUT    /api/v4/campaigns/{campaign_id}/keywords/targeting/{keyword_id}
#   → {data: <keyword>} (single update)
# DELETE /api/v4/campaigns/{campaign_id}/keywords/targeting/{keyword_id}
#   → 204 no content
#
# Shared helpers (_require_auth, _err, _asa_* keyword helpers) are preloaded.

def _resolve_campaign(req):
    # Returns (campaign_num, campaign_doc, None) or (0, None, err_response).
    _seed_campaigns()
    campaign_id = req["params"]["campaign_id"]
    cc = store_collection("campaigns")
    doc = _asa_find_campaign(cc, campaign_id)
    if doc == None:
        return 0, None, respond(404, _err("Campaign not found"))
    cid = doc.get("campaignId", 0)
    if type(cid) == "float":
        cid = int(cid)
    _asa_seed_keywords(cid)
    return cid, doc, None

def on_find_keywords(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    campaign_num, doc, err = _resolve_campaign(req)
    if err != None:
        return err

    keywords = []
    for k in _asa_campaign_keywords(campaign_num):
        keywords.append(_asa_keyword_obj(k))

    # Real find selector (conditions + orderBy + pagination), applied like
    # the real API: filter, sort, then slice.
    keywords, total, offset, limit = _apply_keyword_selector(req, keywords)

    return respond(200, {
        "data": keywords,
        "pagination": {
            "offset": offset,
            "limit": limit,
            "totalResults": total,
        },
    })

# on_create_keyword handles POST .../keywords/targeting. The real API's bulk
# create takes a list body; a single object body is accepted too.
def on_create_keyword(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    campaign_num, doc, err = _resolve_campaign(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        return respond(400, _err("Request body is required"))
    if type(body) == "dict":
        batch = body.get("_batch", None)
        if batch != None and type(batch) == "list":
            # Top-level JSON array bodies arrive wrapped in _batch.
            items = batch
        else:
            items = [body]
    elif type(body) == "list":
        items = body
    else:
        return respond(400, _err("Body must be a keyword or a list of keywords"))

    created = []
    for item in items:
        if item == None or type(item) != "dict":
            return respond(400, _err("Each keyword must be an object"))
        text = item.get("text", item.get("keyword", ""))
        if text == None or text == "":
            return respond(400, _err("Keyword text is required"))
        match_type = item.get("matchType", "BROAD")
        bid = item.get("bidAmount", None)
        if bid != None and type(bid) == "dict":
            amount = _asa_amount_str(bid.get("amount", ""))
        else:
            amount = _asa_amount_str(bid)
        if amount == "":
            amount = "0.50"
        kdoc = _asa_insert_keyword(campaign_num, text, match_type, amount)
        created.append(_asa_keyword_obj(kdoc))

    if len(created) == 1 and type(body) == "dict":
        return respond(200, {"data": created[0]})
    return respond(200, {"data": created})

# _apply_keyword_updates merges a partial update body onto a stored keyword
# doc and persists it. Returns (keyword_obj, None) or (None, err_response).
def _apply_keyword_updates(campaign_num, kdoc, updates):
    if updates == None or type(updates) != "dict":
        return None, respond(400, _err("Update body must be an object"))

    text = updates.get("text", updates.get("keyword", None))
    if text != None:
        if type(text) != "string" or text == "":
            return None, respond(400, _err("Keyword text must be a non-empty string"))
        kdoc["text"] = text

    match_type = updates.get("matchType", None)
    if match_type != None and type(match_type) == "string" and match_type != "":
        kdoc["matchType"] = match_type

    bid = updates.get("bidAmount", None)
    if bid != None:
        if type(bid) == "dict":
            amount = _asa_amount_str(bid.get("amount", ""))
            currency = bid.get("currency", None)
        else:
            amount = _asa_amount_str(bid)
            currency = None
        if amount == "":
            return None, respond(400, _err("bidAmount.amount is required"))
        new_bid = {"amount": amount, "currency": kdoc["bidAmount"].get("currency", "USD")}
        if currency != None and type(currency) == "string" and currency != "":
            new_bid["currency"] = currency
        kdoc["bidAmount"] = new_bid

    status = updates.get("status", None)
    if status != None:
        if type(status) != "string":
            return None, respond(400, _err("status must be a string"))
        s = status.upper()
        if s == "ACTIVE" or s == "PAUSED":
            kdoc["status"] = s
        else:
            return None, respond(400, _err("Invalid status; use ACTIVE or PAUSED"))

    kc = store_collection("keywords")
    kc.update(kdoc["id"], kdoc)
    return _asa_keyword_obj(kdoc), None

# on_update_keyword handles PUT .../keywords/targeting/{keyword_id}.
def on_update_keyword(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    campaign_num, doc, err = _resolve_campaign(req)
    if err != None:
        return err

    kdoc = _asa_find_keyword(campaign_num, req["params"]["keyword_id"])
    if kdoc == None:
        return respond(404, _err("Keyword not found"))

    updated, uerr = _apply_keyword_updates(campaign_num, kdoc, req["body"])
    if uerr != None:
        return uerr
    return respond(200, {"data": updated})

# on_bulk_update_keywords handles PUT .../keywords/targeting/bulk — the real
# API's bulk Update Targeting Keywords: a list body of {id, ...updates}.
def on_bulk_update_keywords(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    campaign_num, doc, err = _resolve_campaign(req)
    if err != None:
        return err

    body = req["body"]
    items = body
    if body != None and type(body) == "dict":
        # Top-level JSON array bodies arrive wrapped in _batch.
        batch = body.get("_batch", None)
        if batch != None and type(batch) == "list":
            items = batch
    if items == None or type(items) != "list":
        return respond(400, _err("Body must be a list of keyword updates"))

    body = items

    updated = []
    for item in body:
        if item == None or type(item) != "dict":
            return respond(400, _err("Each update must be an object with id"))
        kw_id = item.get("id", None)
        if kw_id == None:
            return respond(400, _err("Each update must carry the keyword id"))
        if type(kw_id) == "float":
            kw_id = str(int(kw_id))
        elif type(kw_id) != "string":
            kw_id = str(kw_id)
        kdoc = _asa_find_keyword(campaign_num, kw_id)
        if kdoc == None:
            return respond(404, _err("Keyword not found: " + str(kw_id)))
        obj, uerr = _apply_keyword_updates(campaign_num, kdoc, item)
        if uerr != None:
            return uerr
        updated.append(obj)

    return respond(200, {"data": updated})

# on_delete_keyword handles DELETE .../keywords/targeting/{keyword_id}.
def on_delete_keyword(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    campaign_num, doc, err = _resolve_campaign(req)
    if err != None:
        return err

    kdoc = _asa_find_keyword(campaign_num, req["params"]["keyword_id"])
    if kdoc == None:
        return respond(404, _err("Keyword not found"))

    kc = store_collection("keywords")
    kc.delete(kdoc["id"])
    return respond(204, "")

# --- find selector helpers ---

# _keyword_cond_field maps a condition field name to a (possibly dotted)
# path on the keyword response object. Returns None for unknown fields.
def _keyword_cond_field(field):
    if field == "id" or field == "keywordId":
        return "id"
    if field == "keyword" or field == "text":
        return "text"
    if field == "matchType":
        return "matchType"
    if field == "bidAmount":
        return "bidAmount.amount"
    if field == "status":
        return "status"
    return None

# _keyword_order_field maps an orderBy field name to a response path.
def _keyword_order_field(field):
    return _keyword_cond_field(field)

# _apply_keyword_selector applies the real keywords targeting/find selector:
# selector.conditions filter, selector.orderBy sorts, selector.pagination
# slices. Returns (rows, total, offset, limit) with total counted before
# slicing.
def _apply_keyword_selector(req, rows):
    body = req.get("body")
    if body == None:
        body = {}
    sel = _asa_selector(body)

    rows = _asa_apply_conditions(
        rows,
        sel.get("conditions", None),
        _keyword_cond_field,
        ["id"],
        ["bidAmount.amount"],
    )

    rows = _asa_apply_order(rows, sel.get("orderBy", None), _keyword_order_field)

    total = len(rows)
    offset, limit = _asa_pagination(sel)
    rows = query_select(rows, None, "", "", limit, offset, None)
    return rows, total, offset, limit
