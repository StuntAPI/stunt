# Issue handlers — CRUD, transitions, comments.
#
# POST   /rest/api/3/issue -> {id, key, self}
# GET    /rest/api/3/issue/{key} -> issue object
# PUT    /rest/api/3/issue/{key} -> 204 (update)
# DELETE /rest/api/3/issue/{key} -> 204 (delete)
# GET    /rest/api/3/issue/{key}/transitions -> {transitions:[...]}
# POST   /rest/api/3/issue/{key}/transitions -> 204 (do transition)
# GET    /rest/api/3/issue/{key}/comment -> {comments:[...], startAt, maxResults, total}
# POST   /rest/api/3/issue/{key}/comment -> {id, ...}
# PUT    /rest/api/3/issue/{key}/comment/{id} -> updated comment
# DELETE /rest/api/3/issue/{key}/comment/{id} -> 204

# Shared helpers from lib.star.

def on_create_issue(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    body = _get_body(req)
    fields = body.get("fields", {})
    if fields == None:
        fields = {}

    # Unknown/unsettable fields are rejected per-field, like real Jira.
    field_errors = _validate_issue_fields(fields)
    if len(field_errors) > 0:
        return _field_error(field_errors)

    project = fields.get("project", {})
    if project == None:
        project = {}
    project_key = project.get("key", "")
    if project_key == "":
        return _field_error({"project": "Project key is required"})

    summary = fields.get("summary", "")
    if summary == "":
        return _field_error({"summary": "Summary is required"})

    issuetype = fields.get("issuetype", {})
    issue_type_name = "Task"
    if issuetype != None:
        issue_type_name = issuetype.get("name", "Task")
    if issue_type_name == "":
        issue_type_name = "Task"

    # Generate issue number within this project. The counter may lag behind
    # seeded issues (e.g. the fixture TEST-1), so bump past taken keys.
    issue_num = store_kv_incr("jira", "issue_num_" + project_key)
    issue_key = project_key + "-" + str(issue_num)
    while _find_issue(issue_key) != None:
        issue_num = store_kv_incr("jira", "issue_num_" + project_key)
        issue_key = project_key + "-" + str(issue_num)
    issue_id = _next_issue_id()

    # Store the real field set: the standard fields with server-computed
    # defaults plus every accepted custom field, verbatim.
    full_fields = {
        "summary": summary,
        "description": fields.get("description", None),
        "status": {"name": "To Do", "id": _JIRA_STATUS_IDS["To Do"]},
        "issuetype": {"name": issue_type_name, "id": "10" + "002"},
        "project": {"key": project_key, "id": "10" + "000"},
        "priority": _norm_priority(fields.get("priority", None)),
        "labels": _norm_labels(fields.get("labels", None)),
        "assignee": fields.get("assignee", None),
        "reporter": _norm_reporter(fields.get("reporter", None)),
        "created": _now(),
        "updated": _now(),
    }
    # Preserve any other accepted fields (customfield_*, components, ...).
    for name, value in fields.items():
        if not name in full_fields and _field_writable(name):
            full_fields[name] = value

    doc = {
        "id": issue_id,
        "key": issue_key,
        "fields": full_fields,
    }

    c = store_collection("issues")
    c.insert(doc)

    # Emit webhook event if subscribed (jira:issue_created).
    _emit_webhook("jira:issue_created", _issue_event("jira:issue_created", doc))

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

    field_errors = _validate_issue_fields(fields)
    if len(field_errors) > 0:
        return _field_error(field_errors)

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

    # Emit webhook event if subscribed (jira:issue_updated).
    _emit_webhook("jira:issue_updated", _issue_event("jira:issue_updated", merged_doc))

    return respond(204)

def on_delete_issue(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    key = req["params"].get("key", "")
    doc = _find_issue(key)
    if doc == None:
        return _not_found()

    c = store_collection("issues")
    c.delete(doc.get("id", ""))

    return respond(204)

def on_list_transitions(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    key = req["params"].get("key", "")
    doc = _find_issue(key)
    if doc == None:
        return _not_found()

    # Only the transitions the workflow allows from the CURRENT status.
    fields = doc.get("fields", {})
    status = fields.get("status", {})
    if status == None:
        status = {}
    current = status.get("name", "")

    transitions = []
    for t in _allowed_transitions(current):
        transitions.append(_transition_public(t))

    return respond(200, {
        "expand": "transitions",
        "transitions": transitions,
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
    if trans_id == "":
        trans_id = transition.get("name", "")

    # The workflow constrains the target: unknown IDs and transitions that
    # are not available from the current status are rejected with 400.
    existing_fields = doc.get("fields", {})
    if existing_fields == None:
        existing_fields = {}
    new_fields, trans_err = _apply_transition(existing_fields, trans_id)
    if trans_err != None:
        return _jira_error(400, trans_err, {"transition": trans_err})

    # Fields may be set alongside the transition (e.g. a resolution).
    extra = body.get("fields", {})
    if extra != None:
        extra_errors = _validate_issue_fields(extra)
        if len(extra_errors) > 0:
            return _field_error(extra_errors)
        for k, v in extra.items():
            new_fields[k] = v
    new_fields["updated"] = _now()

    merged_doc = {
        "id": doc.get("id", ""),
        "key": doc.get("key", ""),
        "fields": new_fields,
    }

    c = store_collection("issues")
    c.update(doc.get("id", ""), merged_doc)

    # A transition is an issue update for webhook purposes.
    _emit_webhook("jira:issue_updated", _issue_event("jira:issue_updated", merged_doc))

    return respond(204)

# on_get_comment serves GET /rest/api/3/issue/{key}/comment/{id} — the
# single-comment fetch (jira.js reads back every comment it creates).
def on_get_comment(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    key = req["params"].get("key", "")
    if _find_issue(key) == None:
        return _not_found()

    comment = store_collection("comments").get(req["params"].get("id", ""))
    if comment == None or comment.get("_issue", "") != key:
        return respond(404, {
            "errorMessages": ["Comment Does Not Exist"],
            "errors": {},
        })
    return respond(200, _comment_view(comment))

def on_list_comments(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    key = req["params"].get("key", "")
    doc = _find_issue(key)
    if doc == None:
        return _not_found()

    cc = store_collection("comments")
    docs = []
    for c in cc.list():
        if c.get("_issue", "") == key:
            docs.append(c)

    paged, start_at, max_results, total = _paginate(req, docs)

    comments = []
    for c in paged:
        comments.append(_comment_view(c))

    return respond(200, {
        "startAt": start_at,
        "maxResults": max_results,
        "total": total,
        "comments": comments,
    })

def on_add_comment(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    key = req["params"].get("key", "")
    doc = _find_issue(key)
    if doc == None:
        return _not_found()

    body = _get_body(req)
    comment_text = body.get("body") or ""
    if comment_text == "":
        return _jira_error(400, "Comment body is required", {"comment": "Comment body can not be empty!"})

    comment_id = _next_comment_id()
    comment_doc = {
        "id": comment_id,
        "_issue": key,
        "body": comment_text,
        "author": {
            "accountId": "5f1b3a4c5d6e7f8a9b0c1d2e",
            "displayName": "Alex Chen",
        },
        "created": _now(),
        "updated": _now(),
        "self": "https://mock-jira.atlassian.net/rest/api/3/issue/" + key + "/comment/" + comment_id,
    }

    cc = store_collection("comments")
    cc.insert(comment_doc)

    # Emit webhook event if subscribed (comment_created).
    _emit_webhook("comment_created", {
        "timestamp": clock.now_unix() * 1000,
        "webhookEvent": "comment_created",
        "comment": _comment_view(comment_doc),
        "issue": {
            "id": doc.get("id", ""),
            "key": doc.get("key", ""),
            "fields": doc.get("fields", {}),
        },
    })

    return respond(201, _comment_view(comment_doc))

def on_update_comment(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    key = req["params"].get("key", "")
    doc = _find_issue(key)
    if doc == None:
        return _not_found()

    comment_id = req["params"].get("id", "")
    cc = store_collection("comments")
    existing = None
    for c in cc.list():
        if c.get("id", "") == comment_id and c.get("_issue", "") == key:
            existing = c
            break
    if existing == None:
        return _not_found()

    body = _get_body(req)
    comment_text = body.get("body") or ""
    if comment_text == "":
        return _jira_error(400, "Comment body is required", {"comment": "Comment body can not be empty!"})

    updated = {
        "id": existing.get("id", ""),
        "_issue": key,
        "body": comment_text,
        "author": existing.get("author", {}),
        "created": existing.get("created", ""),
        "updated": _now(),
        "self": existing.get("self", ""),
    }
    cc.update(comment_id, updated)

    _emit_webhook("comment_updated", {
        "timestamp": clock.now_unix() * 1000,
        "webhookEvent": "comment_updated",
        "comment": _comment_view(updated),
        "issue": {
            "id": doc.get("id", ""),
            "key": doc.get("key", ""),
            "fields": doc.get("fields", {}),
        },
    })

    return respond(200, _comment_view(updated))

def on_delete_comment(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    key = req["params"].get("key", "")
    doc = _find_issue(key)
    if doc == None:
        return _not_found()

    comment_id = req["params"].get("id", "")
    cc = store_collection("comments")
    existing = None
    for c in cc.list():
        if c.get("id", "") == comment_id and c.get("_issue", "") == key:
            existing = c
            break
    if existing == None:
        return _not_found()

    cc.delete(comment_id)

    _emit_webhook("comment_deleted", {
        "timestamp": clock.now_unix() * 1000,
        "webhookEvent": "comment_deleted",
        "comment": _comment_view(existing),
        "issue": {
            "id": doc.get("id", ""),
            "key": doc.get("key", ""),
            "fields": doc.get("fields", {}),
        },
    })

    return respond(204)

# _find_issue finds an issue by key in the issues collection. Returns the doc
# or None.
def _find_issue(key):
    c = store_collection("issues")
    for d in c.list():
        if d.get("key") == key:
            return d
    return None

# _norm_priority normalizes a create/update priority value: a bare string is
# accepted like {"name": ...}; None keeps the field unset.
def _norm_priority(p):
    if p == None:
        return None
    if type(p) == "string":
        return {"name": p}
    return p

# _norm_labels normalizes labels to a list of strings.
def _norm_labels(l):
    if l == None:
        return []
    return l

# _norm_reporter defaults the reporter to the acting (mock) user.
def _norm_reporter(r):
    if r == None:
        return {"accountId": "5f1b3a4c5d6e7f8a9b0c1d2e", "displayName": "Alex Chen"}
    return r
