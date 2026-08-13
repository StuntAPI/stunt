# Zones handler — CloudKit Web Services zones endpoint.
#
# GET .../zones/list → list of zones

# on_list_zones returns all zones in the database.
def on_list_zones(req):
    auth, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    zc = store_collection("zones")
    zones = []
    for z in zc.list():
        zones.append({
            "zoneName": z.get("zoneName", ""),
            "zoneType": z.get("zoneType", ""),
        })

    page, next_cursor = _list_page(req, zones)
    resp = {"zones": page}
    if next_cursor != None:
        resp["continuationMarker"] = next_cursor
    return respond(200, resp)
