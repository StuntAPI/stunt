# GA4 Admin API handlers — account / property / dataStream hierarchy.
#
# The GA4 data model is confusing because of the three-level hierarchy:
#   Account → Property → DataStream
# Properties are referenced as "properties/123456789".
#
# All protected endpoints require a valid Bearer token.

# Shared helpers (_bearer, _require_bearer) are preloaded from scripts/lib.star.

# on_list_accounts returns all GA4 accounts.
# GET /v1admin/accounts (Bearer)
def on_list_accounts(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    ac = store_collection("accounts")
    docs = ac.list()
    if len(docs) == 0:
        _seed_hierarchy()
        ac = store_collection("accounts")
        docs = ac.list()

    accounts = []
    for d in docs:
        accounts.append({
            "name": "accounts/" + d["id"],
            "displayName": d["displayName"],
            "createTime": d["createTime"],
            "updateTime": d["updateTime"],
            "regionCode": d.get("regionCode", "US"),
        })

    return respond(200, {"accounts": accounts})

# on_list_properties returns all GA4 properties.
# GET /v1admin/properties (Bearer)
# Optional filter: ?filter=parent:accounts/123
def on_list_properties(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    pc = store_collection("properties")
    docs = pc.list()
    if len(docs) == 0:
        _seed_hierarchy()
        pc = store_collection("properties")
        docs = pc.list()

    filt = req["query"].get("filter", "")

    properties = []
    for d in docs:
        if filt != "" and d.get("parent", "") != filt:
            continue
        properties.append({
            "name": "properties/" + d["id"],
            "parent": d.get("parent", ""),
            "displayName": d["displayName"],
            "industryCategory": d.get("industryCategory", "TECHNOLOGY"),
            "timeZone": d.get("timeZone", "America/Los_Angeles"),
            "currencyCode": d.get("currencyCode", "USD"),
            "createTime": d.get("createTime", "2024-01-01T00:00:00Z"),
        })

    return respond(200, {"properties": properties})

# on_list_datastreams returns all data streams for a property.
# GET /v1admin/properties/{property}/dataStreams (Bearer)
def on_list_datastreams(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    prop = req["params"].get("property", "")
    # Normalize: the route param is the numeric property id; the stored
    # data references the full resource name "properties/<id>".
    if not _contains(prop, "properties/"):
        prop = "properties/" + prop

    dsc = store_collection("datastreams")
    docs = dsc.list()
    if len(docs) == 0:
        _seed_hierarchy()
        dsc = store_collection("datastreams")
        docs = dsc.list()

    streams = []
    for d in docs:
        if d.get("parent", "") != prop:
            continue
        streams.append({
            "name": d["parent"] + "/dataStreams/" + d["id"],
            "type": d.get("type", "WEB_DATA_STREAM"),
            "displayName": d.get("displayName", ""),
            "webStreamData": d.get("webStreamData", {}),
        })

    return respond(200, {"dataStreams": streams})

# --- seed hierarchy ---

def _seed_hierarchy():
    ac = store_collection("accounts")
    ac.insert({
        "id": "100001",
        "displayName": "Mock Analytics Account",
        "createTime": "2024-01-01T00:00:00Z",
        "updateTime": "2024-01-01T00:00:00Z",
        "regionCode": "US",
    })

    pc = store_collection("properties")
    pc.insert({
        "id": "123456789",
        "parent": "accounts/100001",
        "displayName": "Mock Web Property",
        "industryCategory": "TECHNOLOGY",
        "timeZone": "America/Los_Angeles",
        "currencyCode": "USD",
        "createTime": "2024-01-01T00:00:00Z",
    })

    dsc = store_collection("datastreams")
    dsc.insert({
        "id": "2001",
        "parent": "properties/123456789",
        "type": "WEB_DATA_STREAM",
        "displayName": "Mock Web Stream",
        "webStreamData": {
            "measurementId": "G-MOCKMEAS1",
            "defaultUri": "www.mock-example.com",
        },
    })
