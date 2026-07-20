# Networks handler — Tenderly Simulation API.
#
# GET /api/v1/networks → [{id, name, hex_id}, ...]

# Shared helpers (_bearer, _require_auth, _err, _NETWORKS) are preloaded.

def on_list_networks(req):
    if not _require_auth(req):
        return respond(401, _err("unauthorized", "Missing or invalid API key"))

    return respond(200, _NETWORKS)
