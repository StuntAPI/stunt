# Content handlers — Google Apps Script API.
#
# GET  /v1/projects/{scriptId}/content → get script content (files with source)
# POST /v1/projects/{scriptId}/content → update script content

def on_get_content(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    script_id = req["params"]["scriptId"]
    project = _find_project(script_id)
    if project == None:
        return _g_err(404, "Project " + script_id + " not found.", "NOT_FOUND")

    content = project.get("content", {})
    if content == None:
        content = {}

    return respond(200, {
        "scriptId": script_id,
        "files": content.get("files", []),
    })

def on_update_content(req):
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

    files = body.get("files", [])
    if files == None:
        files = []

    project["content"] = {"files": files}
    project["updateTime"] = "2024-01-02T00:00:00.000Z"

    pc = store_collection("projects")
    pc.update(project.get("id"), project)

    return respond(200, {
        "scriptId": script_id,
        "files": files,
    })
