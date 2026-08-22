# Project handlers — Google Apps Script API.
#
# GET    /v1/projects → list projects
# POST   /v1/projects → create a project
# DELETE /v1/projects/{scriptId} → delete a project
# POST   /v1/projects/{scriptId}/deployments → create a deployment

def on_list_projects(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    pc = store_collection("projects")
    items = []
    for p in pc.list():
        items.append(_project_resource(p))

    page, next_token = _list_page(req, items)
    if page == None:
        return _g_err(400, "Invalid pageToken", "INVALID_ARGUMENT")
    result = {"projects": page}
    if next_token != None:
        result["nextPageToken"] = next_token
    return respond(200, result)

def on_create_project(req):
    err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    title = body.get("title", "Untitled project")
    if title == None:
        title = "Untitled project"
    parent_id = body.get("parentId", None)

    seq = store_kv_incr("apps-script", "project_seq") + 1
    script_id = _gen_script_id(seq)

    project = {
        "id": script_id,
        "scriptId": script_id,
        "title": title,
        "parentId": parent_id,
        "createTime": _now_ms(),
        "updateTime": _now_ms(),
        "content": {"files": []},
    }

    pc = store_collection("projects")
    pc.insert(project)

    return respond(200, _project_resource(project))

# GET /v1/projects/{scriptId} → a single project resource.
def on_get_project(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    project = _find_project(req["params"]["scriptId"])
    if project == None:
        return _g_err(404, "Project " + req["params"]["scriptId"] + " not found.", "NOT_FOUND")

    return respond(200, _project_resource(project))

# DELETE /v1/projects/{scriptId} → delete a project.
# The real Apps Script API returns an empty response body on success.
def on_delete_project(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    script_id = req["params"]["scriptId"]
    project = _find_project(script_id)
    if project == None:
        return _g_err(404, "Project " + script_id + " not found.", "NOT_FOUND")

    pc = store_collection("projects")
    pc.delete(project.get("id"))

    return respond(200, None)

def on_create_deployment(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    script_id = req["params"]["scriptId"]
    project = _find_project(script_id)
    if project == None:
        return _g_err(404, "Project " + script_id + " not found.", "NOT_FOUND")

    body = req["body"]
    if body == None:
        body = {}

    version_number = body.get("versionNumber", 1)
    if version_number == None:
        version_number = 1

    dep_id = "dep-" + str(store_kv_incr("apps-script", "deploy_seq") + 1)

    dep = {
        "deploymentId": dep_id,
        "scriptId": script_id,
        "deploymentConfig": body.get("deploymentConfig", {}),
        "version": {"versionNumber": version_number, "createTime": _now_ms()},
    }
    dcp = store_collection("deployments")
    dcp.insert(dep)

    return respond(200, dep)

# GET /v1/projects/{scriptId}/deployments → list a project's deployments.
def on_list_deployments(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    script_id = req["params"]["scriptId"]
    project = _find_project(script_id)
    if project == None:
        return _g_err(404, "Project " + script_id + " not found.", "NOT_FOUND")

    deps = []
    dcp = store_collection("deployments")
    for d in dcp.list():
        if d.get("scriptId") == script_id:
            deps.append(d)

    return respond(200, {"deployments": deps})

# _project_resource builds the API response shape for a project.
def _project_resource(p):
    return {
        "scriptId": p.get("scriptId", p.get("id", "")),
        "title": p.get("title", ""),
        "parentId": p.get("parentId", None),
        "createTime": p.get("createTime", ""),
        "updateTime": p.get("updateTime", ""),
    }
