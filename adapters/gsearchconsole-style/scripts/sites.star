# Sites handlers — Google Search Console API.
#
# GET  /webmasters/v3/sites → {siteEntry:[...]}
# GET  /webmasters/v3/sites/{siteUrl}/sitemaps → {sitemap:[...]}
# POST /webmasters/v3/sites/{siteUrl}/inspect → URL inspection

def on_list_sites(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    sc = store_collection("sites")
    items = []
    for s in sc.list():
        items.append({
            "siteUrl": s.get("siteUrl", ""),
            "permissionLevel": s.get("permissionLevel", "siteFullUser"),
        })

    return respond(200, {"siteEntry": items})

def on_list_sitemaps(req):
    err = _require_bearer(req)
    if err != None:
        return err

    site_url = req["params"]["siteUrl"]

    return respond(200, {
        "sitemap": [
            {
                "path": site_url + "sitemap.xml",
                "lastSubmitted": "2024-01-01T00:00:00.000Z",
                "lastDownloaded": "2024-01-01T00:00:00.000Z",
                "isPending": False,
                "isSitemapsIndex": False,
                "type": "sitemap",
                "errors": "0",
                "warnings": "0",
                "contents": [{"type": "web", "submitted": "42", "indexed": "38"}],
            },
        ],
    })

def on_inspect(req):
    err = _require_bearer(req)
    if err != None:
        return err

    site_url = req["params"]["siteUrl"]
    body = req["body"]
    if body == None:
        body = {}

    inspection_url = body.get("inspectionUrl", site_url)
    if inspection_url == None:
        inspection_url = site_url

    return respond(200, {
        "inspectionResult": {
            "inspectionResultLink": "https://search.google.com/search-console",
            "indexStatusResult": {
                "verdict": "PASS",
                "coverageState": "Indexed",
                "robotsTxtState": "ALLOWED",
                "indexingState": "INDEXING_ALLOWED",
                "lastCrawlTime": "2024-01-01T00:00:00.000Z",
                "googleCanonical": inspection_url,
                "userCanonical": inspection_url,
                "pageFetchState": "SUCCESSFUL",
                "crawledAs": "DESKTOP",
            },
            "mobileUsabilityResult": {
                "verdict": "PASS",
            },
            "richResultsResult": {
                "verdict": "PASS",
                "detectedItems": [],
            },
        },
    })
