# Zone handlers for the Cloudflare API.
#
# GET    /zones                        -> list zones
# POST   /zones                        -> create zone
# GET    /zones/{zone_id}              -> get single zone
# DELETE /zones/{zone_id}              -> delete zone
# POST   /zones/{zone_id}/purge_cache  -> purge cache
#
# DNS records (zones/{zone_id}/dns_records) live in dns.star; firewall and
# page rules live in rules.star.
#
# Stateful: zones created via POST appear in the zones list.
#
# Shared helpers (_require_auth, _cf_ok, _cf_err, _gen_id, _find_zone,
# _ensure_seed_zones) are preloaded from scripts/lib.star.

# on_list_zones returns the zones list.
def on_list_zones(req):
    err = _require_auth(req)
    if err != None:
        return err

    _ensure_seed_zones()
    zc = store_collection("zones")
    zones = zc.list()

    # Derive the current status from the clock and persist transitions so
    # list, get, and the status filter all agree.
    result = []
    for z in zones:
        _advance_zone(z, zc)
        result.append(_zone_result(z))

    result = _apply_zone_filters(req, result)
    page, next_cursor = _list_page(req, result)
    if page == None:
        return _cf_err(400, 400, "Invalid cursor token")
    return _cf_ok_with_info(page, len(result), next_cursor)

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
    fail = body.get("simulate_fail", False)
    if fail == None:
        fail = False
    now = clock.now_unix()
    doc = {
        "zone_id": zone_id,
        "name": name,
        "status": "pending",
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
        "_running_at": now + 1,
        "_done_at": now + 3,
        "_fail": fail,
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

    _advance_zone(zone, zc)
    return _cf_ok(_zone_result(zone))

# on_delete_zone deletes a zone by ID.
# DELETE /zones/{zone_id}
# Real Cloudflare returns 200 with the deleted zone's id in the result.
def on_delete_zone(req):
    err = _require_auth(req)
    if err != None:
        return err

    zone_id = req["params"]["zone_id"]
    _ensure_seed_zones()
    zc = store_collection("zones")
    target = None
    for z in zc.list():
        if z.get("zone_id", "") == zone_id:
            target = z
            break
    if target == None:
        return _cf_err(404, 1003, "Zone not found.")

    zc.delete(target.get("id", ""))
    return _cf_ok({"id": zone_id})

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

# _apply_zone_filters maps the real Cloudflare List Zones query params to
# query_select clauses, applied before paging like the real API: name/status
# and account.id/account.name exact filters plus order/direction sorting
# (order: name, id, status, ...).
def _apply_zone_filters(req, zones):
    f = []
    name = _get_query(req, "name", "")
    if name != "":
        f.append(["name", "=", name])
    status = _get_query(req, "status", "")
    if status != "":
        f.append(["status", "=", status])
    account_id = _get_query(req, "account.id", "")
    if account_id != "":
        f.append(["account.id", "=", account_id])
    account_name = _get_query(req, "account.name", "")
    if account_name != "":
        f.append(["account.name", "=", account_name])
    order = _get_query(req, "order", "")
    direction = _get_query(req, "direction", "asc")
    if len(f) == 0 and order == "":
        return zones
    return query_select(zones, f if len(f) > 0 else None, order, direction, None, None, None)

# _derive_zone_status maps the clock onto Cloudflare's real zone status
# vocabulary (pending -> initializing -> active; "moved" when the zone has
# moved away). Zones stored before the lifecycle fields existed (the seeded
# zone) keep their stored status.
def _derive_zone_status(z):
    if z.get("_done_at", None) == None:
        return z.get("status", "active")
    now = clock.now_unix()
    if now < z.get("_running_at", 0):
        return "pending"
    if now < z["_done_at"]:
        return "initializing"
    if z.get("_fail", False):
        return "moved"
    return "active"

# _advance_zone derives the current status and persists the transition back
# to the zones collection so the list, get, and status-filter views agree.
def _advance_zone(z, zc):
    status = _derive_zone_status(z)
    if z.get("status", "") == status:
        return status
    z["status"] = status
    z["modified_on"] = _iso8601()
    zc.update(z.get("id", ""), z)
    return status

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
