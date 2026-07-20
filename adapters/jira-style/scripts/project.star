# Project handlers — list and get projects.
#
# GET /rest/api/3/project -> [{id, key, name, projectTypeKey, lead:{...}}]
# GET /rest/api/3/project/{key} -> {id, key, name, ...}

# Shared helpers from lib.star.

def on_list_projects(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("projects")
    docs = c.list()

    projects = []
    for d in docs:
        projects.append(_project_summary(d))

    return respond(200, projects)

def on_get_project(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    key = req["params"].get("key", "")
    c = store_collection("projects")

    for d in c.list():
        if d.get("key") == key:
            return respond(200, _project_detail(d))

    return _not_found()

# _project_summary builds the list-view project shape.
def _project_summary(d):
    return {
        "id": d.get("id", ""),
        "key": d.get("key", ""),
        "name": d.get("name", ""),
        "projectTypeKey": d.get("projectTypeKey", "software"),
        "lead": d.get("lead", {}),
        "self": "https://mock-jira.atlassian.net/rest/api/3/project/" + d.get("key", ""),
    }

# _project_detail builds the full project shape.
def _project_detail(d):
    return {
        "id": d.get("id", ""),
        "key": d.get("key", ""),
        "name": d.get("name", ""),
        "projectTypeKey": d.get("projectTypeKey", "software"),
        "lead": d.get("lead", {}),
        "styles": {},
        "projectCategory": {},
        "components": [],
        "issueTypes": [
            {"id": "10001", "name": "Story"},
            {"id": "10002", "name": "Task"},
            {"id": "10003", "name": "Bug"},
            {"id": "10004", "name": "Epic"},
        ],
        "versions": [],
        "self": "https://mock-jira.atlassian.net/rest/api/3/project/" + d.get("key", ""),
    }
