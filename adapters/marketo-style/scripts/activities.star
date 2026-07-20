# Activities handlers — Marketo lead activities with paging tokens.
#
# GET /rest/v1/activities/pagingtoken?sinceDatetime= -> {nextPageToken}
# GET /rest/v1/activities?activityTypeIds=&nextPageToken=
#   -> {success, requestId, result:[...], nextPageToken, moreResult}
#
# Marketo uses cursor-style paging tokens for activities. The paging token is
# an opaque string; clients get one from the pagingtoken endpoint and pass it
# as nextPageToken to fetch activities in pages.

# Shared helpers from lib.star.

def on_paging_token(req):
    ok, err = _require_auth(req)
    if not ok:
        return _marketo_unauth()
    if _check_quota():
        return _quota_err()

    since = _get_query(req, "sinceDatetime", "")

    # Generate a synthetic paging token. Marketo paging tokens are typically
    # numeric and represent a point in time.
    seq = store_kv_incr("marketo", "paging_seq")
    token = str(100000 + seq)

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "nextPageToken": token,
        "moreResult": False,
    })

def on_list_activities(req):
    ok, err = _require_auth(req)
    if not ok:
        return _marketo_unauth()
    if _check_quota():
        return _quota_err()

    activity_type_ids = _get_query(req, "activityTypeIds", "")
    page_token = _get_query(req, "nextPageToken", "")

    col = store_collection("activities")
    docs = col.list()

    # Filter by activity type IDs if specified (comma-separated).
    type_filter = []
    if activity_type_ids != "":
        type_filter = _split(activity_type_ids, ",")

    result = []
    for d in docs:
        if len(type_filter) > 0:
            atype = d.get("activityTypeId", "")
            matched = False
            for tf in type_filter:
                if atype == _trim(tf):
                    matched = True
                    break
            if not matched:
                continue

        result.append({
            "id": d.get("id", ""),
            "leadId": d.get("leadId", ""),
            "activityDate": d.get("activityDate", _now()),
            "activityTypeId": d.get("activityTypeId", ""),
            "activityType": d.get("activityType", ""),
            "primaryAttributeValue": d.get("primaryAttributeValue", ""),
            "attributes": d.get("attributes", {}),
        })

    # Determine if there are more results. This mock returns all results in
    # one page unless a page size is simulated. We set moreResult=False since
    # we return everything.
    more = False
    next_token = None

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": result,
        "nextPageToken": next_token,
        "moreResult": more,
    })
