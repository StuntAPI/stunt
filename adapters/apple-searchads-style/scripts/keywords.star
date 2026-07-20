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

    return respond(200, {
        "data": keywords,
        "pagination": {
            "offset": 0,
            "limit": 1000,
            "totalResults": len(keywords),
        },
    })
