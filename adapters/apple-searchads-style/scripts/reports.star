# Reports handler — Apple Search Ads API.
#
# POST /api/v4/reports/campaigns
#   Body: {startTime, endTime, selector: {orderBy, conditions}, returnRecords: true}
#   → {data: {reportingDataResponse: {row: [{campaignId, ...metrics}], totalCount, ...}}}

# Shared helpers (_require_auth, _err, _seed_campaigns) are preloaded.

def on_report_campaigns(req):
    if not _require_auth(req):
        return respond(401, _err("Missing or invalid authorization"))

    body = req.get("body")
    if body == None:
        body = {}

    # startTime and endTime are required by the real reports endpoint.
    start_time = body.get("startTime", "")
    if start_time == None or start_time == "":
        return respond(400, _err("startTime is required"))
    end_time = body.get("endTime", "")
    if end_time == None or end_time == "":
        return respond(400, _err("endTime is required"))

    # The campaigns report groups by campaign only; the real API rejects
    # other groupBy keys for this endpoint.
    group_by = body.get("groupBy", None)
    if group_by != None:
        if type(group_by) != "list":
            return respond(400, _err("groupBy must be a list"))
        for g in group_by:
            if g != "campaign":
                return respond(400, _err("Unsupported groupBy for the campaigns report: " + str(g)))

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

    # Real report selector: conditions filter the rows (same operators as
    # find), then orderBy sorts them (e.g. impressions DESCENDING). Applied
    # after rows are built.
    sel = _asa_selector(body)
    rows = _asa_apply_conditions(
        rows,
        sel.get("conditions", None),
        _report_order_field,
        ["campaignId"],
        ["spend.amount", "avgCPT.amount", "avgCPA.amount"],
    )
    rows = _asa_apply_order(rows, sel.get("orderBy", None), _report_order_field)

    # grandTotals aggregate the returned rows (the real response carries
    # totals for the metrics in scope).
    tot_impressions = 0
    tot_taps = 0
    tot_installs = 0
    tot_spend = 0
    for r in rows:
        tot_impressions = tot_impressions + r.get("impressions", 0)
        tot_taps = tot_taps + r.get("taps", 0)
        tot_installs = tot_installs + r.get("installs", 0)
        spend = r.get("spend", None)
        if spend != None and type(spend) == "dict":
            amt = _asa_to_float(spend.get("amount", "0"))
            tot_spend = tot_spend + amt

    return respond(200, {
        "data": {
            "reportingDataResponse": {
                "row": rows,
                "startTime": start_time,
                "endTime": end_time,
                "totalCount": len(rows),
                "grandTotals": {
                    "impressions": tot_impressions,
                    "taps": tot_taps,
                    "installs": tot_installs,
                    "spend": {"amount": _asa_num_str(tot_spend), "currency": "USD"},
                },
            },
        },
    })

# --- report selector helpers ---

# _report_order_field maps a report selector orderBy field name to a
# (possibly dotted) path on a report row. Returns None for unknown fields.
def _report_order_field(field):
    if field == "campaignId" or field == "id":
        return "campaignId"
    if field == "campaignName" or field == "name":
        return "campaignName"
    if field == "servingStatus":
        return "servingStatus"
    if field == "impressions":
        return "impressions"
    if field == "taps":
        return "taps"
    if field == "installs":
        return "installs"
    if field == "spend":
        return "spend.amount"
    if field == "avgCPT":
        return "avgCPT.amount"
    if field == "avgCPA":
        return "avgCPA.amount"
    if field == "conversionRate":
        return "conversionRate"
    if field == "ttr":
        return "ttr"
    return None
