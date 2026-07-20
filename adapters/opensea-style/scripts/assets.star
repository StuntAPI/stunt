# Asset handlers — list and get assets.
#
# GET /api/v2/assets?collection_slug=...&limit=...
#   → { assets: [{ id, token_address, token_id, image_url, name, collection: {...} }] }
# GET /api/v2/assets/{chain}/{address}/{identifier}
#   → { id, token_address, token_id, image_url, name, collection: {...} }

# Shared helpers (_require_xapikey, _seed) are preloaded from scripts/lib.star.

def on_list_assets(req):
    auth_err = _require_xapikey(req)
    if auth_err != None:
        return auth_err

    _seed()

    slug = req["query"].get("collection_slug", "")

    ac = store_collection("assets")
    all_assets = ac.list()

    result = []
    for asset in all_assets:
        if slug != None and slug != "":
            coll = asset.get("collection", {})
            asset_slug = coll.get("slug", "")
            if asset_slug != slug:
                continue
        result.append({
            "id": asset.get("id", ""),
            "token_address": asset.get("token_address", ""),
            "token_id": asset.get("token_id", ""),
            "image_url": asset.get("image_url", ""),
            "name": asset.get("name", ""),
            "description": asset.get("description", ""),
            "chain": asset.get("chain", "ethereum"),
            "collection": asset.get("collection", {}),
        })

    return respond(200, {"assets": result})

def on_get_asset(req):
    auth_err = _require_xapikey(req)
    if auth_err != None:
        return auth_err

    _seed()

    chain = req["params"].get("chain", "")
    address = req["params"].get("address", "")
    identifier = req["params"].get("identifier", "")

    ac = store_collection("assets")
    for asset in ac.list():
        if asset.get("token_address", "").lower() == address.lower() and asset.get("token_id", "") == identifier:
            return respond(200, {
                "id": asset.get("id", ""),
                "token_address": asset.get("token_address", ""),
                "token_id": asset.get("token_id", ""),
                "image_url": asset.get("image_url", ""),
                "name": asset.get("name", ""),
                "description": asset.get("description", ""),
                "chain": asset.get("chain", "ethereum"),
                "collection": asset.get("collection", {}),
            })

    return respond(404, {"error": "Asset not found"})
