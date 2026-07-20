# Search analytics handler — Google Search Console API.
#
# POST /webmasters/v3/sites/{siteUrl}/searchAnalytics/query
#   Body: {startDate, endDate, dimensions:["query"|"page"|"country"], rowLimit}
#   → {rows:[{keys:[], clicks, impressions, ctr, position}]}

# Deterministic seeded search analytics data for realistic testing.
_QUERIES = ["how to tie a tie", "best running shoes", "python tutorial", "weather forecast", "recipe ideas"]
_PAGES = ["/guide/tie", "/products/shoes", "/docs/python", "/weather", "/recipes"]
_COUNTRIES = ["usa", "gbr", "ind", "deu", "fra"]

def on_query(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    dimensions = body.get("dimensions", ["query"])
    if dimensions == None:
        dimensions = ["query"]
    row_limit = body.get("rowLimit", 1000)
    if row_limit == None:
        row_limit = 1000
    row_limit = _to_int(str(row_limit))
    if row_limit == 0:
        row_limit = 1000

    rows = []
    for i in range(min(len(_QUERIES), row_limit)):
        keys = []
        for d in dimensions:
            if d == "query":
                keys.append(_QUERIES[i])
            elif d == "page":
                keys.append(_PAGES[i])
            elif d == "country":
                keys.append(_COUNTRIES[i])
            else:
                keys.append(_QUERIES[i])

        # Deterministic seeded metrics based on index.
        clicks = (i + 1) * 137 + 42
        impressions = clicks * 7 + 311
        ctr = float(clicks) / float(impressions)
        position = float(i) * 0.7 + 1.3

        rows.append({
            "keys": keys,
            "clicks": clicks,
            "impressions": impressions,
            "ctr": ctr,
            "position": position,
        })

    return respond(200, {
        "rows": rows,
        "responseAggregationType": "byProperty",
    })
