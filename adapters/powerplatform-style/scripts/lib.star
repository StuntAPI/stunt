# Shared library for powerplatform-style adapter scripts.

# _bearer extracts the token from "Authorization: Bearer <t>".
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _require_bearer returns None if OK, or a 401 response if missing.
def _require_bearer(req):
    if _bearer(req) == "":
        return respond(401, {
            "error": {
                "code": "Unauthorized",
                "message": "Authentication required. Provide a Bearer token.",
            },
        })
    return None

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
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

# _get_query safely returns a query parameter value.
def _get_query(req, key, default_val):
    query = req.get("query")
    if query == None:
        query = {}
    v = query.get(key, default_val)
    if v == None:
        v = default_val
    return v

# _list_page reads the Power Platform OData $top (page size) / $skipToken
# (opaque cursor) query params, slices the already-filtered docs via the
# paginate() builtin, and returns (page, next_link) where next_link is an
# @odata.nextLink URL string the client can follow to round-trip
# $top/$skipToken, or None when there is no further page. Paging is DISABLED
# (whole list returned, next_link None) when $top is missing or <= 0 —
# preserving prior unpaginated behavior. base_path is the route used to build
# the next-link URL.
def _list_page(req, docs, base_path):
    top = _to_int(_get_query(req, "$top", ""))
    skip_token = _get_query(req, "$skipToken", "")
    if skip_token == None:
        skip_token = ""

    page, next_cursor = paginate(docs, top, skip_token)

    next_link = None
    if next_cursor != None:
        next_link = base_path + "?$top=" + str(top) + "&$skipToken=" + next_cursor
    return page, next_link

# _seed populates default environments and accounts.
_ENVS = [
    {
        "name": "Default-d3a1d3a1-d3a1-d3a1-d3a1-d3a1d3a1d3a1",
        "id": "/providers/Microsoft.PowerPlatform/environments/Default-d3a1d3a1-d3a1-d3a1-d3a1-d3a1d3a1d3a1",
        "location": "unitedstates",
        "properties": {
            "displayName": "Production",
            "environmentSku": "Production",
            "azureRegion": "westus",
            "state": "Ready",
            "isDefault": True,
        },
    },
    {
        "name": "Dev-e4b2e4b2-e4b2-e4b2-e4b2-e4b2e4b2e4b2",
        "id": "/providers/Microsoft.PowerPlatform/environments/Dev-e4b2e4b2-e4b2-e4b2-e4b2-e4b2e4b2e4b2",
        "location": "europe",
        "properties": {
            "displayName": "Development",
            "environmentSku": "Sandbox",
            "azureRegion": "westeurope",
            "state": "Ready",
            "isDefault": False,
        },
    },
]

# Seed Dataverse accounts per environment.
_ACCOUNTS = [
    {
        "accountid": "aaa11111-0000-0000-0000-000000000001",
        "name": "Contoso Ltd.",
        "emailaddress1": "info@contoso.com",
        "telephone1": "+1-555-0100",
        "revenue": 5000000,
        "statecode": 0,
        "_primarycontactid_value": "bbb11111-0000-0000-0000-000000000001",
    },
    {
        "accountid": "aaa11111-0000-0000-0000-000000000002",
        "name": "Adventure Works",
        "emailaddress1": "contact@adventure-works.com",
        "telephone1": "+1-555-0200",
        "revenue": 2500000,
        "statecode": 0,
        "_primarycontactid_value": "bbb11111-0000-0000-0000-000000000002",
    },
]
