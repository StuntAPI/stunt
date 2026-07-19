# Metrics handler — memberCreatorPostAnalytics.
#
# GET /rest/memberCreatorPostAnalytics?q=entity&entity=(ugcPost:<urn>)&queryType=<M>
#   (Bearer) -> { elements:[{count, metricType, targetEntity, dateRange}], paging }
#
# Distinct per-queryType totals:
#   REACTION   -> base + 3
#   COMMENT    -> base + 5
#   RESHARE    -> base + 7
#   IMPRESSION -> base + 11
# where base = post seq.

# Shared helpers (_bearer, _member_for_token, _to_int) are preloaded from
# scripts/lib.star.

# on_analytics returns daily metric buckets for a post.
def on_analytics(req):
    member = _member_for_token(req)
    if member == None:
        return respond(401, {"status": 401, "code": "AUTHORIZED", "message": "token"})

    query_type = req["query"].get("queryType", "")
    raw_entity = req["query"].get("entity", "")
    start = _to_int(req["query"].get("start", "0"))

    # Extract the entity URN from "(<kind>:<urn>)".
    entity_urn = raw_entity
    if len(raw_entity) > 0 and raw_entity[0] == "(" and raw_entity[-1] == ")":
        inner = raw_entity[1:-1]
        colon_idx = inner.find(":")
        if colon_idx >= 0:
            entity_urn = inner[colon_idx + 1:]
        else:
            entity_urn = inner

    # Extract the seq from the URN (last colon-separated segment).
    seq = ""
    colon_idx = entity_urn.rfind(":")
    if colon_idx >= 0:
        seq = entity_urn[colon_idx + 1:]
    else:
        seq = entity_urn

    # Verify a ugcPost with this seq exists.
    known_ugc = "urn:li:ugcPost:" + seq
    pc = store_collection("posts")
    post = pc.get(known_ugc)
    if post == None:
        return respond(404, {"status": 404, "message": "entity not found"})

    base = _to_int(seq)

    totals = {
        "REACTION": base + 3,
        "COMMENT": base + 5,
        "RESHARE": base + 7,
        "IMPRESSION": base + 11,
    }
    total = totals.get(query_type, base)

    # Paging: past the data -> empty page.
    if start > 0:
        return respond(200, {
            "elements": [],
            "paging": {"count": 0, "start": start, "links": []},
        })

    lo = total // 2
    hi = total - lo
    elements = [
        _bucket(lo, query_type, known_ugc, 0),
        _bucket(hi, query_type, known_ugc, 1),
    ]
    return respond(200, {
        "elements": elements,
        "paging": {"count": len(elements), "start": start, "links": []},
    })

def _bucket(count, query_type, ugc_urn, i):
    return {
        "count": count,
        "metricType": {
            "com.linkedin.adsexternalapi.memberanalytics.v1.CreatorPostAnalyticsMetricTypeV1": query_type,
        },
        "targetEntity": {"ugcPost": ugc_urn},
        "dateRange": {
            "start": {"day": 1 + i, "month": 6, "year": 2026},
            "end": {"day": 2 + i, "month": 6, "year": 2026},
        },
    }


