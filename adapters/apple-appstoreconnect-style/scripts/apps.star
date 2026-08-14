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

# _seed populates a default app (and its build) on first access.
def _seed():
    if store_kv_get("asc", "seeded") == "yes":
        return
    store_kv_set("asc", "seeded", "yes")
    c = store_collection("apps")
    doc = _app_doc(
        "com.example.mockapp",
        "Mock App",
        "MOCK_SKU_001",
        "en-US",
    )
    c.insert(doc)
    _create_build(doc["id"], False)

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

# --- build lifecycle (derive-on-read) ---

# _create_build stores a freshly uploaded build for an app with the
# derive-on-read timestamps: _running_at (create + 1s) and _done_at
# (create + 3s) computed from clock.now_unix(). fail is the simulator-only
# simulate_fail flag from the app-creation attributes.
def _create_build(app_id, fail):
    now = clock.now_unix()
    bc = store_collection("builds")
    bc.insert({
        "id": "bld_" + app_id + "_1",
        "app": app_id,
        "version": "1",
        "processingState": "PROCESSING",
        "uploadedDate": clock.now_rfc3339(),
        "_running_at": now + 1,
        "_done_at": now + 3,
        "_fail": fail,
    })

# _derive_build_state maps the clock onto Apple's real processingState
# vocabulary: PROCESSING while Apple processes the upload, then VALID (or
# INVALID for the simulate_fail path).
def _derive_build_state(b):
    if b.get("_done_at", None) == None:
        return b.get("processingState", "VALID")
    now = clock.now_unix()
    if now < b["_done_at"]:
        return "PROCESSING"
    if b.get("_fail", False):
        return "INVALID"
    return "VALID"

# _advance_build derives the current processingState and persists the
# transition back to the builds collection so polls, lists, and the
# processingState filter agree. App Store Connect has no build webhooks, so
# no events are emitted.
def _advance_build(b, bc):
    state = _derive_build_state(b)
    if b.get("processingState", "") == state:
        return state
    b["processingState"] = state
    bc.update(b.get("id", ""), b)
    return state

# _build_entity builds a JSON:API resource object from a stored build doc
# (the internal underscore-prefixed lifecycle fields never appear).
def _build_entity(doc):
    return {
        "id": doc["id"],
        "type": "builds",
        "attributes": {
            "version": doc.get("version", "1"),
            "uploadedDate": doc.get("uploadedDate", ""),
            "processingState": doc.get("processingState", "PROCESSING"),
            "usesNonExemptEncryption": False,
        },
        "relationships": {
            "app": {
                "data": {"type": "apps", "id": doc.get("app", "")},
            },
        },
    }

# --- handlers ---

# on_list_apps handles GET /v1/apps.
def on_list_apps(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    _seed()
    c = store_collection("apps")
    docs = c.list()
    data = []
    for d in docs:
        data.append(_app_entity(d))

    # Real Find Apps params (filter[name]/filter[bundleId]/filter[sku],
    # sort, fields[apps]) filter before paging.
    data = _apply_apps_query(req, data)

    page, next_cursor, limit = _list_page(req, data)

    return respond(200, {
        "data": page,
        "links": _page_links("/v1/apps", next_cursor),
        "meta": _page_meta(len(data), limit, next_cursor),
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

    fail = attrs.get("simulate_fail", False)
    if fail == None:
        fail = False

    doc = _app_doc(bundle_id, name, sku, locale)
    c = store_collection("apps")
    c.insert(doc)

    # A freshly uploaded build starts PROCESSING (derive-on-read lifecycle,
    # see on_list_builds).
    _create_build(doc["id"], fail)

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

    data = [
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
    ]

    # Real list params (filter[appStoreState], filter[versionString], sort).
    data = _apply_version_query(req, data)

    return respond(200, {
        "data": data,
        "links": {
            "self": "/v1/apps/" + app_id + "/appStoreVersions",
        },
    })

# on_list_builds handles GET /v1/apps/{id}/builds.
# Build processing is a derive-on-read state machine using Apple's real
# processingState vocabulary: PROCESSING -> VALID (or INVALID with the
# simulator-only simulate_fail flag set at app creation; Apple's real sandbox
# has no failure trigger). Reads derive the state from the clock and persist
# the transition back so polls and lists agree.
def on_list_builds(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    app_id = req["params"]["id"]
    doc = _find_app(app_id)
    if doc == None:
        return _not_found_err("App", app_id)

    bc = store_collection("builds")
    data = []
    for b in bc.list():
        if b.get("app", "") != app_id:
            continue
        _advance_build(b, bc)
        data.append(_build_entity(b))

    # Real list params (filter[processingState], filter[version], sort).
    data = _apply_build_query(req, data)

    return respond(200, {
        "data": data,
        "included": None,
        "links": {
            "self": "/v1/apps/" + app_id + "/builds",
        },
    })

# on_get_build handles GET /v1/builds/{id} (Read Build Information).
def on_get_build(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    build_id = req["params"]["id"]
    bc = store_collection("builds")
    doc = bc.get(build_id)
    if doc == None:
        return _not_found_err("Build", build_id)

    _advance_build(doc, bc)
    return respond(200, {
        "data": _build_entity(doc),
        "links": {
            "self": "/v1/builds/" + build_id,
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

# --- list query helpers ---

# _apply_apps_query maps the real Find Apps query params to query_select
# clauses over JSON:API app entities (dotted attribute paths), then applies
# the fields[apps] projection. Applied before paging like the real API.
def _apply_apps_query(req, data):
    f = []

    v = _get_query(req, "filter[name]")
    if v != "":
        f.append(["attributes.name", "=", v])
    v = _get_query(req, "filter[bundleId]")
    if v != "":
        f.append(["attributes.bundleId", "=", v])
    v = _get_query(req, "filter[sku]")
    if v != "":
        f.append(["attributes.sku", "=", v])

    sort_field, desc = _asc_sort(req)
    order_by = None
    if sort_field == "name" or sort_field == "bundleId" or sort_field == "sku":
        order_by = "attributes." + sort_field
        order_dir = "desc" if desc else "asc"
    else:
        order_dir = ""

    data = query_select(data, f if len(f) > 0 else None, order_by, order_dir, None, None, None)

    return _project_jsonapi_fields(data, _get_query(req, "fields[apps]"))

# _apply_version_query maps the real appStoreVersions list params
# (filter[appStoreState], filter[versionString], sort) over the entities.
def _apply_version_query(req, data):
    f = []

    v = _get_query(req, "filter[appStoreState]")
    if v != "":
        f.append(["attributes.appStoreState", "=", v])
    v = _get_query(req, "filter[versionString]")
    if v != "":
        f.append(["attributes.versionString", "=", v])

    sort_field, desc = _asc_sort(req)
    order_by = None
    if sort_field == "versionString" or sort_field == "appStoreState":
        order_by = "attributes." + sort_field
        order_dir = "desc" if desc else "asc"
    else:
        order_dir = ""

    return query_select(data, f if len(f) > 0 else None, order_by, order_dir, None, None, None)

# _apply_build_query maps the real builds list params
# (filter[processingState], filter[version], sort) over the entities.
def _apply_build_query(req, data):
    f = []

    v = _get_query(req, "filter[processingState]")
    if v != "":
        f.append(["attributes.processingState", "=", v])
    v = _get_query(req, "filter[version]")
    if v != "":
        f.append(["attributes.version", "=", v])

    sort_field, desc = _asc_sort(req)
    order_by = None
    if sort_field == "version" or sort_field == "uploadedDate" or sort_field == "processingState":
        order_by = "attributes." + sort_field
        order_dir = "desc" if desc else "asc"
    else:
        order_dir = ""

    return query_select(data, f if len(f) > 0 else None, order_by, order_dir, None, None, None)
