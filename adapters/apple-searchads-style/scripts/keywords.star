# Keywords handler — Apple Search Ads API.
#
# POST /api/v4/campaigns/{campaign_id}/keywords/targeting/find
#   → {data: [{keyword, matchType, bidAmount: {amount, currency}}], pagination}

# Shared helpers (_require_auth, _err) are preloaded.

def on_find_keywords(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    campaign_id = req["params"]["campaign_id"]

    keywords = [
        {
            "keyword": "photo editor",
            "matchType": "BROAD",
            "bidAmount": {"amount": "0.50", "currency": "USD"},
        },
        {
            "keyword": "edit photos",
            "matchType": "EXACT",
            "bidAmount": {"amount": "1.00", "currency": "USD"},
        },
        {
            "keyword": "[photo editing app]",
            "matchType": "EXACT",
            "bidAmount": {"amount": "2.50", "currency": "USD"},
        },
    ]

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

# --- find selector helpers ---

# _keyword_cond_field maps a condition field name to a (possibly dotted)
# path on the keyword response object. Returns None for unknown fields.
def _keyword_cond_field(field):
    if field == "keyword" or field == "text":
        return "keyword"
    if field == "matchType":
        return "matchType"
    if field == "bidAmount":
        return "bidAmount.amount"
    return None

# _keyword_order_field maps an orderBy field name to a response path.
def _keyword_order_field(field):
    if field == "keyword" or field == "text":
        return "keyword"
    if field == "matchType":
        return "matchType"
    if field == "bidAmount":
        return "bidAmount.amount"
    return None

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
        [],
        ["bidAmount.amount"],
    )

    rows = _asa_apply_order(rows, sel.get("orderBy", None), _keyword_order_field)

    total = len(rows)
    offset, limit = _asa_pagination(sel)
    rows = query_select(rows, None, "", "", limit, offset, None)
    return rows, total, offset, limit
