# Shared library for github-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ============================================================================
# GITHUB WEBHOOK SIGNATURE SCHEME (DOCUMENTATION)
# ============================================================================
# GitHub signs every webhook delivery with HMAC-SHA256 of the raw request
# body using the webhook secret configured when the hook was registered.
#
# Headers:
#   X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(webhook_secret, raw_body))>
#   X-Hub-Signature:     sha1=<hex(HMAC-SHA1(webhook_secret, raw_body))>   (legacy)
#   X-GitHub-Event:      <event_type>  (push, pull_request, issues, etc.)
#   X-GitHub-Delivery:   <uuid>
#
# Verification in Go:
#   mac := hmac.New(sha256.New, []byte(webhookSecret))
#   mac.Write(rawBody)
#   expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
#   if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Hub-Signature-256"))) {
#       return 401 // invalid signature
#   }
#
# IMPORTANT: GitHub expects a 200 response. If verification fails, return 401.
# Successful processing should return 200. GitHub retries with exponential
# backoff for non-2xx responses.
#
# The event type is in the X-GitHub-Event header (e.g. "push",
# "pull_request", "issues", "issue_comment"). This adapter emits events via
# events_emit using the same type names.
# ============================================================================

# _require_auth checks for an Authorization header. Accepts:
#   "Bearer <jwt_or_ghs_token>" — GitHub App JWT or installation token
#   "token <ghp_token>"         — Personal Access Token (PAT)
# Returns None if authorized, or a 401 error-response dict if not.
def _require_auth(req):
    headers = req.get("headers")
    if headers == None:
        return _gh_unauthorized()
    auth = headers.get("Authorization", "")
    if auth == None or auth == "":
        return _gh_unauthorized()
    if not auth.startswith("Bearer ") and not auth.startswith("token "):
        return _gh_unauthorized()
    return None

# _require_app_jwt checks specifically for a Bearer token (app JWT).
# The /app and /app/installations endpoints require an app JWT, not a PAT.
def _require_app_jwt(req):
    headers = req.get("headers")
    if headers == None:
        return _gh_unauthorized()
    auth = headers.get("Authorization", "")
    if auth == None or auth == "":
        return _gh_unauthorized()
    if not auth.startswith("Bearer "):
        return _gh_unauthorized()
    return None

# _token extracts the token from either "Bearer <t>" or "token <t>".
def _token(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    if auth.startswith("token "):
        return auth[6:]
    return ""

# _gh_unauthorized returns a GitHub-style 401 error response.
def _gh_unauthorized():
    return respond(401, {
        "message": "Requires authentication",
        "documentation_url": "https://docs.github.com/rest",
    })

# _gh_err returns a GitHub-style error response.
def _gh_err(status_code, message):
    return respond(status_code, {
        "message": message,
        "documentation_url": "https://docs.github.com/rest",
    })

# _gh_not_found returns a GitHub-style 404.
def _gh_not_found():
    return respond(404, {
        "message": "Not Found",
        "documentation_url": "https://docs.github.com/rest",
    })

# _now returns a synthetic ISO-8601 timestamp.
def _now():
    return "2024-07-01T12:00:00Z"

# _repo_key returns a unique collection key for a repo's scoped data,
# combining owner/repo so multiple repos can coexist.
def _repo_key(owner, repo):
    return owner + "/" + repo

# _seed_issue_number returns the next issue/PR number for a repo. GitHub uses
# repo-scoped sequential numbers starting at 1.
def _seed_issue_number(owner, repo):
    n = store_kv_incr("github", "issue_seq_" + _repo_key(owner, repo))
    return n

# _seed populates default repos, issues, PRs on first access.
def _seed():
    if store_kv_get("github", "seeded") == "yes":
        return
    store_kv_set("github", "seeded", "yes")

    # Seed the default repo's issue sequence.
    store_kv_set("github", "issue_seq_octocat/hello-world", "1")

    ic = store_collection("issues")
    ic.insert({
        "id": _next_id("issue_id"),
        "number": 1,
        "repo": "octocat/hello-world",
        "title": "Welcome to the synthetic repo!",
        "body": "This is a seeded issue for testing.",
        "state": "open",
        "user": {"login": "octocat", "id": 1, "type": "User"},
        "labels": [{"name": "documentation", "color": "0075ca"}],
        "created_at": _now(),
        "updated_at": _now(),
    })

    pc = store_collection("pulls")
    pc.insert({
        "id": _next_id("pull_id"),
        "number": 1,
        "repo": "octocat/hello-world",
        "title": "Initial setup PR",
        "body": "Seeded pull request for testing.",
        "state": "open",
        "draft": False,
        "user": {"login": "octocat", "id": 1, "type": "User"},
        "head": {"ref": "develop", "sha": "abc123def456"},
        "base": {"ref": "main", "sha": "def456abc789"},
        "created_at": _now(),
        "updated_at": _now(),
    })

    rc = store_collection("runs")
    rc.insert({
        "id": _next_id("run_id"),
        "repo": "octocat/hello-world",
        "name": "CI",
        "head_branch": "main",
        "status": "completed",
        "conclusion": "success",
        "event": "push",
        "html_url": "https://github.com/octocat/hello-world/actions/runs/1",
        "created_at": _now(),
        "updated_at": _now(),
    })

# _next_id returns a monotonically-increasing numeric ID string.
_BASE_ID = 80000000

def _next_id(kind):
    n = store_kv_incr("github", kind + "_seq")
    return str(_BASE_ID + n)

# _to_int parses a decimal string to int. Returns 0 for None or empty.
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
