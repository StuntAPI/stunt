# Shared library for gsearchconsole-style adapter scripts.

# Well-known static OAuth test token, seeded once into the KV store on first
# request (see _seed_tokens) so existing clients/tests that use it keep
# working while any other token is rejected with 401 — the same token-store
# model the whatsapp-style adapter uses.
_TEST_TOKEN = "mock-oauth2-token"

# _seed_tokens inserts the well-known test token into the KV store exactly
# once per instance (guarded by the "auth_seeded" flag), stored under
# "tok:<token>" with a far-future expiry computed at runtime.
def _seed_tokens():
    if store_kv_get("gsc", "auth_seeded") == "yes":
        return
    store_kv_set("gsc", "auth_seeded", "yes")
    exp = str(clock.now_unix() + 3600 * 24 * 365 * 10)
    store_kv_set("gsc", "tok:" + _TEST_TOKEN, exp)

# _bearer extracts the token from "Authorization: Bearer <t>".
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _require_bearer returns None if the token is known and unexpired, or a 401
# response if missing/unknown/expired.
def _require_bearer(req):
    token = _bearer(req)
    if token == "":
        return respond(401, {
            "error": {
                "code": 401,
                "message": "The request does not have valid authentication credentials.",
                "status": "UNAUTHENTICATED",
            },
        })
    _seed_tokens()
    exp = store_kv_get("gsc", "tok:" + token)
    if exp == None or clock.now_unix() > _to_int(exp):
        return respond(401, {
            "error": {
                "code": 401,
                "message": "Invalid Credentials: the access token is unknown or expired.",
                "status": "UNAUTHENTICATED",
            },
        })
    return None

# _g_err returns a Google-style error response.
def _g_err(code, message, status):
    return respond(code, {
        "error": {
            "code": code,
            "message": message,
            "status": status,
        },
    })

# _invalid_argument returns the standard Google 400 envelope.
def _invalid_argument(message):
    return _g_err(400, message, "INVALID_ARGUMENT")

# _permission_denied returns the standard Google 403 envelope.
def _permission_denied(message):
    return _g_err(403, message, "PERMISSION_DENIED")

# _not_found_err returns the standard Google 404 envelope.
def _not_found_err(message):
    return _g_err(404, message, "NOT_FOUND")

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _to_int parses a decimal string to int.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _num coerces a JSON-round-tripped number (int or float) to int.
def _num(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(v)

# _query_get reads a string query param from req, returning default when the
# param is absent or None (handles missing "query" dict gracefully).
def _query_get(req, key, default=""):
    q = req.get("query")
    if q == None:
        return default
    v = q.get(key, default)
    if v == None:
        return default
    return v

# _body_of returns the request body as a dict. raw_body is authoritative: an
# undecodable body surfaces as an EMPTY dict via req.body, so the raw bytes
# are decoded with json_safe_decode first. Returns None when raw bytes are
# present but not a JSON object (callers answer 400).
def _body_of(req):
    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""
    if raw != "":
        decoded = json_safe_decode(raw)
        if decoded == None or type(decoded) != "dict":
            return None
        return decoded
    b = req.get("body")
    if b == None:
        return {}
    if type(b) != "dict":
        return None
    return b

# _list_page slices docs by the Search Console API's maxResults/pageToken
# query params via the builtin paginate(), returning (page, next_page_token).
# next_page_token is None when no items remain. maxResults <= 0 / absent
# disables paging (returns all, next None).
def _list_page(req, docs):
    max_results = _to_int(_query_get(req, "maxResults", ""))
    page_token = _query_get(req, "pageToken", "")
    page, next_token = paginate(docs, max_results, page_token)
    return page, next_token

# ============================================================================
# Site lifecycle (derive-on-read verification)
# ============================================================================
# Real Search Console sites.add registers a property that stays UNVERIFIED
# (permissionLevel "siteUnverifiedUser") until ownership is proven. This
# simulator derives the verification transition from the clock, like the
# twilio/whatsapp async exemplars: a site added via PUT is unverified for
# VERIFY_SECONDS, then reads observe and persist the verified state
# (permissionLevel "siteFullUser"). Analytics/inspection of an unverified or
# unknown property is rejected with 403 PERMISSION_DENIED, like the real API.
_VERIFY_SECONDS = 2

# _seed populates default sites (verified) and one sitemap each.
def _seed():
    if store_kv_get("gsc", "seeded") == "yes":
        return
    store_kv_set("gsc", "seeded", "yes")

    sc = store_collection("sites")
    sc.insert(_site_doc("sc-domain:example.com", "siteOwner", 0))
    sc.insert(_site_doc("https://www.example.com/", "siteFullUser", 0))
    sc.insert(_site_doc("https://blog.example.com/", "siteRestrictedUser", 0))

    smc = store_collection("sitemaps")
    for entry in _seed_sitemaps():
        smc.insert(entry)

# _site_doc builds a site record. verify_at 0 means "already verified";
# otherwise it is the unix time at which verification completes.
def _site_doc(site_url, permission_level, verify_at):
    return {
        "id": site_url,
        "siteUrl": site_url,
        "permissionLevel": permission_level,
        "_verify_at": verify_at,
    }

# _seed_sitemaps returns the seeded sitemap entries (one per property).
def _seed_sitemaps():
    now = clock.now_rfc3339()
    return [
        _sitemap_doc("sc-domain:example.com", "sitemap.xml", now),
        _sitemap_doc("https://www.example.com/", "sitemap.xml", now),
    ]

# _sitemap_doc builds a sitemap record for a site + relative feedpath.
def _sitemap_doc(site_url, feedpath, last_submitted):
    return {
        "id": site_url + "|" + feedpath,
        "siteUrl": site_url,
        "path": _site_origin(site_url) + "/" + feedpath,
        "feedpath": feedpath,
        "lastSubmitted": last_submitted,
        "lastDownloaded": last_submitted,
        "isPending": False,
        "isSitemapsIndex": False,
        "type": "sitemap",
        "errors": "0",
        "warnings": "0",
        "contents": [{"type": "web", "submitted": "6", "indexed": "5"}],
    }

# _site_origin derives the URL origin a property covers: domain properties
# (sc-domain:example.com) map to https://example.com; URL-prefix properties
# use their own origin.
def _site_origin(site_url):
    if site_url[:10] == "sc-domain:":
        return "https://" + site_url[10:]
    origin = site_url
    for _i in range(3):
        if origin[len(origin) - 1:] == "/":
            origin = origin[:len(origin) - 1]
        else:
            break
    return origin

# _site_host returns just the host of the property origin.
def _site_host(site_url):
    origin = _site_origin(site_url)
    rest = origin
    if rest[:8] == "https://":
        rest = rest[8:]
    elif rest[:7] == "http://":
        rest = rest[7:]
    return rest

# _resolve_site looks a site up by its (decoded) path param and applies the
# derive-on-read verification transition, persisting it. Returns the site doc
# or None when the property is unknown.
def _resolve_site(site_url):
    if site_url == None or site_url == "":
        return None
    target = _normalize_site(site_url)
    sc = store_collection("sites")
    for doc in sc.list():
        if doc.get("id") == target:
            verify_at = _num(doc.get("_verify_at", 0))
            if verify_at != 0 and clock.now_unix() >= verify_at:
                doc["permissionLevel"] = "siteFullUser"
                doc["_verify_at"] = 0
                sc.update(doc["id"], doc)
            return doc
    return None

# _normalize_site canonicalizes a property identifier: lowercase the scheme
# and the sc-domain: prefix; URL-prefix properties keep their trailing slash
# (the real API addresses them exactly as registered).
def _normalize_site(site_url):
    if site_url[:10].lower() == "sc-domain:":
        return "sc-domain:" + site_url[10:]
    return site_url

# _site_view renders the public WmxSite shape (internal keys stripped).
def _site_view(doc):
    return {
        "siteUrl": doc.get("siteUrl", ""),
        "permissionLevel": doc.get("permissionLevel", "siteUnverifiedUser"),
    }

# _require_site resolves the property from the request and enforces real
# access rules: unknown property → 403; unverified property → 403. Returns
# (site_doc, None) or (None, error_response).
def _require_site(req):
    raw = req["params"].get("siteUrl", "")
    doc = _resolve_site(raw)
    if doc == None:
        return None, _permission_denied("You don't have access to the property '" + _normalize_site(raw) + "'.")
    if doc.get("permissionLevel", "") == "siteUnverifiedUser":
        return None, _permission_denied("The property '" + doc.get("siteUrl", "") + "' has not been verified in Search Console.")
    return doc, None

# _sitemap_view renders the public WmxSitemap shape (internal keys stripped).
def _sitemap_view(doc):
    return {
        "path": doc.get("path", ""),
        "lastSubmitted": doc.get("lastSubmitted", ""),
        "lastDownloaded": doc.get("lastDownloaded", ""),
        "isPending": doc.get("isPending", False),
        "isSitemapsIndex": doc.get("isSitemapsIndex", False),
        "type": doc.get("type", "sitemap"),
        "errors": doc.get("errors", "0"),
        "warnings": doc.get("warnings", "0"),
        "contents": doc.get("contents", []),
    }

# ============================================================================
# Deterministic synthetic search data
# ============================================================================

# _stable_hash maps a string to a stable non-negative int (polynomial rolling
# hash over runes; no randomness — same input, same metrics, every boot).
def _stable_hash(s):
    h = 7
    for i in range(len(s)):
        c = ord(s[i])
        if c >= 97 and c <= 122:
            c = c - 96      # a-z → 1..26
        elif c >= 65 and c <= 90:
            c = c - 64      # A-Z → 1..26
        elif c >= 48 and c <= 57:
            c = c - 47      # 0-9 → 1..10
        h = (h * 31 + c * 97) % 978 * 97
    return h % (2 * 52428 * 10 + 1)

# _day_ordinal converts "YYYY-MM-DD" to a proleptic day number (for range
# iteration and comparisons); 0 when malformed.
_CUM_DAYS = [0, 31, 59, 90, 120, 151, 181, 212, 243, 273, 304, 334]

def _is_leap(y):
    return (y % 4 == 0 and y % 100 != 0) or y % 400 == 0

def _day_ordinal(date_str):
    if date_str == None or len(date_str) != 10 or date_str[4] != "-" or date_str[7] != "-":
        return 0
    y = _to_int(date_str[0:4])
    m = _to_int(date_str[5:7])
    d = _to_int(date_str[8:10])
    if y <= 0 or m < 1 or m > 12 or d < 1 or d > 31:
        return 0
    days = (y - 1) * 365 + (y - 1) // 4 - (y - 1) // 100 + (y - 1) // 400 + _CUM_DAYS[m - 1] + d
    if m > 2 and _is_leap(y):
        days = days + 1
    return days

# _ordinal_day converts a day ordinal back to "YYYY-MM-DD".
def _ordinal_day(n):
    y = (n * 400) // (14609 * 10) + 1
    while _day_ordinal(_fmt_day(y + 1, 1, 1)) <= n:
        y = y + 1
    while n < _day_ordinal(_fmt_day(y, 1, 1)):
        y = y - 1
    rest = n - _day_ordinal(_fmt_day(y, 1, 1))
    m = 1
    while m <= 12:
        md = _month_days(y, m)
        if rest < md:
            break
        rest = rest - md
        m = m + 1
    if m > 12:
        m = 12
    return _fmt_day(y, m, rest + 1)

def _month_days(y, m):
    if m == 2 and _is_leap(y):
        return 29
    if m == 12:
        return 31
    return _CUM_DAYS[m] - _CUM_DAYS[m - 1]

def _fmt_day(y, m, d):
    mm = str(m)
    dd = str(d)
    if len(mm) < 2:
        mm = "0" + mm
    if len(dd) < 2:
        dd = "0" + dd
    return str(y) + "-" + mm + "-" + dd
