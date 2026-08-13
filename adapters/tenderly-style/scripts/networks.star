# Networks handler — Tenderly Simulation API.
#
# GET /api/v1/networks → [{id, name, hex_id}, ...]

# Shared helpers (_bearer, _require_auth, _err, _NETWORKS, _list_page) are
# preloaded.

def on_list_networks(req):
    if not _require_auth(req):
        return respond(401, _err("unauthorized", "Missing or invalid API key"))

    page, next_page, paged = _list_page(req, _NETWORKS)
    if not paged:
        # Unpaginated: preserve the bare-array response shape.
        return respond(200, page)
    body = {"networks": page}
    if next_page != None:
        body["next_page"] = next_page
    return respond(200, body)
