# Shared library for azure-devops-style adapter scripts.

# _check_auth extracts the Azure DevOps PAT credential. Accepts either:
#   Authorization: Basic <base64(PAT:)>  (PAT as username, empty password)
#   Authorization: Bearer <PAT>
# Returns the presented credential string if present, or None if missing.
def _check_auth(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:6] == "Basic ":
        return auth[6:]
    if auth[:7] == "Bearer ":
        return auth[7:]
    return None

# _auth_fault returns the Azure DevOps 401 envelope for a missing, unknown,
# or expired PAT.
def _auth_fault():
    return respond(401, {
        "$id": "1",
        "innerException": None,
        "message": "Access Denied: The Personal Access Token used has expired, is invalid, or does not have the necessary permissions.",
        "typeName": "Microsoft.TeamFoundation.Framework.Server.UnauthorizedRequestException",
        "typeKey": "UnauthorizedRequestException",
        "errorCode": 0,
        "eventId": 3000,
    })

# _seed_known_pats inserts (once, guarded by a KV flag) the static mock PATs
# used by engine tests into the "pats" collection. Azure DevOps has no token
# minting endpoint in this sim (PATs are created in the portal UI), so this
# bootstrap stands in for "a PAT the org previously issued". Both wire forms
# of the same PAT are stored: the raw PAT (Bearer) and its base64(PAT:)
# encoding (Basic). Far-future expiry; unknown PATs still 401.
def _seed_known_pats():
    if store_kv_get("azure-devops", "pats_seeded") == "yes":
        return
    store_kv_set("azure-devops", "pats_seeded", "yes")
    pc = store_collection("pats")
    far = clock.now_unix() + 3600*24*365
    pc.insert({"id": "testPAT", "expires_at": far})
    pc.insert({"id": "dGVzdFBBVDo=", "expires_at": far})

# _require_auth returns (token, None) if the presented PAT is known to the
# store and not expired, or (None, error_response) if missing/unknown/expired.
def _require_auth(req):
    token = _check_auth(req)
    if token == None:
        return None, _auth_fault()
    _seed_known_pats()
    pc = store_collection("pats")
    doc = pc.get(token)
    if doc == None:
        return None, _auth_fault()
    exp = doc.get("expires_at", 0)
    if exp != None and exp != 0 and clock.now_unix() > exp:
        return None, _auth_fault()
    return token, None

# _as_int coerces a value that may have round-tripped through the JSON-backed
# store as a float back to an int (ids stay clean in URLs and str()).
def _as_int(v):
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(v)

# _to_int parses a decimal string to int.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _get_query safely returns a query parameter value, or default_val if the
# parameter is absent or None.
def _get_query(req, key, default_val):
    q = req.get("query")
    if q == None:
        return default_val
    val = q.get(key, default_val)
    if val == None:
        return default_val
    return val

# _now_iso returns the current UTC time in RFC3339 form (ADO date fields).
def _now_iso():
    return clock.now_rfc3339()

_HEXDIGITS = "0123" + "45" + "6789abcdef"

# _hex40 renders n as a 40-char lowercase hex string (git object id shape).
def _hex40(n):
    s = ""
    v = n
    for _ in range(40):
        s = _HEXDIGITS[v % 16] + s
        v = v // 16
    return s

# _new_object_id mints a synthetic 40-hex git object id from a monotonic
# per-kind sequence (commit_<n>, blob_<n>).
def _new_object_id(kind):
    return _hex40(store_kv_incr("azure-devops", kind + "_seq") + 1)

# _repo_by_id returns the repo doc for a repository id, or None.
def _repo_by_id(repo_id):
    rc = store_collection("repos")
    return rc.get(repo_id)

# _ref_key is the collection id for a repo ref (repo id + "|" + ref name).
def _ref_key(repo_id, ref_name):
    return repo_id + "|" + ref_name

# _dict_delete returns d minus key k (Starlark dicts have no del).
def _dict_delete(d, k):
    out = {}
    for key in d:
        if key != k:
            out[key] = d[key]
    return out

# _list_page reads Azure DevOps OData $top (page size) / $skip (offset cursor)
# query params, slices the already-filtered docs via the paginate() builtin,
# and returns (page, continuation_token) where continuation_token is a token
# string the client round-trips via $skip (and surfaced as the response's
# top-level continuationToken field), or None when there is no further page.
# Paging is DISABLED (whole list returned, continuation_token None) when $top
# is missing or <= 0 — preserving prior unpaginated behavior.
def _list_page(req, docs):
    top = _to_int(_get_query(req, "$top", ""))
    skip = _get_query(req, "$skip", "")
    if skip == None:
        skip = ""

    page, next_cursor = paginate(docs, top, skip)
    return page, next_cursor

# _seed populates default projects, repos, and a work item.
def _seed():
    if store_kv_get("azure-devops", "seeded") == "yes":
        return
    store_kv_set("azure-devops", "seeded", "yes")

    repo1_id = "11111111-0000-0000-0000-000000000001"
    project1_id = "00000000-0000-0000-0000-000000000001"

    pc = store_collection("projects")
    pc.insert({
        "id": "00000000-0000-0000-0000-000000000001",
        "name": "MyFirstProject",
        "description": "A test project for local development",
        "url": "https://dev.azure.com/mock-org/_apis/projects/00000000-0000-0000-0000-000000000001",
        "state": "wellFormed",
        "visibility": "private",
        "revision": 1,
    })
    pc.insert({
        "id": "00000000-0000-0000-0000-000000000002",
        "name": "BackendServices",
        "description": "Backend microservices",
        "url": "https://dev.azure.com/mock-org/_apis/projects/00000000-0000-0000-0000-000000000002",
        "state": "wellFormed",
        "visibility": "private",
        "revision": 1,
    })

    rc = store_collection("repos")
    rc.insert({
        "id": repo1_id,
        "name": "MyFirstProject",
        "url": "https://dev.azure.com/mock-org/MyFirstProject/_apis/git/repositories/" + repo1_id,
        "project": {
            "id": project1_id,
            "name": "MyFirstProject",
        },
        "defaultBranch": "refs/heads/main",
        "size": 1024,
        "remoteUrl": "https://dev.azure.com/mock-org/MyFirstProject/_git/MyFirstProject",
        "sshUrl": "git@ssh.dev.azure.com:v3/mock-org/MyFirstProject/MyFirstProject",
        "webUrl": "https://dev.azure.com/mock-org/MyFirstProject/_git/MyFirstProject",
    })

    wc = store_collection("workitems")
    wc.insert({
        "id": "1",
        "wi_id": 1,
        "rev": 1,
        "fields": {
            "System.AreaPath": "MyFirstProject",
            "System.TeamProject": "MyFirstProject",
            "System.IterationPath": "MyFirstProject",
            "System.WorkItemType": "Bug",
            "System.State": "Active",
            "System.Reason": "New",
            "System.CreatedDate": "2024-01-01T00:00:00.000Z",
            "System.CreatedBy": "Test User <test@example.com>",
            "System.ChangedDate": "2024-01-01T00:00:00.000Z",
            "System.ChangedBy": "Test User <test@example.com>",
            "System.Title": "Sample bug report",
            "System.Description": "This is a sample bug for testing.",
        },
        "url": "https://dev.azure.com/mock-org/MyFirstProject/_apis/wit/workItems/1",
    })

    # Initial git state: one seeded commit on refs/heads/main carrying a
    # readme.md blob, so items/commits reflect real content from the start.
    blob_id = _new_object_id("blob")
    bc = store_collection("gitblobs")
    bc.insert({"id": blob_id, "content": "# MyFirstProject\n\nSeeded readme for local testing.\n"})

    commit_id = _new_object_id("commit")
    cc = store_collection("commits")
    cc.insert({
        "id": commit_id,
        "repo_id": repo1_id,
        "comment": "Initial commit",
        "author": {"name": "Test User", "email": "test@example.com", "date": "2024-01-01T00:00:00.000Z"},
        "parent": None,
        "push_id": 0,
        "tree": {"readme.md": blob_id},
    })

    fc = store_collection("refs")
    fc.insert({"id": _ref_key(repo1_id, "refs/heads/main"), "object_id": commit_id})

    # One CI pipeline.
    plc = store_collection("pipelines")
    plc.insert({
        "id": "1",
        "pipeline_id": 1,
        "name": "MyFirstProject-CI",
        "folder": "\\",
        "configuration": {
            "type": "yaml",
            "path": "/azure-pipelines.yml",
            "repository": {"type": "azureReposGit", "name": "MyFirstProject", "id": repo1_id},
        },
    })

# _find_project returns the project doc by name, or None.
def _find_project(name):
    pc = store_collection("projects")
    for p in pc.list():
        if p.get("name") == name:
            return p
    return None

# ============================================================================
# AZURE DEVOPS SERVICE HOOK DELIVERY (DOCUMENTATION)
# ============================================================================
# Service hooks (generic "webHooks" consumer) POST an event envelope like:
#
#   {
#     "subscriptionId": "...", "notificationId": 1, "id": "...",
#     "eventType": "workitem.created", "publisherId": "tfs",
#     "message": {"text": "..."}, "detailedMessage": {"text": "..."},
#     "resource": {...}, "resourceVersion": "1.0", "createdDate": "..."
#   }
#
# Azure DevOps signs NOTHING — deliveries are UNSIGNED BY DESIGN. Secure the
# receiving endpoint via the subscription's basic auth / bearer / custom
# headers configuration. stunt does NOT invent a signature. _emit_service_hook
# delivers only when a subscription for that eventType exists.
def _emit_service_hook(event_type, message_text, resource):
    sc = store_collection("subscriptions")
    for s in sc.list():
        if s.get("eventType", "") == event_type:
            payload = {
                "subscriptionId": s.get("id", ""),
                "notificationId": store_kv_incr("azure-devops", "notif_seq") + 1,
                "id": _next_notification_id(),
                "eventType": event_type,
                "publisherId": "tfs",
                "scope": "all",
                "message": {"text": message_text, "html": message_text, "markdown": message_text},
                "detailedMessage": {"text": message_text, "html": message_text, "markdown": message_text},
                "resource": resource,
                "resourceVersion": "1.0",
                "createdDate": "2024-01-01T00:00:00.000Z",
            }
            events_emit(event_type, payload)
            return

# _next_subscription_id returns a synthetic subscription GUID.
def _next_subscription_id():
    n = store_kv_incr("azure-devops", "sub_seq") + 1
    return "aaaa000" + str(n) + "-bbbb-cccc-dddd-eeeeffff0000"

# _next_notification_id returns a synthetic notification GUID.
def _next_notification_id():
    n = store_kv_incr("azure-devops", "notif_guid_seq") + 1
    return "ffff000" + str(n) + "-bbbb-cccc-dddd-eeeeaaaa0000"
