# Git handlers — Azure DevOps git repository operations.
#
# GET  /{org}/{project}/_apis/git/repositories                       → {value:[...], count}
# GET  /{org}/{project}/_apis/git/repositories/{repoId}/items?path=   → file content from stored commit state
# GET  /{org}/{project}/_apis/git/repositories/{repoId}/commits       → {value:[...], count}
# POST /{org}/{project}/_apis/git/repositories/{repoId}/pushes        → push (stores commits/blobs/refs)
#
# STATE MODEL: a push applies its commit changes onto the target ref's
# current tree (a path → blob-id snapshot stored on each commit doc), mints
# blob ids for new content, and moves the ref. Items are served by resolving
# versionDescriptor (branch/tag/commit, defaulting to the repo default
# branch) to a commit and reading that commit's tree — so GETs reflect
# exactly what was pushed, at the version asked for. Unknown paths 404.

_NULL_ID = "0" * 40  # git null object id

def on_list_repos(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    project_name = req["params"]["project"]
    rc = store_collection("repos")
    items = []
    for r in rc.list():
        proj = r.get("project", {})
        if proj.get("name", "") == project_name or project_name == "_apis":
            items.append(_repo_resource(r))

    # Apply OData $top/$skip paging (after filtering by project).
    page, continuation = _list_page(req, items)

    resp = {"value": page, "count": len(page)}
    if continuation != None:
        resp["continuationToken"] = continuation
    return respond(200, resp)

# on_get_item returns a file's content as of a version. versionDescriptor is
# a JSON query param: {"versionType":"branch"|"tag"|"commit","version":"..."}.
def on_get_item(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    repo_id = req["params"]["repoId"]
    repo = _repo_by_id(repo_id)
    if repo == None:
        return _git_fault(404, "The repository " + repo_id + " does not exist.", "GitRepositoryNotFoundException", 404)

    path = _get_query(req, "path", "")
    if path == None or path == "":
        return _git_fault(400, "A path must be supplied.", "GitArgumentOutOfRangeException", 400)

    commit_id = _resolve_commit_id(req, repo)
    if commit_id == None:
        return _git_fault(404, "The item " + path + " does not exist at the requested version.", "GitItemNotFoundException", 4096)

    cc = store_collection("commits")
    commit = cc.get(commit_id)
    if commit == None:
        return _git_fault(404, "The item " + path + " does not exist at the requested version.", "GitItemNotFoundException", 4096)

    tree = commit.get("tree", {})
    key = _norm_path(path)
    if key not in tree:
        return _git_fault(404, "The item " + path + " does not exist at the requested version.", "GitItemNotFoundException", 4096)

    blob_id = tree[key]
    bc = store_collection("gitblobs")
    blob = bc.get(blob_id)
    content = ""
    if blob != None:
        content = blob.get("content", "")

    return respond(200, {
        "objectId": blob_id,
        "gitObjectType": "blob",
        "commitId": commit_id,
        "path": "/" + key,
        "content": content,
        "contentMetadata": {"encoding": "utf-8"},
        "_links": {
            "self": {
                "href": "https://dev.azure.com/mock-org/MyFirstProject/_apis/git/repositories/" + repo_id + "/items?path=" + path + "&versionDescriptor=" + _get_query(req, "versionDescriptor", ""),
            },
            "repository": {
                "href": "https://dev.azure.com/mock-org/MyFirstProject/_apis/git/repositories/" + repo_id,
            },
        },
    })

# on_list_commits lists the commits recorded for a repository (newest first),
# optionally scoped by searchCriteria.refName.
def on_list_commits(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    repo_id = req["params"]["repoId"]
    if _repo_by_id(repo_id) == None:
        return _git_fault(404, "The repository " + repo_id + " does not exist.", "GitRepositoryNotFoundException", 404)

    ref_name = _get_query(req, "searchCriteria.refName", "")

    # Walk each ref head back through its parents (iterative — no recursion).
    cc = store_collection("commits")
    wanted = []
    seen = {}
    fc = store_collection("refs")
    for f in fc.list():
        if not f.get("id", "").startswith(repo_id + "|"):
            continue
        if ref_name != "" and f.get("id", "") != _ref_key(repo_id, ref_name):
            continue
        cur = f.get("object_id", None)
        while cur != None and cur not in seen:
            seen[cur] = True
            c = cc.get(cur)
            if c == None:
                break
            wanted.append(c)
            cur = c.get("parent", None)

    items = []
    for c in wanted:
        items.append({
            "commitId": c.get("id", ""),
            "author": c.get("author", {}),
            "comment": c.get("comment", ""),
            "changes": c.get("changes", []),
            "parents": [c.get("parent", "")] if c.get("parent", None) != None else [],
            "url": "https://dev.azure.com/mock-org/MyFirstProject/_apis/git/repositories/" + repo_id + "/commits/" + c.get("id", ""),
        })

    page, continuation = _list_page(req, items)
    resp = {"value": page, "count": len(page)}
    if continuation != None:
        resp["continuationToken"] = continuation
    return respond(200, resp)

# on_push stores the pushed commits (applying each commit's changes onto the
# ref's current tree), mints blob ids for new content, and advances the ref.
def on_push(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    repo_id = req["params"]["repoId"]
    repo = _repo_by_id(repo_id)
    if repo == None:
        return _git_fault(404, "The repository " + repo_id + " does not exist.", "GitRepositoryNotFoundException", 404)

    body = req["body"]
    if body == None:
        body = {}

    ref_updates = body.get("refUpdates", [])
    if ref_updates == None:
        ref_updates = []
    commits = body.get("commits", [])
    if commits == None:
        commits = []

    default_branch = repo.get("defaultBranch", "refs/heads/main")
    ref_name = default_branch
    if len(ref_updates) > 0:
        ru0 = ref_updates[0]
        if ru0 != None and ru0.get("name", "") != "":
            ref_name = ru0.get("name", "")

    fc = store_collection("refs")
    old_id = _NULL_ID
    parent_tree = {}
    fdoc = fc.get(_ref_key(repo_id, ref_name))
    if fdoc != None:
        old_id = fdoc.get("object_id", _NULL_ID)
    cc = store_collection("commits")
    parent_commit = cc.get(old_id) if old_id != _NULL_ID else None
    if parent_commit != None:
        for k in parent_commit.get("tree", {}):
            parent_tree[k] = parent_commit["tree"][k]

    tree = {}
    for k in parent_tree:
        tree[k] = parent_tree[k]

    bc = store_collection("gitblobs")
    now_iso = _now_iso()
    push_id = store_kv_incr("azure-devops", "push_seq") + 1
    resp_commits = []
    head_id = old_id if old_id != _NULL_ID else None
    parent_id = None if old_id == _NULL_ID else old_id

    for c in commits:
        if c == None:
            continue
        commit_id = _new_object_id("commit")
        changes = c.get("changes", [])
        if changes == None:
            changes = []
        author = c.get("author", None)
        if author == None:
            author = {"name": "Test User", "email": "test@example.com", "date": now_iso}
        resp_changes = []
        for ch in changes:
            if ch == None:
                continue
            item = ch.get("item", {})
            if item == None:
                item = {}
            key = _norm_path(item.get("path", ""))
            if key == "":
                continue
            change_type = ch.get("changeType", "edit")
            if change_type == "delete":
                tree = _dict_delete(tree, key)
                resp_changes.append({
                    "changeType": "delete",
                    "item": {"path": "/" + key, "gitObjectType": "blob"},
                })
                continue
            new_content = ch.get("newContent", None)
            content = ""
            if new_content != None:
                content = new_content.get("content", "")
            blob_id = _new_object_id("blob")
            bc.insert({"id": blob_id, "content": content})
            tree[key] = blob_id
            resp_changes.append({
                "changeType": change_type,
                "item": {"path": "/" + key, "objectId": blob_id, "gitObjectType": "blob"},
                "newContent": new_content,
            })
        cc.insert({
            "id": commit_id,
            "repo_id": repo_id,
            "comment": c.get("comment", ""),
            "author": author,
            "parent": parent_id,
            "push_id": push_id,
            "tree": tree,
            "changes": resp_changes,
        })
        resp_commits.append({
            "commitId": commit_id,
            "author": author,
            "comment": c.get("comment", ""),
            "changes": resp_changes,
            "treeId": commit_id,
            "parents": [parent_id] if parent_id != None else [],
        })
        head_id = commit_id
        parent_id = commit_id

    if head_id == None:
        head_id = _NULL_ID

    # Advance the ref (create-or-update).
    if fdoc != None:
        fdoc["object_id"] = head_id
        fc.update(fdoc["id"], fdoc)
    else:
        fc.insert({"id": _ref_key(repo_id, ref_name), "object_id": head_id})

    resp_ref_updates = []
    for i in range(len(ref_updates)):
        ru = ref_updates[i]
        if ru == None:
            continue
        new_id = head_id if i == 0 else ru.get("newObjectId", head_id)
        resp_ref_updates.append({
            "name": ru.get("name", ref_name),
            "oldObjectId": ru.get("oldObjectId", old_id),
            "newObjectId": new_id,
        })

    push_resource = {
        "pushId": push_id,
        "date": now_iso,
        "repository": _repo_resource(repo),
        "pushedBy": {"displayName": "Test User", "id": "aaaaaaaa-bbbb-cccc-dddd-eeeeffff0001", "uniqueName": "test@example.com"},
        "commits": resp_commits,
        "refUpdates": resp_ref_updates,
        "url": "https://dev.azure.com/mock-org/MyFirstProject/_apis/git/repositories/" + repo_id + "/pushes/" + str(push_id),
    }

    # Emit service-hook event (git.push) if a subscription exists.
    _emit_service_hook("git.push", "Push " + str(push_id) + " to " + repo.get("name", ""), push_resource)

    return respond(200, push_resource)

# --- helpers ---

# _norm_path strips leading slashes and normalizes backslashes for tree keys.
def _norm_path(path):
    p = path.replace("\\", "/")
    while p[:1] == "/":
        p = p[1:]
    return p

# _resolve_commit_id maps the request's versionDescriptor (or the repo's
# default branch) to a commit id, or None when unresolvable.
def _resolve_commit_id(req, repo):
    vt = "branch"
    version = repo.get("defaultBranch", "refs/heads/main")
    vd = _get_query(req, "versionDescriptor", "")
    if vd != None and vd != "" and vd[:1] == "{":
        parsed = json.decode(vd)
        vt = parsed.get("versionType", "branch")
        version = parsed.get("version", "")

    if vt == "commit":
        return version

    if version[:5] == "refs/":
        ref_name = version
    elif vt == "tag":
        ref_name = "refs/tags/" + version
    else:
        ref_name = "refs/heads/" + version

    fc = store_collection("refs")
    fdoc = fc.get(_ref_key(repo.get("id", ""), ref_name))
    if fdoc == None:
        return None
    return fdoc.get("object_id", None)

# _git_fault builds a Azure DevOps error envelope.
def _git_fault(status, message, type_key, event_id):
    return respond(status, {
        "$id": "1",
        "innerException": None,
        "message": message,
        "typeName": "Microsoft.TeamFoundation.Git.Server." + type_key,
        "typeKey": type_key,
        "errorCode": 0,
        "eventId": event_id,
    })

# _repo_resource builds the API response shape for a repo.
def _repo_resource(r):
    return {
        "id": r.get("id", ""),
        "name": r.get("name", ""),
        "url": r.get("url", ""),
        "project": r.get("project", {}),
        "defaultBranch": r.get("defaultBranch", "refs/heads/main"),
        "size": r.get("size", 0),
        "remoteUrl": r.get("remoteUrl", ""),
        "sshUrl": r.get("sshUrl", ""),
        "webUrl": r.get("webUrl", ""),
    }
