# Zone handlers for the Cloudflare API.
#
# GET    /zones                        -> list zones
# POST   /zones                        -> create zone
# GET    /zones/{zone_id}              -> get single zone
# GET    /zones/{zone_id}/dns_records  -> list DNS records
# GET    /zones/{zone_id}/firewall/rules -> list firewall rules
# GET    /zones/{zone_id}/page_rules   -> list page rules
# POST   /zones/{zone_id}/purge_cache  -> purge cache
#
# Stateful: zones created via POST appear in the zones list.
#
# Shared helpers (_require_auth, _cf_ok, _cf_err, _gen_id) are preloaded
# from scripts/lib.star.

# on_list_zones returns the zones list.
def on_list_zones(req):
    err = _require_auth(req)
    if err != None:
        return err

    _ensure_seed_zones()
    zc = store_collection("zones")
    zones = zc.list()

    # Extract clean zone objects (strip internal id field)
    result = []
    for z in zones:
        result.append(_zone_result(z))

    return _cf_ok_with_info(result, len(result))

# on_create_zone creates a new zone.
def on_create_zone(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        return _cf_err(400, 1003, "Invalid or missing zone.")

    name = body.get("name", "")
    if name == None:
        name = ""
    if name == "":
        return _cf_err(400, 1003, "Invalid or missing zone.")

    # Check for duplicates
    zc = store_collection("zones")
    for z in zc.list():
        if z.get("name", "") == name:
            return _cf_err(400, 1061, "Zone already exists.")

    zone_id = _gen_id("zone")
    doc = {
        "zone_id": zone_id,
        "name": name,
        "status": "active",
        "account": {
            "id": _default_account_id(),
            "name": "stunt-account",
        },
        "name_servers": [
            name + ".ns1.stunt.dev",
            name + ".ns2.stunt.dev",
        ],
        "type": "full",
        "created_on": _iso8601(),
        "modified_on": _iso8601(),
    }
    zc.insert(doc)

    return _cf_ok(_zone_result(doc))

# on_get_zone returns a single zone by ID.
def on_get_zone(req):
    err = _require_auth(req)
    if err != None:
        return err

    zone_id = req["params"]["zone_id"]
    _ensure_seed_zones()
    zc = store_collection("zones")
    zone = None
    for z in zc.list():
        if z.get("zone_id", "") == zone_id:
            zone = z
            break
    if zone == None:
        return _cf_err(404, 1003, "Zone not found.")

    return _cf_ok(_zone_result(zone))

# on_list_dns_records returns synthetic DNS records for a zone.
def on_list_dns_records(req):
    err = _require_auth(req)
    if err != None:
        return err

    zone_id = req["params"]["zone_id"]
    _ensure_seed_zones()
    zc = store_collection("zones")
    zone = None
    for z in zc.list():
        if z.get("zone_id", "") == zone_id:
            zone = z
            break
    if zone == None:
        return _cf_err(404, 1003, "Zone not found.")

    zone_name = zone.get("name", "example.com")
    records = [
        {
            "id": _gen_id("dns"),
            "zone_id": zone_id,
            "zone_name": zone_name,
            "name": zone_name,
            "type": "A",
            "content": "192.0.2.1",
            "proxied": True,
            "ttl": 1,
            "created_on": _iso8601(),
            "modified_on": _iso8601(),
        },
        {
            "id": _gen_id("dns"),
            "zone_id": zone_id,
            "zone_name": zone_name,
            "name": "www." + zone_name,
            "type": "CNAME",
            "content": zone_name,
            "proxied": True,
            "ttl": 1,
            "created_on": _iso8601(),
            "modified_on": _iso8601(),
        },
        {
            "id": _gen_id("dns"),
            "zone_id": zone_id,
            "zone_name": zone_name,
            "name": zone_name,
            "type": "MX",
            "content": "mail.stunt.dev",
            "priority": 10,
            "proxied": False,
            "ttl": 3600,
            "created_on": _iso8601(),
            "modified_on": _iso8601(),
        },
    ]
    return _cf_ok_with_info(records, len(records))

# on_list_firewall_rules returns synthetic firewall rules.
def on_list_firewall_rules(req):
    err = _require_auth(req)
    if err != None:
        return err

    zone_id = req["params"]["zone_id"]
    rules = [
        {
            "id": _gen_id("fw"),
            "zone_id": zone_id,
            "description": "Block known bad IPs",
            "action": "block",
            "filter": {
                "id": _gen_id("filter"),
                "expression": "(ip.src eq 192.0.2.0/24)",
            },
            "created_on": _iso8601(),
            "modified_on": _iso8601(),
        },
    ]
    return _cf_ok_with_info(rules, len(rules))

# on_list_page_rules returns synthetic page rules.
def on_list_page_rules(req):
    err = _require_auth(req)
    if err != None:
        return err

    zone_id = req["params"]["zone_id"]
    rules = [
        {
            "id": _gen_id("pr"),
            "zone_id": zone_id,
            "targets": [{"target": "url", "constraint": {"operator": "matches", "value": "stunt.dev/*"}}],
            "actions": [{"id": "browser_cache_ttl", "value": 300}],
            "priority": 1,
            "status": "active",
            "created_on": _iso8601(),
            "modified_on": _iso8601(),
        },
    ]
    return _cf_ok_with_info(rules, len(rules))

# on_purge_cache purges the cache for a zone.
def on_purge_cache(req):
    err = _require_auth(req)
    if err != None:
        return err

    zone_id = req["params"]["zone_id"]
    _ensure_seed_zones()
    zc = store_collection("zones")
    zone_found = False
    for z in zc.list():
        if z.get("zone_id", "") == zone_id:
            zone_found = True
            break
    if not zone_found:
        return _cf_err(404, 1003, "Zone not found.")

    body = req.get("body")
    purged = "everything"
    if body != None:
        files = body.get("files", None)
        if files != None:
            purged = "files"

    return _cf_ok({"id": zone_id, "purged": purged})

# ====================================================================
# Helpers
# ====================================================================

# _default_account_id returns a fixed synthetic account ID.
def _default_account_id():
    return "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"

# _zone_result returns a clean zone object for the API response.
def _zone_result(z):
    return {
        "id": z.get("zone_id", ""),
        "name": z.get("name", ""),
        "status": z.get("status", "active"),
        "account": z.get("account", {"id": _default_account_id(), "name": "stunt-account"}),
        "name_servers": z.get("name_servers", []),
        "type": z.get("type", "full"),
        "created_on": z.get("created_on", _iso8601()),
        "modified_on": z.get("modified_on", _iso8601()),
    }

# _ensure_seed_zones seeds the zones collection with a default zone if empty.
def _ensure_seed_zones():
    zc = store_collection("zones")
    if len(zc.list()) > 0:
        return
    seeded = store_kv_get("cf", "zones_seeded")
    if seeded == "1":
        return
    zc.insert({
        "zone_id": "023e105f4ecef8ad9ca31a8372d0c353",
        "name": "stunt.dev",
        "status": "active",
        "account": {
            "id": _default_account_id(),
            "name": "stunt-account",
        },
        "name_servers": ["stunt.dev.ns1.stunt.dev", "stunt.dev.ns2.stunt.dev"],
        "type": "full",
        "created_on": _iso8601(),
        "modified_on": _iso8601(),
    })
    store_kv_set("cf", "zones_seeded", "1")
