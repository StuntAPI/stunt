# Sites + sitemaps handlers — Google Search Console API.
#
# GET    /webmasters/v3/sites                     → {siteEntry:[…]}
# GET    /webmasters/v3/sites/{siteUrl}           → one site entry
# PUT    /webmasters/v3/sites/{siteUrl}           → sites.add (204, starts
#                                                   unverified — see lib.star)
# DELETE /webmasters/v3/sites/{siteUrl}           → sites.delete (204/404)
# GET    /webmasters/v3/sites/{siteUrl}/sitemaps  → {sitemap:[…]}
# GET    /webmasters/v3/sites/{siteUrl}/sitemaps/{feedpath}   → one sitemap
# PUT    /webmasters/v3/sites/{siteUrl}/sitemaps/{feedpath}   → submit (204)
# DELETE /webmasters/v3/sites/{siteUrl}/sitemaps/{feedpath}   → remove (204)

def on_list_sites(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    sc = store_collection("sites")
    items = []
    for doc in sc.list():
        items.append(_site_view(doc))

    # Apply Search Console pagination (maxResults + pageToken) after listing.
    page, next_token = _list_page(req, items)
    result = {"siteEntry": page}
    if next_token != None:
        result["nextPageToken"] = next_token
    return respond(200, result)

# on_get_site returns a single site entry (404 when unknown).
def on_get_site(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    doc = _resolve_site(req["params"].get("siteUrl", ""))
    if doc == None:
        return _not_found_err("Site '" + _normalize_site(req["params"].get("siteUrl", "")) + "' not found.")
    return respond(200, _site_view(doc))

# on_add_site implements sites.add: PUT a property URL. The real API answers
# 204 with an empty body; the site is listed immediately with permissionLevel
# siteUnverifiedUser and completes verification via the derive-on-read
# lifecycle in lib.star (_VERIFY_SECONDS).
def on_add_site(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    raw = req["params"].get("siteUrl", "")
    site_url = _normalize_site(raw)
    if not _valid_site_url(site_url):
        return _invalid_argument("Invalid site URL: must be an https:// URL-prefix property or a sc-domain: property.")

    existing = _resolve_site(site_url)
    if existing != None:
        # Re-adding a known property is a no-op, like the real API.
        return respond(204)

    sc = store_collection("sites")
    sc.insert(_site_doc(site_url, "siteUnverifiedUser", clock.now_unix() + _VERIFY_SECONDS))
    return respond(204)

# on_delete_site implements sites.delete (204; 404 when unknown).
def on_delete_site(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    raw = req["params"].get("siteUrl", "")
    doc = _resolve_site(raw)
    if doc == None:
        return _not_found_err("Site '" + _normalize_site(raw) + "' not found.")
    store_collection("sites").delete(doc.get("id", ""))

    # A deleted property no longer has sitemaps.
    smc = store_collection("sitemaps")
    for sm in smc.list():
        if sm.get("siteUrl", "") == doc.get("siteUrl", ""):
            smc.delete(sm.get("id", ""))
    return respond(204)

# _valid_site_url checks the two real property forms: URL-prefix (http(s)://…)
# or domain (sc-domain:example.com).
def _valid_site_url(site_url):
    if site_url[:10] == "sc-domain:":
        return len(site_url) > 10 and _contains(site_url[10:], ".")
    return site_url[:8] == "https://" or site_url[:7] == "http://"

# --- sitemaps ---

def on_list_sitemaps(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    site, err = _require_site(req)
    if err != None:
        return err

    items = []
    for sm in store_collection("sitemaps").list():
        if sm.get("siteUrl", "") == site.get("siteUrl", ""):
            items.append(_sitemap_view(sm))

    # Apply Search Console pagination (maxResults + pageToken).
    page, next_token = _list_page(req, items)
    result = {"sitemap": page}
    if next_token != None:
        result["nextPageToken"] = next_token
    return respond(200, result)

# on_get_sitemap returns one sitemap entry (404 when unknown).
def on_get_sitemap(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    site, err = _require_site(req)
    if err != None:
        return err

    doc = _find_sitemap(site.get("siteUrl", ""), req["params"].get("feedpath", ""))
    if doc == None:
        return _not_found_err("Sitemap not found: " + req["params"].get("feedpath", ""))
    return respond(200, _sitemap_view(doc))

# on_submit_sitemap implements sitemaps.submit: PUT a feedpath under a
# verified property → 204, recorded with lastSubmitted = now.
def on_submit_sitemap(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    site, err = _require_site(req)
    if err != None:
        return err

    feedpath = req["params"].get("feedpath", "")
    if feedpath == "":
        return _invalid_argument("feedpath is required.")
    smc = store_collection("sitemaps")
    existing = _find_sitemap(site.get("siteUrl", ""), feedpath)
    now = clock.now_rfc3339()
    if existing != None:
        existing["lastSubmitted"] = now
        existing["isPending"] = False
        smc.update(existing["id"], existing)
        return respond(204)
    smc.insert(_sitemap_doc(site.get("siteUrl", ""), feedpath, now))
    return respond(204)

# on_delete_sitemap implements sitemaps.delete (204; 404 when unknown).
def on_delete_sitemap(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    site, err = _require_site(req)
    if err != None:
        return err

    feedpath = req["params"].get("feedpath", "")
    doc = _find_sitemap(site.get("siteUrl", ""), feedpath)
    if doc == None:
        return _not_found_err("Sitemap not found: " + feedpath)
    store_collection("sitemaps").delete(doc.get("id", ""))
    return respond(204)

# _find_sitemap locates a sitemap by site + feedpath. The feedpath is a
# single path segment (the engine routes percent-encoded slashes as separate
# segments, so full-URL feedpaths are addressed by their final segment).
def _find_sitemap(site_url, feedpath):
    for sm in store_collection("sitemaps").list():
        if sm.get("siteUrl", "") != site_url:
            continue
        if sm.get("feedpath", "") == feedpath:
            return sm
        full = sm.get("path", "")
        if full != "" and full == feedpath:
            return sm
        if full != "" and full[(full.rfind("/") + 1):] == feedpath and _contains(full, feedpath):
            return sm
    return None
