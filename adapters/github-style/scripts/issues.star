# Issue handlers — stateful issues matching GitHub REST.
#
# GET    /repos/{owner}/{repo}/issues                      -> [{number, title, state, ...}]
# POST   /repos/{owner}/{repo}/issues                      -> {number, title, state, ...}  (201)
# GET    /repos/{owner}/{repo}/issues/{number}             -> {number, title, state, ...}
# PATCH  /repos/{owner}/{repo}/issues/{number}             -> {number, title, state, ...}  (422 on bad state)
# GET    /repos/{owner}/{repo}/issues/{number}/comments    -> [{id, body, user, ...}]
# POST   /repos/{owner}/{repo}/issues/{number}/comments    -> {id, body, user, ...}  (201)
# POST   /repos/{owner}/{repo}/issues/{number}/labels/{name}  -> [{name, color, ...}]  (200)
# DELETE /repos/{owner}/{repo}/issues/{number}/labels/{name}  -> 204
# GET    /repos/{owner}/{repo}/issues/{number}/events      -> [{id, event, actor, ...}]
#
# Requires Bearer (ghs_) or token (ghp_) auth. Issues are repo-scoped and
# use sequential numbers per repo. The comments/labels/events surfaces also
# cover PR numbers — GitHub serves PRs through the issues API.

# Shared helpers (_require_auth, _gh_not_found, _gh_err, _seed_issue_number,
# _next_id, _repo_key, _now) are preloaded from scripts/lib.star.

# on_list_issues returns issues for the given repo.
def on_list_issues(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)

    ic = store_collection("issues")
    all_issues = ic.list()
    result = []
    for i in all_issues:
        if i.get("repo", "") != repo_key:
            continue
        result.append(_issue_view(i))

    result = _apply_issue_filters(req, result)
    page, next_link = _list_page(req, result)
    return respond(200, page, _gh_link_headers(next_link))

# on_create_issue creates a new issue.
def on_create_issue(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    if repo_key != "octocat/hello-world":
        return _gh_not_found()

    body = req["body"]
    if body == None:
        body = {}

    num = _seed_issue_number(owner, repo)
    title = body.get("title", "")
    if title == None:
        title = ""

    labels_input = body.get("labels", [])
    if labels_input == None:
        labels_input = []
    labels = []
    for l in labels_input:
        if type(l) == "string":
            labels.append({"name": l, "color": "ededed"})
        elif type(l) == "dict":
            labels.append(l)

    issue = {
        "id": _next_id("issue_id"),
        "number": num,
        "repo": repo_key,
        "title": title,
        "body": body.get("body", ""),
        "state": "open",
        "user": {"login": "stunt-dev", "id": 1000002, "type": "Bot"},
        "labels": labels,
        "created_at": _now(),
        "updated_at": _now(),
    }

    ic = store_collection("issues")
    ic.insert(issue)

    # Emit webhook event if subscribed (action "opened" per GitHub's payload).
    _emit_if_subscribed(repo_key, "issues", _gh_event_payload(repo_key, "opened", "issue", _issue_view(issue)))

    return respond(201, _issue_view(issue))

# on_get_issue returns a single issue by number.
def on_get_issue(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["issue_number"])

    ic = store_collection("issues")
    i = _find_doc(ic, repo_key, number)
    if i == None:
        return _gh_not_found()
    return respond(200, _issue_view(i))

# on_update_issue updates an issue (PATCH — typically to close it). state must
# be exactly "open" or "closed" (anything else -> 422 Validation Failed, like
# the real API); closing sets closed_at and reopening clears it; state_reason
# (completed | not_planned | reopened) round-trips and is validated too.
def on_update_issue(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["issue_number"])

    body = req["body"]
    if body == None:
        body = {}

    state = body.get("state", None)
    if state != None and state != "open" and state != "closed":
        return _gh_validation_failed("Issue", "state", "invalid")
    state_reason = body.get("state_reason", None)
    if state_reason != None and state_reason not in ["completed", "not_planned", "reopened"]:
        return _gh_validation_failed("Issue", "state_reason", "invalid")

    ic = store_collection("issues")
    i = _find_doc(ic, repo_key, number)
    if i == None:
        return _gh_not_found()

    prev_state = i.get("state", "open")
    if state != None:
        i["state"] = state
    if state_reason != None:
        i["state_reason"] = state_reason
    if state == "closed" and prev_state != "closed":
        i["closed_at"] = _now()
    if state == "open" and prev_state != "open":
        i["closed_at"] = None
    if body.get("title", None) != None:
        i["title"] = body["title"]
    if body.get("body", None) != None:
        i["body"] = body["body"]
    i["updated_at"] = _now()
    ic.update(i["id"], i)

    # Emit webhook event if subscribed. GitHub reports state changes as
    # closed/reopened; other edits as "edited". Transitions are also recorded
    # on the issue-events surface.
    action = "edited"
    if prev_state != i.get("state", prev_state):
        if i.get("state", "") == "closed":
            action = "closed"
        else:
            action = "reopened"
        _record_issue_event(repo_key, number, action)
    _emit_if_subscribed(repo_key, "issues", _gh_event_payload(repo_key, action, "issue", _issue_view(i)))

    return respond(200, _issue_view(i))

# on_create_comment posts a comment on an issue or PR (GitHub's issues-comments
# API covers both — PRs share the issue number space). body is required.
def on_create_comment(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["issue_number"])

    body = req["body"]
    if body == None:
        body = {}
    text = body.get("body", "")
    if text == None or text == "":
        return _gh_validation_failed("IssueComment", "body", "missing")

    subject = _comment_subject(repo_key, number)
    if subject == None:
        return _gh_not_found()

    comment = {
        "id": _next_id("comment_id"),
        "repo": repo_key,
        "number": number,
        "body": text,
        "user": _actor(),
        "created_at": _now(),
        "updated_at": _now(),
    }
    store_collection("comments").insert(comment)

    # GitHub delivers issue_comment (action=created) with the comment plus the
    # subject under the "issue" key (a PR is rendered as an issue there).
    payload = _gh_event_payload(repo_key, "created", "issue", subject)
    payload["comment"] = _comment_view(comment)
    _emit_if_subscribed(repo_key, "issue_comment", payload)

    return respond(201, _comment_view(comment))

# on_list_comments lists comments on an issue or PR, oldest first (GitHub's
# order for this endpoint).
def on_list_comments(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["issue_number"])

    if _comment_subject(repo_key, number) == None:
        return _gh_not_found()

    cc = store_collection("comments")
    docs = []
    for c in cc.list():
        if c.get("repo", "") == repo_key and c.get("number", 0) == number:
            docs.append(c)
    docs = query_select(docs, [], "created_at", "asc")

    views = []
    for c in docs:
        views.append(_comment_view(c))
    page, next_link = _list_page(req, views)
    return respond(200, page, _gh_link_headers(next_link))

# on_add_label adds a label to an issue (or PR — same surface). Returns the
# issue's full label set, like the real API. Idempotent: re-adding a label
# already present is a no-op (200, no event).
def on_add_label(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["issue_number"])
    name = req["params"]["name"]

    ic = store_collection("issues")
    doc = _find_doc(ic, repo_key, number)
    coll = ic
    event_type = "issues"
    subject_key = "issue"
    if doc == None:
        coll = store_collection("pulls")
        doc = _find_doc(coll, repo_key, number)
        event_type = "pull_request"
        subject_key = "pull_request"
    if doc == None:
        return _gh_not_found()

    present = False
    for l in doc.get("labels", []):
        if l.get("name", "") == name:
            present = True
    if not present:
        labels = doc.get("labels", [])
        labels.append({"name": name, "color": "ededed"})
        doc["labels"] = labels
        doc["updated_at"] = _now()
        coll.update(doc["id"], doc)
        _record_issue_event(repo_key, number, "labeled", name)
        _emit_label_event(repo_key, event_type, subject_key, _subject_view(subject_key, doc), "labeled", name)

    views = []
    for l in doc.get("labels", []):
        views.append(l)
    return respond(200, views)

# on_remove_label removes a label from an issue (or PR). 204 on success, 404
# when the label is not on the issue (GitHub's behavior).
def on_remove_label(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["issue_number"])
    name = req["params"]["name"]

    ic = store_collection("issues")
    doc = _find_doc(ic, repo_key, number)
    coll = ic
    event_type = "issues"
    subject_key = "issue"
    if doc == None:
        coll = store_collection("pulls")
        doc = _find_doc(coll, repo_key, number)
        event_type = "pull_request"
        subject_key = "pull_request"
    if doc == None:
        return _gh_not_found()

    labels = doc.get("labels", [])
    kept = []
    found = False
    for l in labels:
        if l.get("name", "") == name:
            found = True
        else:
            kept.append(l)
    if not found:
        return _gh_not_found()

    doc["labels"] = kept
    doc["updated_at"] = _now()
    coll.update(doc["id"], doc)

    _record_issue_event(repo_key, number, "unlabeled", name)
    _emit_label_event(repo_key, event_type, subject_key, _subject_view(subject_key, doc), "unlabeled", name)
    return respond(204)

# on_list_issue_events returns the issue-events surface (labeled, unlabeled,
# closed, reopened, merged) for an issue or PR, newest first.
def on_list_issue_events(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["issue_number"])

    if _comment_subject(repo_key, number) == None:
        return _gh_not_found()

    ec = store_collection("issue_events")
    docs = []
    for e in ec.list():
        if e.get("repo", "") == repo_key and e.get("number", 0) == number:
            docs.append(e)
    docs = query_select(docs, [], "created_at", "desc")

    views = []
    for e in docs:
        views.append(_issue_event_view(e))
    page, next_link = _list_page(req, views)
    return respond(200, page, _gh_link_headers(next_link))

# --- helpers ---

# _record_issue_event and _pull_view live in lib.star (shared with
# pulls.star, which records merged/closed/reopened events and renders PRs).

# _subject_view renders a doc with its owning collection's view (issues vs
# pulls — both surfaces share the issue number space).
def _subject_view(subject_key, doc):
    if subject_key == "pull_request":
        return _pull_view(doc)
    return _issue_view(doc)

# _comment_subject returns the rendered issue/PR view for a number, or None
# when no such issue or PR exists in the repo.
def _comment_subject(repo_key, number):
    ic = store_collection("issues")
    doc = _find_doc(ic, repo_key, number)
    if doc != None:
        return _issue_view(doc)
    pc = store_collection("pulls")
    pdoc = _find_doc(pc, repo_key, number)
    if pdoc != None:
        return _pull_view(pdoc)
    return None

# _emit_label_event delivers the labeled/unlabeled webhook: an issues event
# for issues, a pull_request event for PRs (GitHub's split), each carrying the
# label that changed.
def _emit_label_event(repo_key, event_type, subject_key, view, action, name):
    payload = _gh_event_payload(repo_key, action, subject_key, view)
    payload["label"] = {"name": name, "color": "ededed"}
    _emit_if_subscribed(repo_key, event_type, payload)

def _comment_view(c):
    return {
        "id": _to_int(c["id"]),
        "body": c.get("body", ""),
        "user": c.get("user", {}),
        "created_at": c.get("created_at", _now()),
        "updated_at": c.get("updated_at", _now()),
        "html_url": "https://github.com/" + c.get("repo", "") + "/issues/" + str(c.get("number", 0)) + "#issuecomment-" + c["id"],
        "issue_url": "https://api.github.com/repos/" + c.get("repo", "") + "/issues/" + str(c.get("number", 0)),
    }

def _issue_event_view(e):
    v = {
        "id": _to_int(e["id"]),
        "event": e.get("event", ""),
        "actor": e.get("actor", {}),
        "created_at": e.get("created_at", _now()),
    }
    if e.get("label", None) != None:
        v["label"] = e["label"]
    return v

# _apply_issue_filters maps the real GitHub issue-list query params onto
# query_select, applied after repo scoping and before paging like the real
# API. state defaults to "open" (GitHub's default). labels requires the
# issue to carry every requested label name. since filters on updated_at.
# sort supports created (default) / updated (comments falls back to
# created — no comment counts are stored); direction defaults to desc.
def _apply_issue_filters(req, docs):
    labels_q = _get_query(req, "labels", "")
    if labels_q != "":
        wanted = []
        for part in labels_q.split(","):
            part = part.strip()
            if part != "":
                wanted.append(part)
        kept = []
        for d in docs:
            names = []
            for l in d.get("labels", []):
                names.append(l.get("name", ""))
            ok = True
            for w in wanted:
                if w not in names:
                    ok = False
                    break
            if ok:
                kept.append(d)
        docs = kept

    f = []
    state = _get_query(req, "state", "open")
    if state != "all":
        f.append(["state", "=", state])
    creator = _get_query(req, "creator", "")
    if creator != "":
        f.append(["user.login", "=", creator])
    since = _get_query(req, "since", "")
    if since != "":
        f.append(["updated_at", ">=", since])

    sort = _get_query(req, "sort", "created")
    order_by = "created_at"
    if sort == "updated":
        order_by = "updated_at"
    direction = _get_query(req, "direction", "desc")

    return query_select(docs, f, order_by, direction)

def _issue_view(i):
    return {
        "id": _to_int(i["id"]),
        "number": i.get("number", 0),
        "title": i.get("title", ""),
        "body": i.get("body", ""),
        "state": i.get("state", "open"),
        "state_reason": i.get("state_reason", None),
        "closed_at": i.get("closed_at", None),
        "user": i.get("user", {}),
        "labels": i.get("labels", []),
        "created_at": i.get("created_at", _now()),
        "updated_at": i.get("updated_at", _now()),
    }

# _emit_if_subscribed lives in lib.star (shared with actions.star) and now
# signs deliveries.
