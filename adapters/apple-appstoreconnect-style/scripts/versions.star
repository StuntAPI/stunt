# App Store Connect API — appStoreVersions lifecycle handlers (JSON:API).
#
# POST  /v1/apps/{id}/appStoreVersions      → create a version
#                                             (PREPARE_FOR_SUBMISSION)
# GET   /v1/apps/{id}/appStoreVersions      → list versions for an app
# GET   /v1/appStoreVersions/{id}           → get a version
# PATCH /v1/appStoreVersions/{id}           → modify versionString etc.
# POST  /v1/appStoreVersionSubmissions      → submit a version for review
# GET   /v1/appStoreVersions/{id}/builds    → builds for a version
#
# VERSION LIFECYCLE (derive-on-read): a version sits in
# PREPARE_FOR_SUBMISSION until it is submitted for review (the real action:
# POST /v1/appStoreVersionSubmissions). From the submission timestamp the
# clock drives Apple's real appStoreState vocabulary:
#
#   PREPARE_FOR_SUBMISSION
#     → (submit) WAITING_FOR_REVIEW → IN_REVIEW → READY_FOR_SALE
#                                       └────────→ REJECTED (simulate_fail)
#
# Every read derives the current state from the clock and persists the
# transition back to the versions collection, so polls, lists, and the
# filter[appStoreState] param agree. App Store Connect has no version
# webhooks, so no events are emitted.
#
# Shared helpers (_require_jwt, _err, _not_found_err, _find_app,
# _advance_build, _build_entity, _apply_build_query, _num, _get_query,
# _asc_sort) are preloaded from scripts/lib.star.

# _EDITABLE_STATES are the appStoreStates in which Apple lets you modify a
# version (or resubmit it after a rejection).
_EDITABLE_STATES = ["PREPARE_FOR_SUBMISSION", "REJECTED"]

# _version_entity builds a JSON:API resource object from a stored version doc
# (internal underscore-prefixed lifecycle fields never appear).
def _version_entity(doc):
    version_id = doc.get("id", "")
    return {
        "id": version_id,
        "type": "appStoreVersions",
        "attributes": {
            "versionString": doc.get("versionString", ""),
            "appStoreState": doc.get("appStoreState", "PREPARE_FOR_SUBMISSION"),
            "releaseType": doc.get("releaseType", "AFTER_APPROVAL"),
            "usesIdfa": doc.get("usesIdfa", False),
            "platform": doc.get("platform", "IOS"),
        },
        "relationships": {
            "app": {
                "data": {"type": "apps", "id": doc.get("app", "")},
            },
        },
        "links": {
            "self": "/v1/appStoreVersions/" + version_id,
        },
    }

# _derive_version_state maps the clock onto Apple's real appStoreState
# vocabulary. Unsubmitted versions are PREPARE_FOR_SUBMISSION; from the
# submission timestamp: WAITING_FOR_REVIEW (+1s) → IN_REVIEW (+3s) →
# READY_FOR_SALE, or REJECTED with the simulator-only simulate_fail flag.
def _derive_version_state(doc):
    submitted_at = doc.get("_submitted_at", None)
    if submitted_at == None:
        return "PREPARE_FOR_SUBMISSION"
    now = clock.now_unix()
    if now < _num(doc.get("_review_at", 0)):
        return "WAITING_FOR_REVIEW"
    if now < _num(doc.get("_decision_at", 0)):
        return "IN_REVIEW"
    if doc.get("_fail", False):
        return "REJECTED"
    return "READY_FOR_SALE"

# _advance_version derives the current appStoreState and persists the
# transition back to the versions collection so polls, lists, and filters
# agree. Returns the derived state.
def _advance_version(doc):
    state = _derive_version_state(doc)
    if doc.get("appStoreState", "") == state:
        return state
    doc["appStoreState"] = state
    vc = store_collection("versions")
    vc.update(doc.get("id", ""), doc)
    return state

# _find_version looks up a version by id. Returns the doc or None.
def _find_version(version_id):
    vc = store_collection("versions")
    return vc.get(version_id)

# _version_data extracts the JSON:API `data` object from a request body,
# tolerating a bare attributes dict.
def _version_data(req):
    body = req.get("body")
    if body == None:
        body = {}
    data = body.get("data", body)
    if data == None:
        data = {}
    return data

# on_create_version handles POST /v1/apps/{id}/appStoreVersions (Add a new
# App Store Version). New versions start in PREPARE_FOR_SUBMISSION.
def on_create_version(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    app_id = req["params"]["id"]
    if _find_app(app_id) == None:
        return _not_found_err("App", app_id)

    data = _version_data(req)
    attrs = data.get("attributes", {})
    if attrs == None:
        attrs = {}
    version_string = attrs.get("versionString", "")
    if version_string == None:
        version_string = ""

    if version_string == "":
        return _err(409, "ENTITY_ERROR.ATTRIBUTE.REQUIRED",
                    "An attribute is missing or invalid",
                    "The required attribute 'versionString' must be provided.")

    # Apple rejects a duplicate version of the same app.
    vc = store_collection("versions")
    for v in vc.list():
        if v.get("app", "") == app_id and v.get("versionString", "") == version_string:
            return _err(409, "ENTITY_ERROR.ATTRIBUTE.INVALID",
                        "An attribute is invalid",
                        "The app already has a version with versionString '" + version_string + "'.")

    fail = attrs.get("simulate_fail", False)
    if fail == None:
        fail = False

    seq = store_kv_incr("asc", "version_seq")
    version_id = "av_" + app_id + "_" + str(seq)
    doc = {
        "id": version_id,
        "app": app_id,
        "platform": attrs.get("platform", "IOS"),
        "versionString": version_string,
        "appStoreState": "PREPARE_FOR_SUBMISSION",
        "releaseType": attrs.get("releaseType", "AFTER_APPROVAL"),
        "usesIdfa": attrs.get("usesIdfa", False),
        "createdDate": clock.now_rfc3339(),
        "_fail": fail,
    }
    vc.insert(doc)

    # Attach the app's not-yet-released builds to this version, mirroring
    # Apple's versionString ↔ build mapping.
    bc = store_collection("builds")
    for b in bc.list():
        if b.get("app", "") == app_id and b.get("appStoreVersion", None) == None:
            b["appStoreVersion"] = version_id
            bc.update(b.get("id", ""), b)

    return respond(201, {
        "data": _version_entity(doc),
        "links": {
            "self": "/v1/appStoreVersions/" + version_id,
        },
    })

# on_list_app_versions handles GET /v1/apps/{id}/appStoreVersions (List App
# Store Versions for an App) from the versions collection, advancing each
# version's derive-on-read state first.
def on_list_app_versions(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    app_id = req["params"]["id"]
    if _find_app(app_id) == None:
        return _not_found_err("App", app_id)

    vc = store_collection("versions")
    data = []
    for v in vc.list():
        if v.get("app", "") != app_id:
            continue
        _advance_version(v)
        data.append(_version_entity(v))

    # Real list params (filter[appStoreState], filter[versionString], sort).
    data = _apply_version_query(req, data)

    return respond(200, {
        "data": data,
        "links": {
            "self": "/v1/apps/" + app_id + "/appStoreVersions",
        },
    })

# on_get_version handles GET /v1/appStoreVersions/{id} (Read App Store
# Version Information).
def on_get_version(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    version_id = req["params"]["id"]
    doc = _find_version(version_id)
    if doc == None:
        return _not_found_err("AppStoreVersion", version_id)

    _advance_version(doc)
    return respond(200, {
        "data": _version_entity(doc),
        "links": {
            "self": "/v1/appStoreVersions/" + version_id,
        },
    })

# on_update_version handles PATCH /v1/appStoreVersions/{id} (Modify an App
# Store Version). Apple only allows edits while the version is in an
# editable state (PREPARE_FOR_SUBMISSION, or REJECTED for a resubmission).
def on_update_version(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    version_id = req["params"]["id"]
    doc = _find_version(version_id)
    if doc == None:
        return _not_found_err("AppStoreVersion", version_id)

    state = _advance_version(doc)
    if state not in _EDITABLE_STATES:
        return _err(409, "OPERATION_NOT_ALLOWED",
                    "The operation is not allowed",
                    "The version can only be modified while in PREPARE_FOR_SUBMISSION or REJECTED (current state: " + state + ").")

    data = _version_data(req)
    attrs = data.get("attributes", {})
    if attrs == None:
        attrs = {}

    new_version_string = attrs.get("versionString", None)
    if new_version_string != None and new_version_string != "":
        # Reject a collision with another version of the same app.
        vc = store_collection("versions")
        for v in vc.list():
            other = v.get("id", "")
            if other != version_id and v.get("app", "") == doc.get("app", "") and v.get("versionString", "") == new_version_string:
                return _err(409, "ENTITY_ERROR.ATTRIBUTE.INVALID",
                            "An attribute is invalid",
                            "The app already has a version with versionString '" + new_version_string + "'.")
        doc["versionString"] = new_version_string

    release_type = attrs.get("releaseType", None)
    if release_type != None and release_type != "":
        doc["releaseType"] = release_type

    uses_idfa = attrs.get("usesIdfa", None)
    if uses_idfa != None:
        doc["usesIdfa"] = uses_idfa

    vc = store_collection("versions")
    vc.update(version_id, doc)

    return respond(200, {
        "data": _version_entity(doc),
        "links": {
            "self": "/v1/appStoreVersions/" + version_id,
        },
    })

# on_create_version_submission handles POST /v1/appStoreVersionSubmissions
# (Submit an App Store Version for Review) — the real action that moves a
# version out of PREPARE_FOR_SUBMISSION into WAITING_FOR_REVIEW.
def on_create_version_submission(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    data = _version_data(req)
    rel = data.get("relationships", {})
    if rel == None:
        rel = {}
    ver = rel.get("appStoreVersion", {})
    if ver == None:
        ver = {}
    ver_data = ver.get("data", {})
    if ver_data == None:
        ver_data = {}
    version_id = ver_data.get("id", "")
    if version_id == "":
        return _err(409, "ENTITY_ERROR.RELATIONSHIP.REQUIRED",
                    "A relationship is missing or invalid",
                    "The required relationship 'appStoreVersion' must be provided.")

    doc = _find_version(version_id)
    if doc == None:
        return _not_found_err("AppStoreVersion", version_id)

    state = _advance_version(doc)
    if state not in _EDITABLE_STATES:
        return _err(409, "OPERATION_NOT_ALLOWED",
                    "The operation is not allowed",
                    "The version has already been submitted for review (current state: " + state + ").")

    # Stamp the derive-on-read review schedule: WAITING_FOR_REVIEW from the
    # submission, IN_REVIEW at +1s, a decision at +3s (clock-derived, never
    # hardcoded).
    now = clock.now_unix()
    doc["_submitted_at"] = now
    doc["_review_at"] = now + 1
    doc["_decision_at"] = now + 3
    doc["appStoreState"] = "WAITING_FOR_REVIEW"
    vc = store_collection("versions")
    vc.update(version_id, doc)

    seq = store_kv_incr("asc", "submission_seq")
    submission_id = "avs_" + str(seq)
    sc = store_collection("versionSubmissions")
    sc.insert({
        "id": submission_id,
        "appStoreVersion": version_id,
        "createdDate": clock.now_rfc3339(),
    })

    return respond(201, {
        "data": {
            "id": submission_id,
            "type": "appStoreVersionSubmissions",
            "relationships": {
                "appStoreVersion": {
                    "data": {"type": "appStoreVersions", "id": version_id},
                },
            },
            "links": {
                "self": "/v1/appStoreVersionSubmissions/" + submission_id,
            },
        },
    })

# on_list_version_builds handles GET /v1/appStoreVersions/{id}/builds (List
# Builds for an App Store Version).
def on_list_version_builds(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    version_id = req["params"]["id"]
    if _find_version(version_id) == None:
        return _not_found_err("AppStoreVersion", version_id)

    bc = store_collection("builds")
    data = []
    for b in bc.list():
        if b.get("appStoreVersion", "") != version_id:
            continue
        _advance_build(b, bc)
        data.append(_build_entity(b))

    # Real list params (filter[processingState], filter[version], sort).
    data = _apply_build_query(req, data)

    return respond(200, {
        "data": data,
        "links": {
            "self": "/v1/appStoreVersions/" + version_id + "/builds",
        },
    })

# --- list query helpers ---

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
    order_dir = ""
    if sort_field == "versionString" or sort_field == "appStoreState":
        order_by = "attributes." + sort_field
        order_dir = "desc" if desc else "asc"

    return query_select(data, f if len(f) > 0 else None, order_by, order_dir, None, None, None)
