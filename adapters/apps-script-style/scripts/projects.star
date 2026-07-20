# Project handlers — Google Apps Script API.
#
# GET  /v1/projects → list projects
# POST /v1/projects → create a project
# POST /v1/projects/{scriptId}/deployments → create a deployment

def on_list_projects(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    pc = store_collection("projects")
    items = []
    for p in pc.list():
        items.append(_project_resource(p))

    return respond(200, {"projects": items})

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
        "createTime": "2024-01-01T00:00:00.000Z",
        "updateTime": "2024-01-01T00:00:00.000Z",
        "content": {"files": []},
    }

    pc = store_collection("projects")
    pc.insert(project)

    return respond(200, _project_resource(project))

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

    return respond(200, {
        "deploymentId": dep_id,
        "scriptId": script_id,
        "deploymentConfig": body.get("deploymentConfig", {}),
        "version": {"versionNumber": version_number, "createTime": "2024-01-01T00:00:00.000Z"},
    })

# _project_resource builds the API response shape for a project.
def _project_resource(p):
    return {
        "scriptId": p.get("scriptId", p.get("id", "")),
        "title": p.get("title", ""),
        "parentId": p.get("parentId", None),
        "createTime": p.get("createTime", ""),
        "updateTime": p.get("updateTime", ""),
    }
