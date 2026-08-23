# projects.locations handlers — location metadata and the CMEK config.
#
# Shared helpers (_require_bearer, _err, _query_get, _to_int, _LOCATIONS,
# _location_known, _location_entity) are preloaded from scripts/lib.star.

# on_list_locations returns the regions the service operates in.
# GET /v2/projects/{project}/locations
def on_list_locations(req):
    err = _require_bearer(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    docs = [_location_entity(project, loc) for loc in _LOCATIONS]

    page_size = _to_int(_query_get(req, "pageSize", ""))
    page_token = _query_get(req, "pageToken", "")
    page, next_token = paginate(docs, page_size, page_token)
    if page == None:
        return _invalid("Invalid pageToken")
    resp = {"locations": page}
    if next_token != None:
        resp["nextPageToken"] = next_token
    return respond(200, resp)

# on_get_location returns one location.
# GET /v2/projects/{project}/locations/{location}
def on_get_location(req):
    err = _require_bearer(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    location = req["params"].get("location", "")
    if not _location_known(location):
        return _missing("Location", "projects/" + project + "/locations/" + location)
    return respond(200, _location_entity(project, location))

# on_get_cmek returns the location's customer-managed-encryption-key config.
# The real service has no key configured by default. GET
# /v2/projects/{project}/locations/{location}/cmekConfig
def on_get_cmek(req):
    err = _require_bearer(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    location = req["params"].get("location", "")
    if not _location_known(location):
        return _missing("Location", "projects/" + project + "/locations/" + location)

    name = "projects/" + project + "/locations/" + location + "/cmekConfig"
    stored = store_kv_get("cloudtasks", "cmek:" + name)
    kms_key = stored if stored != None else ""
    return respond(200, {"name": name, "kmsKey": kms_key})

# on_update_cmek sets the location's CMEK key (creating the config on first
# use, like the real PATCH). PATCH
# /v2/projects/{project}/locations/{location}/cmekConfig
def on_update_cmek(req):
    err = _require_bearer(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    location = req["params"].get("location", "")
    if not _location_known(location):
        return _missing("Location", "projects/" + project + "/locations/" + location)

    name = "projects/" + project + "/locations/" + location + "/cmekConfig"
    body, jerr = _json_body(req)
    if jerr != None:
        return jerr
    if body.get("kmsKey", None) == None:
        return _invalid("CmekConfig.kmsKey is required.")
    if type(body.get("kmsKey")) != "string":
        return _invalid("Invalid value at 'cmek_config.kms_key'.")
    if body.get("name", "") != "" and body.get("name") != name:
        return _invalid("CmekConfig.name must be " + name)

    store_kv_set("cloudtasks", "cmek:" + name, body.get("kmsKey"))
    return respond(200, {"name": name, "kmsKey": body.get("kmsKey")})
