# Collection handler.
#
# GET /api/v2/collections/{slug}
#   → { slug, name, description, image_url, primary_asset_contracts, stats, ... }

# Shared helpers (_require_xapikey, _seed) are preloaded from scripts/lib.star.

def on_get_collection(req):
    auth_err = _require_xapikey(req)
    if auth_err != None:
        return auth_err

    _seed()

    slug = req["params"].get("slug", "")

    cc = store_collection("collections")
    for coll in cc.list():
        if coll.get("slug", "") == slug:
            return respond(200, {
                "slug": coll.get("slug", ""),
                "name": coll.get("name", ""),
                "description": coll.get("description", ""),
                "image_url": coll.get("image_url", ""),
                "banner_image_url": coll.get("banner_image_url", ""),
                "external_url": coll.get("external_url", ""),
                "twitter_username": coll.get("twitter_username", ""),
                "discord_url": coll.get("discord_url", ""),
                "primary_asset_contracts": coll.get("primary_asset_contracts", []),
                "stats": coll.get("stats", {}),
            })

    return respond(404, {"error": "Collection not found"})
