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

# _field_error returns Jira's per-field 400 shape (empty errorMessages, the
# rejected fields keyed in "errors" with the real "cannot be set" wording).
def _field_error(field_errors):
    return respond(400, {
        "errorMessages": [],
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

def _int_to_str(n):
    digits = "0123456789"
    if n == 0:
        return "0"
    s = ""
    v = n
    while v > 0:
        s = digits[v % 10] + s
        v = v // 10
    return s

# _next_issue_id returns a monotonically-increasing numeric issue ID.
def _next_issue_id():
    n = store_kv_incr("jira", "issue_seq")
    return _int_to_str(1000 * 10 + n)

# _next_comment_id returns a monotonically-increasing comment ID.
def _next_comment_id():
    n = store_kv_incr("jira", "comment_seq")
    return _int_to_str(1000 * 10 + n)

# ============================================================================
# JIRA WEBHOOK DELIVERY (DOCUMENTATION)
# ============================================================================
# Jira Cloud webhooks (registered via POST /rest/api/3/webhook) are
# UNSIGNED BY DESIGN: Atlassian documents no HMAC/signature header for
# webhook deliveries. The payload envelope is:
#
#   {"timestamp": <ms>, "webhookEvent": "jira:issue_created", "issue": {...}}
#
# (comment events add a "comment" object). Secure the receiving endpoint via a
# secret token in the URL or basic auth on the target — stunt does NOT invent
# a signature. _emit_webhook delivers only when a registered hook subscribes
# to the event type (a hook with an empty events list receives everything).
def _emit_webhook(event_type, payload):
    wc = store_collection("webhooks")
    for w in wc.list():
        events = w.get("events", [])
        if events == None:
            events = []
        if len(events) == 0 or event_type in events:
            events_emit(event_type, payload)
            return

# _issue_event builds Jira's webhook payload envelope for issue-scoped events.
def _issue_event(event_type, doc):
    return {
        "timestamp": clock.now_unix() * 1000,
        "webhookEvent": event_type,
        "issue": {
            "id": doc.get("id", ""),
            "key": doc.get("key", ""),
            "fields": doc.get("fields", {}),
        },
    }

# _next_webhook_id returns a monotonically-increasing webhook registration ID.
def _next_webhook_id():
    n = store_kv_incr("jira", "webhook_seq")
    return _int_to_str(1000 * 10 + n)

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
_INT64_MAX = (1 << 63) - 1

def _to_int(s):
    if s == "" or s == None:
        return 0
    result = 0
    for i in range(len(s)):
        ch = s[i]
        code = ord(ch)
        if code >= 48 and code <= 57:
            result = result * 10 + (code - 48)
            if result > _INT64_MAX:
                return 0
        else:
            return 0
    return result

# ============================================================================
# JQL ENGINE (real subset)
# ============================================================================
#
# Supported grammar (a useful, faithful subset of JQL):
#
#   clause   := field OP value | field IN (v1, v2, ...) | field NOT IN (...)
#            | field IS [NOT] EMPTY | field IS [NOT] NULL
#   OP       := = | != | ~ | !~ | > | >= | < | <=
#   query    := clause (AND clause)* [OR clause (AND clause)*]*
#   tail     := ORDER BY field [ASC|DESC] [, field [ASC|DESC]]*
#
# AND binds tighter than OR (real JQL precedence), so the filter is parsed
# into a disjunction of AND-groups. String values compare case-insensitively
# (real Jira text matching); ~ is a case-insensitive "contains". EMPTY/NULL
# (and "= EMPTY") match missing/None fields. Anything that does not parse —
# unbalanced quotes, parentheses, stray operators, unknown fields — is
# rejected by the caller with a 400, like real Jira's "Error in the JQL
# Query" response. Parenthesised grouping is intentionally not supported;
# express it with explicit OR groups.

# _jql_field_path maps a JQL field name to the dotted path inside an issue
# doc (for ORDER BY via query_select). Returns "" for unknown fields.
def _jql_field_path(field):
    l = _lower(field)
    if l == "project":
        return "fields.project.key"
    if l == "status":
        return "fields.status.name"
    if l == "issuetype" or l == "type":
        return "fields.issuetype.name"
    if l == "priority":
        return "fields.priority.name"
    if l == "resolution":
        return "fields.resolution.name"
    if l == "summary":
        return "fields.summary"
    if l == "description":
        return "fields.description"
    if l == "labels":
        return "fields.labels"
    if l == "assignee":
        return "fields.assignee.accountId"
    if l == "reporter":
        return "fields.reporter.accountId"
    if l == "key" or l == "issuekey":
        return "key"
    if l == "id" or l == "issue":
        return "id"
    if l == "created" or l == "updated":
        return "fields." + l
    return ""

# _jql_field_value extracts the comparable value for a JQL field from an
# issue doc. User fields (assignee/reporter) yield a list of matchable
# strings (accountId + displayName); the pseudo-field "text" yields the
# summary+description blob. Returns None when the field is unset.
def _jql_field_value(doc, field):
    l = _lower(field)
    f = doc.get("fields", {})
    if f == None:
        f = {}
    if l == "project":
        p = f.get("project", None)
        if p == None:
            return None
        return p.get("key", "")
    if l == "status":
        s = f.get("status", None)
        if s == None:
            return None
        return s.get("name", "")
    if l == "issuetype" or l == "type":
        t = f.get("issuetype", None)
        if t == None:
            return None
        return t.get("name", "")
    if l == "priority":
        p = f.get("priority", None)
        if p == None:
            return None
        return p.get("name", "")
    if l == "resolution":
        r = f.get("resolution", None)
        if r == None:
            return None
        return r.get("name", "")
    if l == "summary" or l == "description":
        v = f.get(l, None)
        return v
    if l == "labels":
        v = f.get("labels", None)
        return v
    if l == "assignee" or l == "reporter":
        u = f.get(l, None)
        if u == None:
            return None
        return [u.get("accountId", ""), u.get("displayName", "")]
    if l == "key" or l == "issuekey":
        return doc.get("key", "")
    if l == "id" or l == "issue":
        return doc.get("id", "")
    if l == "created" or l == "updated":
        return f.get(l, None)
    if l == "text":
        s = f.get("summary", "")
        d = f.get("description", "")
        if d == None:
            d = ""
        return s + " " + d
    return None

# _jql_ci_eq compares a scalar field value to a wanted string,
# case-insensitively for strings.
def _jql_ci_eq(actual, want):
    if actual == None:
        return False
    if type(actual) == "string":
        return _lower(actual) == _lower(want)
    return str(actual) == want

# _jql_ci_list_match reports whether want matches (case-insensitively) any
# string in actual.
def _jql_ci_list_match(actual, want):
    for v in actual:
        if type(v) == "string" and _lower(v) == _lower(want):
            return True
    return False

# _jql_scalar_in reports whether the scalar value matches any wanted value
# (case-insensitive for strings).
def _jql_scalar_in(actual, want_list):
    for w in want_list:
        if _jql_ci_eq(actual, w):
            return True
    return False

# _jql_empty reports whether a field value counts as EMPTY/NULL.
def _jql_empty(v):
    if v == None:
        return True
    if type(v) == "string" and v == "":
        return True
    if type(v) == "list" and len(v) == 0:
        return True
    return False

# _jql_clause_matches evaluates one parsed clause against an issue doc.
def _jql_clause_matches(doc, clause):
    field = clause[0]
    op = clause[1]
    want = clause[2]
    lf = _lower(field)
    if want == "currentUser()":
        want = "5f1b3a4c5d6e7f8a9b0c1d2e"
    v = _jql_field_value(doc, field)

    if op == "isempty":
        return _jql_empty(v)
    if op == "isnotempty":
        return not _jql_empty(v)

    if lf == "labels":
        labels = v
        if labels == None:
            labels = []
        if op == "=":
            return _jql_ci_list_match(labels, want)
        if op == "!=":
            return not _jql_ci_list_match(labels, want)
        if op == "in":
            for w in want:
                if _jql_ci_list_match(labels, w):
                    return True
            return False
        if op == "not in":
            for w in want:
                if _jql_ci_list_match(labels, w):
                    return False
            return True
        return False

    if lf == "assignee" or lf == "reporter":
        if op == "=":
            if v == None:
                return False
            return _jql_ci_list_match(v, want)
        if op == "!=":
            if v == None:
                return True
            return not _jql_ci_list_match(v, want)
        if op == "in":
            if v == None:
                return False
            for w in want:
                if _jql_ci_list_match(v, w):
                    return True
            return False
        if op == "not in":
            if v == None:
                return True
            for w in want:
                if _jql_ci_list_match(v, w):
                    return False
            return True
        return False

    if op == "=":
        return _jql_ci_eq(v, want)
    if op == "!=":
        return not _jql_ci_eq(v, want)
    if op == "~" or op == "!~":
        matched = False
        if v != None and type(v) == "string":
            matched = _contains(_lower(v), _lower(want))
        if op == "~":
            return matched
        return not matched
    if op == "in":
        return _jql_scalar_in(v, want)
    if op == "not in":
        return not _jql_scalar_in(v, want)
    if op == ">" or op == ">=" or op == "<" or op == "<=":
        if v == None or type(v) != "string":
            return False
        if op == ">":
            return v > want
        if op == ">=":
            return v >= want
        if op == "<":
            return v < want
        return v <= want
    return False

# _jql_matches evaluates the parsed OR-of-AND-groups structure against a
# doc (an empty structure matches everything).
def _jql_matches(doc, groups):
    for g in groups:
        keep = True
        for clause in g:
            if not _jql_clause_matches(doc, clause):
                keep = False
                break
        if keep:
            return True
    return False

# _jql_tokenize splits a JQL string into [kind, text] tokens. kind is
# "word", "str" (quoted literal) or "punct". Returns None on a bad quote or
# a stray character.
def _jql_tokenize(jql):
    tokens = []
    i = 0
    n = len(jql)
    while i < n:
        ch = jql[i]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            i = i + 1
            continue
        if ch == '"' or ch == "'":
            j = i + 1
            buf = ""
            closed = False
            while j < n:
                if jql[j] == "\\" and j + 1 < n and (jql[j + 1] == ch or jql[j + 1] == "\\"):
                    buf = buf + jql[j + 1]
                    j = j + 2
                    continue
                if jql[j] == ch:
                    closed = True
                    break
                buf = buf + jql[j]
                j = j + 1
            if not closed:
                return None
            tokens.append(["str", buf])
            i = j + 1
            continue
        two = jql[i:i + 2]
        if two == "!=" or two == ">=" or two == "<=" or two == "!~":
            tokens.append(["punct", two])
            i = i + 2
            continue
        if ch == "=" or ch == "~" or ch == ">" or ch == "<" or ch == "(" or ch == ")" or ch == ",":
            tokens.append(["punct", ch])
            i = i + 1
            continue
        if ch == "!" or ch == ";" or ch == "[" or ch == "]":
            return None
        j = i
        buf = ""
        while j < n:
            c = jql[j]
            if c == " " or c == "\t" or c == "\n" or c == "\r" or c == "=" or c == "~" or c == ">" or c == "<" or c == "!" or c == "(" or c == ")" or c == "," or c == '"' or c == "'":
                break
            buf = buf + c
            j = j + 1
        if buf == "":
            return None
        tokens.append(["word", buf])
        i = j
    return tokens

# _jql_is_kw reports whether a token is the given lowercased keyword.
def _jql_is_kw(token, kw):
    return token[0] == "word" and _lower(token[1]) == kw

# _jql_parse parses a full JQL string. Returns
# {"groups": [[clause, ...], ...], "order": [[path, dir], ...]} or None when
# the query does not parse. clause is [field, op, value] with value a string
# (scalars) or list (IN / NOT IN).
def _jql_parse(jql):
    if jql == None:
        jql = ""
    tokens = _jql_tokenize(jql)
    if tokens == None:
        return None

    # Split the ORDER BY tail off.
    order_at = -1
    for t in range(len(tokens)):
        if _jql_is_kw(tokens[t], "order") and t + 1 < len(tokens) and _jql_is_kw(tokens[t + 1], "by"):
            order_at = t
            break
    where = tokens
    order_tokens = []
    if order_at >= 0:
        where = tokens[:order_at]
        order_tokens = tokens[order_at + 2:]

    # Parse ORDER BY field [ASC|DESC] {, field [ASC|DESC]}.
    order = []
    k = 0
    while k < len(order_tokens):
        tok = order_tokens[k]
        if tok[0] != "word":
            return None
        path = _jql_field_path(tok[1])
        if path == "":
            return None
        direction = "asc"
        k = k + 1
        if k < len(order_tokens) and order_tokens[k][0] == "word" and (_lower(order_tokens[k][1]) == "asc" or _lower(order_tokens[k][1]) == "desc"):
            if _lower(order_tokens[k][1]) == "desc":
                direction = "desc"
            k = k + 1
        order.append([path, direction])
        if k < len(order_tokens):
            if order_tokens[k][0] == "punct" and order_tokens[k][1] == ",":
                k = k + 1
            else:
                return None

    # Parse the WHERE part into OR-of-AND groups.
    groups = [[]]
    k = 0
    while k < len(where):
        tok = where[k]
        if tok[0] != "word":
            return None
        wl = _lower(tok[1])
        if wl == "and" or wl == "or":
            return None
        field = tok[1]
        if _jql_field_path(field) == "":
            return None
        k = k + 1
        if k >= len(where):
            return None

        nxt = where[k]
        clause = []
        if nxt[0] == "punct":
            op = nxt[1]
            if op != "=" and op != "!=" and op != "~" and op != "!~" and op != ">" and op != ">=" and op != "<" and op != "<=":
                return None
            k = k + 1
            if k >= len(where):
                return None
            if where[k][0] != "word" and where[k][0] != "str":
                return None
            value = where[k][1]
            k = k + 1
            # "= EMPTY"/"!= EMPTY" mean IS (NOT) EMPTY.
            vl = _lower(value)
            if (vl == "empty" or vl == "null") and where[k - 1][0] == "word":
                if op == "=":
                    clause = [field, "isempty", None]
                elif op == "!=":
                    clause = [field, "isnotempty", None]
                else:
                    return None
            else:
                clause = [field, op, value]
        elif nxt[0] == "word":
            nl = _lower(nxt[1])
            if nl == "in" or nl == "not":
                if nl == "not":
                    if k + 1 >= len(where) or not _jql_is_kw(where[k + 1], "in"):
                        return None
                    op = "not in"
                    k = k + 2
                else:
                    op = "in"
                    k = k + 1
                if k >= len(where) or not (where[k][0] == "punct" and where[k][1] == "("):
                    return None
                k = k + 1
                vals = []
                if k < len(where) and where[k][0] == "punct" and where[k][1] == ")":
                    k = k + 1
                else:
                    while True:
                        if k >= len(where):
                            return None
                        if where[k][0] != "word" and where[k][0] != "str":
                            return None
                        vals.append(where[k][1])
                        k = k + 1
                        if k >= len(where):
                            return None
                        if where[k][0] == "punct" and where[k][1] == ",":
                            k = k + 1
                            continue
                        if where[k][0] == "punct" and where[k][1] == ")":
                            k = k + 1
                            break
                        return None
                clause = [field, op, vals]
            elif nl == "is":
                k = k + 1
                if k >= len(where):
                    return None
                neg = False
                if _jql_is_kw(where[k], "not"):
                    neg = True
                    k = k + 1
                    if k >= len(where):
                        return None
                if where[k][0] != "word":
                    return None
                el = _lower(where[k][1])
                if el != "empty" and el != "null":
                    return None
                k = k + 1
                if neg:
                    clause = [field, "isnotempty", None]
                else:
                    clause = [field, "isempty", None]
            else:
                return None
        else:
            return None

        groups[len(groups) - 1].append(clause)

        # Connector (a trailing AND/OR with no following clause is invalid).
        if k < len(where):
            if _jql_is_kw(where[k], "and"):
                k = k + 1
                if k >= len(where):
                    return None
            elif _jql_is_kw(where[k], "or"):
                groups.append([])
                k = k + 1
                if k >= len(where):
                    return None
            else:
                return None

    return {"groups": groups, "order": order}

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

# ============================================================================
# ISSUE FIELD VALIDATION
# ============================================================================
#
# Real Jira rejects create/update requests that set a field it does not know
# (or that is not on the screen) with 400 and an entry in the "errors" object
# per bad field. Known writable fields plus any customfield_* are accepted
# and preserved; everything else is rejected.

_WRITABLE_ISSUE_FIELDS = [
    "summary",
    "description",
    "project",
    "issuetype",
    "assignee",
    "reporter",
    "priority",
    "labels",
    "components",
    "fixVersions",
    "affectedVersions",
    "versions",
    "environment",
    "duedate",
    "timetracking",
    "security",
    "parent",
]

# _field_writable reports whether an issue field may be set via the API.
def _field_writable(name):
    if name in _WRITABLE_ISSUE_FIELDS:
        return True
    if name.startswith("customfield_"):
        return True
    return False

# _validate_issue_fields checks a fields dict for unknown/unsettable fields.
# Returns the Jira errors object ({} when everything is settable).
def _validate_issue_fields(fields):
    errors = {}
    for name in fields:
        if not _field_writable(name):
            errors[name] = "Field '" + name + "' cannot be set. It is not on the appropriate screen, or unknown."
    return errors

# ============================================================================
# WORKFLOW / TRANSITIONS
# ============================================================================
#
# A fixed, workflow-constrained transition graph (a realistic Jira Software
# simplified workflow). Transitions are only available from the statuses
# listed in "from"; GET transitions returns only those available for the
# issue's CURRENT status, and POSTing a transition that is not available for
# that status fails with 400 — like the real API, where the workflow (not the
# client) decides what is possible.

_JIRA_STATUS_IDS = {
    "To Do": "11",
    "In Progress": "21",
    "Done": "31",
    "Reopened": "41",
}

# _TRANSITION_DEFS: id/name/to_status/from-statuses. Order matters: the
# In Progress transition is listed first so a fresh "To Do" issue's first
# available transition changes its status.
_TRANSITION_DEFS = [
    {"id": "21", "name": "In Progress", "to_status": "In Progress", "from": ["To Do", "Reopened"]},
    {"id": "31", "name": "Done", "to_status": "Done", "from": ["To Do", "In Progress", "Reopened"]},
    {"id": "11", "name": "Stop Progress", "to_status": "To Do", "from": ["In Progress"]},
    {"id": "41", "name": "Reopen", "to_status": "Reopened", "from": ["Done"]},
]

# _transition_def_by_id returns the transition definition for an ID, or None.
def _transition_def_by_id(trans_id):
    for t in _TRANSITION_DEFS:
        if t["id"] == trans_id:
            return t
    return None

# _allowed_transitions returns the transition definitions available from
# the given status name.
def _allowed_transitions(status_name):
    out = []
    for t in _TRANSITION_DEFS:
        if status_name in t["from"]:
            out.append(t)
    return out

# _transition_public builds the API shape of a transition definition.
def _transition_public(t):
    return {
        "id": t["id"],
        "name": t["name"],
        "to": {
            "id": _JIRA_STATUS_IDS.get(t["to_status"], ""),
            "name": t["to_status"],
        },
        "hasScreen": False,
        "isGlobal": False,
        "isInitial": False,
        "isAvailable": True,
        "isConditional": False,
        "isLooped": False,
    }

# _apply_transition computes the new fields for a transition. Returns
# (new_fields, None) on success or (None, error_message) when the transition
# ID is unknown or not allowed from the issue's current status. Entering the
# Done status sets resolution "Done"; leaving Done (Reopen) clears it, like
# the real workflow post-functions.
def _apply_transition(fields, trans_id):
    t = _transition_def_by_id(trans_id)
    if t == None:
        return None, "Transition ID is not valid: " + trans_id
    status = fields.get("status", {})
    if status == None:
        status = {}
    current = status.get("name", "")
    if current not in t["from"]:
        return None, "It isn't allowed to transition issue to the status '" + t["to_status"] + "' from '" + current + "'"

    new_fields = {}
    for k, v in fields.items():
        new_fields[k] = v
    new_fields["status"] = {
        "id": _JIRA_STATUS_IDS.get(t["to_status"], ""),
        "name": t["to_status"],
    }
    if t["to_status"] == "Done":
        new_fields["resolution"] = {"name": "Done"}
    elif current == "Done":
        new_fields["resolution"] = None
    return new_fields, None

# ============================================================================
# COMMENT VIEWS
# ============================================================================

# _comment_view builds the API shape of a stored comment, stripping the
# internal _issue linkage key.
def _comment_view(c):
    return {
        "id": c.get("id", ""),
        "body": c.get("body", ""),
        "author": c.get("author", {}),
        "created": c.get("created", ""),
        "updated": c.get("updated", ""),
        "self": c.get("self", ""),
    }
