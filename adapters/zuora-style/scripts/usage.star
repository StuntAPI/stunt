# Usage handlers — Zuora metered billing usage records.
#
# POST /v1/usage  -> record usage ({AccountId, Quantity, StartDateTime, UOM})
# GET  /v1/usage  -> list usage records

# Shared helpers from lib.star.

def on_record_usage(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)

    # Zuora usage can be a single object or an array.
    items = []
    if body == None:
        items = []
    elif "records" in body:
        items = body.get("records", [])
    elif "_batch" in body:
        items = body.get("_batch", [])
    else:
        items = [body]

    if items == None:
        items = []

    col = store_collection("usage")
    results = []
    for item in items:
        usage_id = _next_id("usage")
        doc = {
            "id": usage_id,
            "usageId": usage_id,
            "AccountId": item.get("AccountId", ""),
            "Quantity": item.get("Quantity", 0),
            "StartDateTime": item.get("StartDateTime", _now()),
            "EndDateTime": item.get("EndDateTime", _now()),
            "UOM": item.get("UOM", "Each"),
            "Status": "Imported",
        }
        col.insert(doc)
        results.append({"Success": True, "UsageId": usage_id})

    return respond(200, {
        "success": True,
        "usageRecords": results,
    })

def on_list_usage(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("usage")
    docs = col.list()

    account_filter = _get_query(req, "AccountId", "")

    usage = []
    for d in docs:
        if account_filter != "":
            if d.get("AccountId", "") != account_filter:
                continue
        usage.append({
            "usageId": d.get("usageId", ""),
            "AccountId": d.get("AccountId", ""),
            "Quantity": d.get("Quantity", 0),
            "StartDateTime": d.get("StartDateTime", _now()),
            "UOM": d.get("UOM", "Each"),
            "Status": d.get("Status", "Imported"),
        })

    return respond(200, {
        "success": True,
        "usage": usage,
    })
