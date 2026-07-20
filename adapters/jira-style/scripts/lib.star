# Shared library for jira-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins.

# Jira Cloud auth: Basic (email:api_token) or Bearer (PAT). Both are checked.

# _auth_header extracts the Authorization header value. Returns "" if absent.
def _auth_header(req):
    auth = req["headers"].get("Authorization", "")
    if auth == None:
        auth = ""
    return auth

# _has_auth checks whether a valid auth header is present (Basic or Bearer).
def _has_auth(req):
    auth = _auth_header(req)
    if auth == "":
        return False
    if auth.startswith("Basic "):
        return True
    if auth.startswith("Bearer "):
        return True
    return False

# _require_auth validates auth. Returns (account_id, error_resp). If no auth
# is present, returns a 401 error response.
def _require_auth(req):
    if not _has_auth(req):
        return None, _auth_error()
    # All auth is accepted (mock). Return a synthetic account ID.
    return "5f1b3a4c5d6e7f8a9b0c1d2e", None

# _auth_error returns the Jira 401 error response.
def _auth_error():
    return respond(401, {
        "errorMessages": ["You do not have the permission to see the specified issue"],
        "errors": {},
    })

# _jira_error returns a Jira-style error response.
def _jira_error(status, message, field_errors):
    return respond(status, {
        "errorMessages": [message],
        "errors": field_errors,
    })

# _not_found returns a 404 for a missing issue/resource.
def _not_found():
    return respond(404, {
        "errorMessages": ["Issue Does Not Exist"],
        "errors": {},
    })

# _now returns a synthetic timestamp.
def _now():
    return "2024-01-01T00:00:00.000+0000"

# _next_issue_id returns a monotonically-increasing numeric issue ID.
def _next_issue_id():
    n = store_kv_incr("jira", "issue_seq")
    return str(10000 + n)

# _next_comment_id returns a monotonically-increasing comment ID.
def _next_comment_id():
    n = store_kv_incr("jira", "comment_seq")
    return str(10000 + n)

# _project_from_key extracts the project key from an issue key like "TEST-1".
def _project_from_key(issue_key):
    parts = _split(issue_key, "-")
    if len(parts) >= 2:
        return parts[0]
    return ""

# _issue_number returns the numeric suffix from an issue key like "TEST-1".
def _issue_number(issue_key):
    parts = _split(issue_key, "-")
    if len(parts) >= 2:
        return parts[len(parts) - 1]
    return ""

# _split splits a string on a delimiter (single-char). Returns a list.
def _split(s, delim):
    result = []
    current = ""
    for i in range(len(s)):
        ch = s[i]
        if ch == delim:
            result.append(current)
            current = ""
        else:
            current = current + ch
    result.append(current)
    return result

# _lower returns a lowercased copy of the string.
def _lower(s):
    out = ""
    for i in range(len(s)):
        ch = s[i]
        code = ord(ch)
        if code >= 65 and code <= 90:
            code = code + 32
        out += chr(code)
    return out

# _index returns the index of the first occurrence of needle in haystack, or
# -1 if not found.
def _index(haystack, needle):
    if len(needle) == 0:
        return 0
    for i in range(len(haystack) - len(needle) + 1):
        match = True
        for j in range(len(needle)):
            if haystack[i + j] != needle[j]:
                match = False
                break
        if match:
            return i
    return -1

# _contains returns True if haystack contains needle.
def _contains(haystack, needle):
    return _index(haystack, needle) >= 0

# _trim strips leading/trailing spaces from a string.
def _trim(s):
    start = 0
    end = len(s)
    while start < end and s[start] == " ":
        start = start + 1
    while end > start and s[end - 1] == " ":
        end = end - 1
    return s[start:end]

# _to_int converts a string to an int (returns 0 on failure).
def _to_int(s):
    if s == "" or s == None:
        return 0
    result = 0
    for i in range(len(s)):
        ch = s[i]
        code = ord(ch)
        if code >= 48 and code <= 57:
            result = result * 10 + (code - 48)
        else:
            return 0
    return result

# _parse_jql extracts the project key and optional status filter from a JQL
# query. Pattern-matches — no real JQL parser. Returns (project_key, status).
# Examples:
#   "project = TEST" -> ("TEST", "")
#   "project = TEST AND status = Done" -> ("TEST", "Done")
def _parse_jql(jql):
    project_key = ""
    status = ""

    lower_jql = _lower(jql)

    # Extract project key.
    proj_idx = _index(lower_jql, "project")
    if proj_idx >= 0:
        rest = jql[proj_idx:]
        lower_rest = _lower(rest)
        # Find "=" or "in"
        eq_idx = _index(lower_rest, "=")
        if eq_idx >= 0:
            after_eq = rest[eq_idx + 1:]
            # Find the project key token.
            project_key = _extract_token(after_eq)
        else:
            in_idx = _index(lower_rest, " in ")
            if in_idx >= 0:
                after_in = rest[in_idx + 4:]
                project_key = _extract_token(after_in)

    # Extract status filter.
    status_idx = _index(lower_jql, "status")
    if status_idx >= 0:
        rest = jql[status_idx:]
        lower_rest = _lower(rest)
        eq_idx = _index(lower_rest, "=")
        if eq_idx >= 0:
            after_eq = rest[eq_idx + 1:]
            status = _extract_token(after_eq)

    return project_key, status

# _extract_token extracts the first meaningful token after "=". Handles
# quoted strings ("TEST") and bare words (TEST).
def _extract_token(s):
    s = _trim(s)
    if len(s) == 0:
        return ""
    # Quoted value.
    if s[0] == "'":
        end = _index(s[1:], "'")
        if end >= 0:
            return s[1:end + 1]
        return s[1:]
    if s[0] == '"':
        end = _index(s[1:], '"')
        if end >= 0:
            return s[1:end + 1]
        return s[1:]
    # Bare word — read until space.
    result = ""
    for i in range(len(s)):
        ch = s[i]
        if ch == " " or ch == "\n" or ch == "\t":
            break
        result = result + ch
    return result

# _get_body safely returns the request body dict.
def _get_body(req):
    body = req.get("body")
    if body == None:
        return {}
    return body

# _get_query safely returns a query parameter value.
def _get_query(req, key, default_val):
    q = req.get("query")
    if q == None:
        return default_val
    val = q.get(key, default_val)
    if val == None:
        return default_val
    return val

# _paginate slices a list using startAt/maxResults query params.
# Returns (sliced_list, startAt, maxResults, total).
def _paginate(req, docs):
    start_at = _to_int(_get_query(req, "startAt", "0"))
    max_results = _to_int(_get_query(req, "maxResults", "50"))
    if max_results <= 0:
        max_results = 50
    total = len(docs)
    end = start_at + max_results
    if end > total:
        end = total
    if start_at > total:
        start_at = total
    return docs[start_at:end], start_at, max_results, total

# Transition definitions — standard Jira workflow.
# ID 11: To Do, ID 21: In Progress, ID 31: Done, ID 41: Reopened
# Order: In Progress comes first so newly-created issues (status "To Do")
# get a status-changing transition when picking the first one.
_TRANSITIONS = [
    {"id": "21", "name": "In Progress", "to": {"id": "21", "name": "In Progress"}},
    {"id": "31", "name": "Done", "to": {"id": "31", "name": "Done"}},
    {"id": "41", "name": "Reopened", "to": {"id": "41", "name": "Reopened"}},
    {"id": "11", "name": "To Do", "to": {"id": "11", "name": "To Do"}},
]

# _transition_name_by_id returns the status name for a transition ID.
def _transition_name_by_id(trans_id):
    for t in _TRANSITIONS:
        if t["id"] == trans_id:
            return t["name"]
    return ""
