# Issue handlers — CRUD, transitions, comments.
#
# POST   /rest/api/3/issue -> {id, key, self}
# GET    /rest/api/3/issue/{key} -> issue object
# PUT    /rest/api/3/issue/{key} -> 204 (update)
# GET    /rest/api/3/issue/{key}/transitions -> {transitions:[...]}
# POST   /rest/api/3/issue/{key}/transitions -> 204 (do transition)
# POST   /rest/api/3/issue/{key}/comment -> {id, ...}

# Shared helpers from lib.star.

def on_create_issue(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    body = _get_body(req)
    fields = body.get("fields", {})
    if fields == None:
        fields = {}

    project = fields.get("project", {})
    project_key = project.get("key", "")
    if project_key == "":
        return _jira_error(400, "project is required", {"project": "Project key is required"})

    summary = fields.get("summary", "")
    if summary == "":
        return _jira_error(400, "summary is required", {"summary": "Summary is required"})

    issuetype = fields.get("issuetype", {})
    issue_type_name = "Task"
    if issuetype != None:
        issue_type_name = issuetype.get("name", "Task")
    if issue_type_name == "":
        issue_type_name = "Task"

    # Generate issue number within this project.
    issue_num = store_kv_incr("jira", "issue_num_" + project_key)
    issue_key = project_key + "-" + str(issue_num)
    issue_id = _next_issue_id()

    full_fields = {
        "summary": summary,
        "status": {"name": "To Do", "id": "11"},
        "issuetype": {"name": issue_type_name, "id": "10002"},
        "project": {"key": project_key, "id": "10000"},
        "assignee": fields.get("assignee", None),
        "reporter": {"accountId": "5f1b3a4c5d6e7f8a9b0c1d2e", "displayName": "Alex Chen"},
        "created": _now(),
        "updated": _now(),
    }

    doc = {
        "id": issue_id,
        "key": issue_key,
        "fields": full_fields,
    }

    c = store_collection("issues")
    c.insert(doc)

    return respond(201, {
        "id": issue_id,
        "key": issue_key,
        "self": "https://mock-jira.atlassian.net/rest/api/3/issue/" + issue_id,
    })

def on_get_issue(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    key = req["params"].get("key", "")
    doc = _find_issue(key)
    if doc == None:
        return _not_found()

    return respond(200, {
        "id": doc.get("id", ""),
        "key": doc.get("key", ""),
        "self": "https://mock-jira.atlassian.net/rest/api/3/issue/" + doc.get("id", ""),
        "fields": doc.get("fields", {}),
    })

def on_update_issue(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    key = req["params"].get("key", "")
    doc = _find_issue(key)
    if doc == None:
        return _not_found()

    body = _get_body(req)
    fields = body.get("fields", {})
    if fields == None:
        fields = {}

    # Merge fields.
    existing_fields = doc.get("fields", {})
    merged_fields = {}
    for k, v in existing_fields.items():
        merged_fields[k] = v
    for k, v in fields.items():
        merged_fields[k] = v
    merged_fields["updated"] = _now()

    merged_doc = {
        "id": doc.get("id", ""),
        "key": doc.get("key", ""),
        "fields": merged_fields,
    }

    c = store_collection("issues")
    c.update(doc.get("id", ""), merged_doc)

    return respond(204)

def on_list_transitions(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    key = req["params"].get("key", "")
    doc = _find_issue(key)
    if doc == None:
        return _not_found()

    return respond(200, {
        "transitions": _TRANSITIONS,
    })

def on_do_transition(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    key = req["params"].get("key", "")
    doc = _find_issue(key)
    if doc == None:
        return _not_found()

    body = _get_body(req)
    transition = body.get("transition", {})
    if transition == None:
        transition = {}
    trans_id = transition.get("id", "")

    status_name = _transition_name_by_id(trans_id)
    if status_name == "":
        return _jira_error(400, "Invalid transition ID", {"transition": "Invalid transition ID"})

    # Update the issue status.
    existing_fields = doc.get("fields", {})
    existing_fields["status"] = {"name": status_name, "id": trans_id}
    existing_fields["updated"] = _now()

    merged_doc = {
        "id": doc.get("id", ""),
        "key": doc.get("key", ""),
        "fields": existing_fields,
    }

    c = store_collection("issues")
    c.update(doc.get("id", ""), merged_doc)

    return respond(204)

def on_add_comment(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    key = req["params"].get("key", "")
    doc = _find_issue(key)
    if doc == None:
        return _not_found()

    body = _get_body(req)
    comment_text = body.get("body", "")
    if comment_text == "":
        return _jira_error(400, "Comment body is required", {"body": "Comment body is required"})

    comment_id = _next_comment_id()
    comment_doc = {
        "id": comment_id,
        "body": comment_text,
        "author": {
            "accountId": "5f1b3a4c5d6e7f8a9b0c1d2e",
            "displayName": "Alex Chen",
        },
        "created": _now(),
        "updated": _now(),
        "self": "https://mock-jira.atlassian.net/rest/api/3/issue/" + doc.get("key", "") + "/comment/" + comment_id,
    }

    cc = store_collection("comments")
    cc.insert(comment_doc)

    return respond(201, comment_doc)

# _find_issue finds an issue by key in the issues collection. Returns the doc
# or None.
def _find_issue(key):
    c = store_collection("issues")
    for d in c.list():
        if d.get("key") == key:
            return d
    return None
