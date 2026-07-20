# Shared library for azure-devops-style adapter scripts.

# _check_auth validates Azure DevOps PAT auth. Accepts either:
#   Authorization: Basic <base64(PAT:)>  (PAT as username, empty password)
#   Authorization: Bearer <PAT>
# Returns the token string if valid, or None if missing.
def _check_auth(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:6] == "Basic ":
        return auth[6:]
    if auth[:7] == "Bearer ":
        return auth[7:]
    return None

# _require_auth returns (token, None) if auth is present, or
# (None, error_response) if missing.
def _require_auth(req):
    token = _check_auth(req)
    if token == None:
        return None, respond(401, {
            "$id": "1",
            "innerException": None,
            "message": "Access Denied: The Personal Access Token used has expired, is invalid, or does not have the necessary permissions.",
            "typeName": "Microsoft.TeamFoundation.Framework.Server.UnauthorizedRequestException",
            "typeKey": "UnauthorizedRequestException",
            "errorCode": 0,
            "eventId": 3000,
        })
    return token, None

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

# _seed populates default projects, repos, and a work item.
def _seed():
    if store_kv_get("azure-devops", "seeded") == "yes":
        return
    store_kv_set("azure-devops", "seeded", "yes")

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
        "id": "11111111-0000-0000-0000-000000000001",
        "name": "MyFirstProject",
        "url": "https://dev.azure.com/mock-org/MyFirstProject/_apis/git/repositories/11111111-0000-0000-0000-000000000001",
        "project": {
            "id": "00000000-0000-0000-0000-000000000001",
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

# _find_project returns the project doc by name, or None.
def _find_project(name):
    pc = store_collection("projects")
    for p in pc.list():
        if p.get("name") == name:
            return p
    return None
