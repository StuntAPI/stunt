# App Store Connect API — apps CRUD handlers (JSON:API style).
#
# GET   /v1/apps              → list apps
# GET   /v1/apps/{id}         → get a single app
# POST  /v1/apps              → create an app
# GET   /v1/apps/{id}/appStoreVersions → list versions for an app
# GET   /v1/apps/{id}/builds  → list builds for an app
# GET   /v1/apps/{id}/appPrices → list prices for an app
#
# All endpoints require a valid JWT bearer token (ES256, structural check).
# Responses follow JSON:API conventions: { data: ..., links: ..., meta: ... }.
# Errors: { errors: [ { status, code, title, detail } ] }.

# Shared helpers (_require_jwt, _ok, _ok_list, _err, _not_found_err,
# _to_int, _b64url_decode, _jose_header, _reverse) are preloaded from
# scripts/lib.star.

# _seed populates a default app on first access.
def _seed():
    if store_kv_get("asc", "seeded") == "yes":
        return
    store_kv_set("asc", "seeded", "yes")
    c = store_collection("apps")
    c.insert(_app_doc(
        "com.example.mockapp",
        "Mock App",
        "MOCK_SKU_001",
        "en-US",
    ))

# _app_doc builds a stored app document.
def _app_doc(bundle_id, name, sku, locale):
    seq = store_kv_incr("asc", "app_seq")
    app_id = "app_" + str(1500000000 + seq)
    return {
        "id": app_id,
        "name": name,
        "bundleId": bundle_id,
        "sku": sku,
        "primaryLocale": locale,
        "contentRightsDeclaration": "Does not contain third-party content",
    }

# _app_entity builds a JSON:API resource object from a stored app doc.
def _app_entity(doc):
    return {
        "id": doc["id"],
        "type": "apps",
        "attributes": {
            "name": doc["name"],
            "bundleId": doc["bundleId"],
            "sku": doc["sku"],
            "primaryLocale": doc["primaryLocale"],
            "contentRightsDeclaration": doc.get("contentRightsDeclaration", "Does not contain third-party content"),
        },
        "links": {
            "self": "/v1/apps/" + doc["id"],
        },
    }

# _find_app looks up an app by id. Returns the doc or None.
def _find_app(app_id):
    c = store_collection("apps")
    return c.get(app_id)

# --- handlers ---

# on_list_apps handles GET /v1/apps.
def on_list_apps(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    _seed()
    limit = _to_int(req["query"].get("limit", "50"))
    if limit == 0:
        limit = 50

    c = store_collection("apps")
    docs = c.list()
    data = []
    for d in docs:
        data.append(_app_entity(d))
    if len(data) > limit:
        data = data[:limit]

    return respond(200, {
        "data": data,
        "links": {
            "self": "/v1/apps",
        },
        "meta": {
            "paging": {
                "total": len(docs),
                "limit": limit,
            },
        },
    })

# on_get_app handles GET /v1/apps/{id}.
def on_get_app(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    app_id = req["params"]["id"]
    doc = _find_app(app_id)
    if doc == None:
        return _not_found_err("App", app_id)

    return respond(200, {
        "data": _app_entity(doc),
        "links": {
            "self": "/v1/apps/" + app_id,
        },
    })

# on_create_app handles POST /v1/apps.
def on_create_app(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    data = body.get("data", body)
    attrs = data.get("attributes", {})
    name = attrs.get("name", "")
    bundle_id = attrs.get("bundleId", "")
    sku = attrs.get("sku", "")
    locale = attrs.get("primaryLocale", "en-US")

    if name == "" or bundle_id == "":
        return _err(409, "ENTITY_ERROR.ATTRIBUTE.REQUIRED",
                     "An attribute is missing or invalid",
                     "The required attributes 'name' and 'bundleId' must be provided.")

    doc = _app_doc(bundle_id, name, sku, locale)
    c = store_collection("apps")
    c.insert(doc)

    return respond(201, {
        "data": _app_entity(doc),
        "links": {
            "self": "/v1/apps/" + doc["id"],
        },
    })

# on_list_app_versions handles GET /v1/apps/{id}/appStoreVersions.
def on_list_app_versions(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    app_id = req["params"]["id"]
    doc = _find_app(app_id)
    if doc == None:
        return _not_found_err("App", app_id)

    return respond(200, {
        "data": [
            {
                "id": "av_" + app_id,
                "type": "appStoreVersions",
                "attributes": {
                    "versionString": "1.0.0",
                    "appStoreState": "READY_FOR_SALE",
                    "releaseType": "AFTER_APPROVAL",
                    "usesIdfa": False,
                },
                "relationships": {
                    "app": {
                        "data": {"type": "apps", "id": app_id},
                    },
                },
            }
        ],
        "links": {
            "self": "/v1/apps/" + app_id + "/appStoreVersions",
        },
    })

# on_list_builds handles GET /v1/apps/{id}/builds.
def on_list_builds(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    app_id = req["params"]["id"]
    doc = _find_app(app_id)
    if doc == None:
        return _not_found_err("App", app_id)

    return respond(200, {
        "data": [
            {
                "id": "bld_" + app_id + "_1",
                "type": "builds",
                "attributes": {
                    "version": "1",
                    "uploadedDate": "2024-01-15T10:00:00Z",
                    "processingState": "VALID",
                    "usesNonExemptEncryption": False,
                },
                "relationships": {
                    "app": {
                        "data": {"type": "apps", "id": app_id},
                    },
                },
            }
        ],
        "included": None,
        "links": {
            "self": "/v1/apps/" + app_id + "/builds",
        },
    })

# on_list_app_prices handles GET /v1/apps/{id}/appPrices.
def on_list_app_prices(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    app_id = req["params"]["id"]
    doc = _find_app(app_id)
    if doc == None:
        return _not_found_err("App", app_id)

    return respond(200, {
        "data": [
            {
                "id": "price_" + app_id,
                "type": "appPrices",
                "attributes": {
                    "startDate": None,
                    "endDate": None,
                },
                "relationships": {
                    "app": {
                        "data": {"type": "apps", "id": app_id},
                    },
                    "priceTier": {
                        "data": {"type": "appPriceTiers", "id": "0"},
                    },
                },
            }
        ],
        "links": {
            "self": "/v1/apps/" + app_id + "/appPrices",
        },
    })
