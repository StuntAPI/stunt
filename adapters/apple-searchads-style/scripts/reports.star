# Reports handler — Apple Search Ads API.
#
# POST /api/v4/reports/campaigns
#   Body: {startTime, endTime, selector: {orderBy, conditions}, returnRecords: true}
#   → {data: {reportingDataResponse: {row: [{campaignId, ...metrics}], totalCount, ...}}}

# Shared helpers (_require_auth, _err, _seed_campaigns) are preloaded.

def on_report_campaigns(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    _seed_campaigns()

    cc = store_collection("campaigns")
    all_camps = cc.list()

    rows = []
    for c in all_camps:
        rows.append({
            "campaignId": c.get("campaignId", 0),
            "campaignName": c.get("name", ""),
            "servingStatus": c.get("servingStatus", ""),
            "impressions": 10000 + c.get("campaignId", 0) % 50000,
            "taps": 500 + c.get("campaignId", 0) % 1000,
            "installs": 100 + c.get("campaignId", 0) % 500,
            "spend": {"amount": str(100 + c.get("campaignId", 0) % 500), "currency": "USD"},
            "avgCPT": {"amount": "0.85", "currency": "USD"},
            "avgCPA": {"amount": "5.20", "currency": "USD"},
            "conversionRate": 0.25,
            "ttr": 0.05,
        })

    return respond(200, {
        "data": {
            "reportingDataResponse": {
                "row": rows,
                "totalCount": len(rows),
                "grandTotals": {
                    "impressions": 0,
                    "taps": 0,
                    "installs": 0,
                    "spend": {"amount": "0", "currency": "USD"},
                },
            },
        },
    })
