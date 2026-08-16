# URL inspection handlers — Google Search Console URL Inspection API.
#
# POST /v1/urlInspection/index:inspect      (the real endpoint)
#   Body: {inspectionUrl, siteUrl, languageCode}
#   → {inspectionResult:{…}}
#
# POST /webmasters/v3/sites/{siteUrl}/inspect   (legacy simulator route)
#   Body: {inspectionUrl}
#
# The inspection derives deterministic verdicts from the inspected URL path
# against the SEEDED properties: a property you do not own (or have not
# verified) answers 403 PERMISSION_DENIED, a URL outside the property or a
# missing inspectionUrl answers 400 INVALID_ARGUMENT — both like the real
# URL Inspection API.

def on_index_inspect(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    body = _body_of(req)
    if body == None:
        return _invalid_argument("Request body is not a valid JSON object.")

    site_url = body.get("siteUrl", "")
    if site_url == None:
        site_url = ""
    return _inspect(req, site_url, body.get("inspectionUrl", None), body.get("languageCode", "en-US"))

def on_site_inspect(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    body = _body_of(req)
    if body == None:
        return _invalid_argument("Request body is not a valid JSON object.")

    return _inspect(req, req["params"].get("siteUrl", ""), body.get("inspectionUrl", None), body.get("languageCode", "en-US"))

# _inspect runs the shared inspection pipeline for a property + URL.
def _inspect(req, site_url, inspection_url, language_code):
    site, err = _require_site_property(site_url)
    if err != None:
        return err

    if inspection_url == None or inspection_url == "":
        return _invalid_argument("Invalid JSON payload received. Missing required field: inspectionUrl.")
    inspection_url = str(inspection_url)
    if inspection_url[:7] != "http://" and inspection_url[:8] != "https://":
        return _invalid_argument("Invalid URL: inspectionUrl must be a fully-qualified URL.")

    if not _url_under_site(inspection_url, site.get("siteUrl", "")):
        return _invalid_argument("Invalid URL: the inspectionUrl must be under the siteUrl property.")

    path = _url_path(inspection_url)
    low = path.lower()

    # Deterministic verdicts keyed off the URL path.
    verdict = "PASS"
    coverage_state = "Indexed"
    robots_state = "ALLOWED"
    indexing_state = "INDEXING_ALLOWED"
    fetch_state = "SUCCESSFUL"
    crawled_as = "SMARTPHONE"
    google_canonical = inspection_url
    user_canonical = inspection_url

    if _contains(low, "disallow"):
        robots_state = "DISALLOWED"
        verdict = "NEUTRAL"
        coverage_state = "Crawled - currently not indexed"
    elif _contains(low, "noindex"):
        indexing_state = "BLOCKED_BY_META_TAG"
        verdict = "NEUTRAL"
        coverage_state = "Excluded by 'noindex'"
    elif _contains(low, "canonical"):
        verdict = "PARTIAL"
        coverage_state = "Duplicate, Google chose different canonical"
        google_canonical = _site_origin(site.get("siteUrl", "")) + "/"
    elif _contains(low, "soft404"):
        fetch_state = "SOFT_404"
        verdict = "NEUTRAL"
        coverage_state = "Crawled - currently not indexed"
    elif _contains(low, "servererror"):
        fetch_state = "SERVER_ERROR"
        verdict = "NEUTRAL"
        coverage_state = "Server error (5xx)"

    rich_verdict = "NEUTRAL"
    detected_items = []
    if _contains(low, "structured") or _contains(low, "recipe") or _contains(low, "article"):
        rich_verdict = "PASS"
        detected_items = [{
            "itemType": "Search results",
            "items": [{"issueType": "MISSING_FIELD_LOGO", "severity": "ERROR"}],
        }]

    last_crawl = clock.unix_to_rfc3339(clock.now_unix() - 24 * 3600)
    result = {
        "inspectionResultLink": "https://search.google.com/search-console/inspect?entity=" + site.get("siteUrl", "") + "&id=" + inspection_url,
        "indexStatusResult": {
            "verdict": verdict,
            "coverageState": coverage_state,
            "robotsTxtState": robots_state,
            "indexingState": indexing_state,
            "lastCrawlTime": last_crawl,
            "pageFetchState": fetch_state,
            "googleCanonical": google_canonical,
            "userCanonical": user_canonical,
            "referringUrls": [_site_origin(site.get("siteUrl", "")) + _referring_path(low)],
            "crawledAs": crawled_as,
            "transportEncryptionStatus": "HTTPS",
        },
        "mobileUsabilityResult": {"verdict": "PASS", "issues": []},
        "richResultsResult": {"verdict": rich_verdict, "detectedItems": detected_items},
        "ampResult": {"verdict": "VERDICT_UNSPECIFIED"},
    }
    if language_code != None and language_code != "":
        result["languageCode"] = language_code
    return respond(200, {"inspectionResult": result})

# _require_site_property resolves + verifies a property by its identifier.
def _require_site_property(site_url):
    doc = _resolve_site(site_url)
    if doc == None:
        return None, _permission_denied("You don't have access to the property '" + _normalize_site(site_url) + "'.")
    if doc.get("permissionLevel", "") == "siteUnverifiedUser":
        return None, _permission_denied("The property '" + doc.get("siteUrl", "") + "' has not been verified in Search Console.")
    return doc, None

# _url_under_site reports whether the inspected URL belongs to the property:
# domain properties cover the registrable domain and all subdomains;
# URL-prefix properties cover their prefix.
def _url_under_site(url, site_url):
    site_url = _normalize_site(site_url)
    if site_url[:10] == "sc-domain:":
        domain = site_url[10:]
        host = _url_host(url)
        return host == domain or (len(host) > len(domain) and host[len(host) - len(domain) - 1:] == "." + domain)
    prefix = site_url
    if prefix[len(prefix) - 1:] != "/":
        prefix = prefix + "/"
    return url == site_url or url[:len(prefix)] == prefix

# _url_host extracts the host from a URL (empty when absent).
def _url_host(url):
    rest = url
    if rest[:8] == "https://":
        rest = rest[8:]
    elif rest[:7] == "http://":
        rest = rest[7:]
    end = len(rest)
    for i in range(len(rest)):
        ch = rest[i]
        if ch == "/" or ch == "?" or ch == "#":
            end = i
            break
    host = rest[:end]
    port = host.find(":")
    if port >= 0:
        host = host[:port]
    return host.lower()

# _url_path extracts the path portion of a URL ("/" when none).
def _url_path(url):
    rest = url
    if rest[:8] == "https://":
        rest = rest[8:]
    elif rest[:7] == "http://":
        rest = rest[7:]
    slash = rest.find("/")
    if slash < 0:
        return "/"
    end = len(rest)
    for i in range(slash, len(rest)):
        ch = rest[i]
        if ch == "?" or ch == "#":
            end = i
            break
    return rest[slash:end]

# _referring_path picks a stable seeded referring page for the URL.
def _referring_path(low):
    if _contains(low, "guide") or _contains(low, "docs"):
        return "/docs/python"
    if _contains(low, "product") or _contains(low, "shoes"):
        return "/products/shoes"
    return "/guide/tie"
