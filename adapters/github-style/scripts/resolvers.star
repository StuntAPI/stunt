# GitHub GraphQL resolvers — served by the engine's real GraphQL executor
# at POST /graphql (see adapter.yaml).
#
# Root fields use on_<field>(callArg); object fields use
# resolve_<Type>_<field>(callArg). Scalar fields fall back to the default
# resolver (parent[fieldName]). Objects map onto the same collections as the
# REST surface (issues/pulls/comments/...) with GitHub's base64 global IDs
# (base64("04:Issue<number>")), and mutations share the REST state machine
# (issue numbers, state validation, webhook emission).
#
# All data is synthetic.

# The one repository this adapter serves (REST surface's default repo).
_REPO_OWNER = "octocat"
_REPO_NAME = "hello-world"
_REPO_KEY = "octocat/hello-world"
# Numeric repository id, assembled at runtime (matches the REST surface;
# no literal digit run longer than four digits).
_REPO_NUM = (12 * 100 + 96) * 1000 + 269

# ---------------------------------------------------------------------------
# Global-ID helpers (GitHub's base64 "NN:Type<key>" convention)
# ---------------------------------------------------------------------------

# _gid renders a GitHub global id from its typed prefix + key.
def _gid(prefix, key):
    return crypto.base64_encode(prefix + str(key))

# _gid_key decodes a global id back to its key ("123" from
# base64("04:Issue123")), or None when it does not decode/match.
def _gid_key(gid, prefix):
    if gid == None:
        return None
    decoded = crypto.base64_decode(gid)
    if decoded == None or decoded == "":
        return None
    if not decoded.startswith(prefix):
        return None
    key = decoded[len(prefix):]
    if key == "":
        return None
    return key

def _repo_gid():
    return crypto.base64_encode("010:Repository" + str(_REPO_NUM))

# _num_key renders a stored numeric field as a clean decimal string for a
# gid key (docs round-trip through JSON, so ints come back as floats).
def _num_key(v):
    if v == None:
        return "0"
    if type(v) == "int":
        return str(v)
    if type(v) == "float":
        return str(int(v))
    return str(v)

# ---------------------------------------------------------------------------
# Shared helpers
# ---------------------------------------------------------------------------

# _int_arg coerces an Int argument to int (variables arrive as JSON floats).
def _int_arg(v):
    if v == None:
        return None
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(str(v))

# _as_list normalizes a list-typed argument: a bare literal (e.g. the single
# enum `states: OPEN`) arrives as a scalar, not a one-element list.
def _as_list(v):
    if v == None:
        return None
    if type(v) == "list":
        return v
    return [v]

# _connection builds a GitHub connection object with offset cursors.
def _connection(items, first, after):
    total = len(items)
    offset = 0
    if after != None and after != "":
        offset = _to_int(after)
    page = query_select(items, None, None, "", _int_arg(first), offset, None)
    end = offset + len(page)

    edges = []
    for it in page:
        edges.append({"node": it, "cursor": str(offset + len(edges) + 1)})

    return {
        "edges": edges,
        "nodes": page,
        "pageInfo": {
            "hasNextPage": end < total,
            "hasPreviousPage": offset > 0,
            "startCursor": str(offset + 1) if len(page) > 0 else None,
            "endCursor": str(end) if len(page) > 0 else None,
        },
    }

# _actor projects a stored user dict into an Actor value: the __typename key
# selects the interface's runtime type (User vs Bot), exactly like GitHub's
# Actor interface.
def _actor_value(user):
    if user == None:
        return None
    t = user.get("type", "User")
    if t != "User" and t != "Bot":
        t = "User"
    return {
        "__typename": t,
        "login": user.get("login", ""),
        "id": user.get("id", 0),
        "url": "https://github.com/" + user.get("login", ""),
    }

# _issue_order maps an IssueOrder input to a (field, direction) pair.
def _issue_order(order_by, default_field, default_dir):
    field = default_field
    direction = default_dir
    if order_by != None and type(order_by) == "dict":
        f = order_by.get("field", None)
        if f == "UPDATED_AT":
            field = "updated_at"
        else:
            field = "created_at"
        d = order_by.get("direction", None)
        if d != None:
            direction = _lower(d)
    return field, direction

# _lower lowercases an ASCII string.
def _lower(s):
    if s == None:
        return ""
    out = ""
    for i in range(len(s)):
        ch = s[i]
        if ch >= "A" and ch <= "Z":
            out = out + chr(ord(ch) + 32)
        else:
            out = out + ch
    return out

# _issue_doc returns the stored issue doc for a number, or None.
def _issue_doc(number):
    ic = store_collection("issues")
    return _find_doc(ic, _REPO_KEY, number)

# _pull_doc returns the stored PR doc for a number, or None.
def _pull_doc(number):
    pc = store_collection("pulls")
    return _find_doc(pc, _REPO_KEY, number)

# ---------------------------------------------------------------------------
# Query root resolvers
# ---------------------------------------------------------------------------

# viewer → Viewer (the synthetic bot identity, like the REST actor).
def on_viewer(args):
    return respond(200, {
        "__typename": "Bot",
        "login": "stunt-dev",
        "id": _BOT_ID,
        "name": "Stunt Dev Bot",
        "url": "https://github.com/stunt-dev",
    })

# repository(owner, name) → Repository | None (only the default repo exists,
# matching the REST surface's 404 for other repos).
def on_repository(args):
    _seed()
    a = args["args"]
    if a.get("owner", "") != _REPO_OWNER or a.get("name", "") != _REPO_NAME:
        return respond(200, None)
    return respond(200, {
        "id": _repo_gid(),
        "name": _REPO_NAME,
        "nameWithOwner": _REPO_KEY,
        "description": "Synthetic repo for local testing",
        "isPrivate": False,
        "defaultBranchRef": {"name": "main", "prefix": "refs/heads/"},
        "createdAt": _now(),
        "updatedAt": _now(),
    })

# ---------------------------------------------------------------------------
# Repository object resolvers
# ---------------------------------------------------------------------------

# Repository.issues(first, after, orderBy, filterBy, states) → IssueConnection
def resolve_Repository_issues(args):
    a = args["args"]
    docs = []
    for i in store_collection("issues").list():
        if i.get("repo", "") != _REPO_KEY:
            continue
        if not _issue_matches(i, _as_list(a.get("states")), a.get("filterBy")):
            continue
        docs.append(i)
    field, direction = _issue_order(a.get("orderBy"), "created_at", "desc")
    docs = query_select(docs, None, field, direction, None, None, None)
    conn = _connection(docs, a.get("first"), a.get("after"))
    conn["totalCount"] = len(docs)
    return respond(200, conn)

# Repository.issue(number) → Issue | None
def resolve_Repository_issue(args):
    return respond(200, _issue_doc(_int_arg(args["args"].get("number"))))

# Repository.pullRequests(first, after, orderBy, states) → PullRequestConnection
def resolve_Repository_pullRequests(args):
    a = args["args"]
    wanted = _as_list(a.get("states"))
    docs = []
    for p in store_collection("pulls").list():
        if p.get("repo", "") != _REPO_KEY:
            continue
        if wanted != None and len(wanted) > 0:
            if _pull_state(p) not in wanted:
                continue
        docs.append(p)
    field, direction = _issue_order(a.get("orderBy"), "created_at", "desc")
    docs = query_select(docs, None, field, direction, None, None, None)
    conn = _connection(docs, a.get("first"), a.get("after"))
    conn["totalCount"] = len(docs)
    return respond(200, conn)

# Repository.pullRequest(number) → PullRequest | None
def resolve_Repository_pullRequest(args):
    return respond(200, _pull_doc(_int_arg(args["args"].get("number"))))

# ---------------------------------------------------------------------------
# Issue object resolvers
# ---------------------------------------------------------------------------

def resolve_Issue_id(args):
    return respond(200, _gid("04:Issue", _num_key(args["parent"].get("number", 0))))

def resolve_Issue_state(args):
    return respond(200, str(args["parent"].get("state", "open")).upper())

def resolve_Issue_stateReason(args):
    v = args["parent"].get("state_reason", None)
    if v == None or v == "":
        return respond(200, None)
    return respond(200, str(v).upper())

def resolve_Issue_closedAt(args):
    return respond(200, args["parent"].get("closed_at", None))

def resolve_Issue_author(args):
    return respond(200, _actor_value(args["parent"].get("user", None)))

def resolve_Issue_createdAt(args):
    return respond(200, args["parent"].get("created_at", ""))

def resolve_Issue_updatedAt(args):
    return respond(200, args["parent"].get("updated_at", ""))

def resolve_Issue_url(args):
    p = args["parent"]
    return respond(200, "https://github.com/" + p.get("repo", _REPO_KEY) + "/issues/" + str(p.get("number", 0)))

def resolve_Issue_labels(args):
    a = args["args"]
    labels = args["parent"].get("labels", [])
    if labels == None:
        labels = []
    return respond(200, _connection(labels, a.get("first"), a.get("after")))

def resolve_Issue_comments(args):
    a = args["args"]
    p = args["parent"]
    docs = []
    for c in store_collection("comments").list():
        if c.get("repo", "") == p.get("repo", "") and c.get("number", 0) == p.get("number", 0):
            docs.append(c)
    docs = query_select(docs, None, "created_at", "asc", None, None, None)
    conn = _connection(docs, a.get("first"), a.get("after"))
    conn["totalCount"] = len(docs)
    return respond(200, conn)

# ---------------------------------------------------------------------------
# PullRequest object resolvers
# ---------------------------------------------------------------------------

def resolve_PullRequest_id(args):
    return respond(200, _gid("04:PullRequest", _num_key(args["parent"].get("number", 0))))

def resolve_PullRequest_state(args):
    return respond(200, _pull_state(args["parent"]))

def resolve_PullRequest_merged(args):
    return respond(200, bool(args["parent"].get("merged", False)))

def resolve_PullRequest_mergedAt(args):
    return respond(200, args["parent"].get("merged_at", None))

def resolve_PullRequest_closedAt(args):
    return respond(200, args["parent"].get("closed_at", None))

def resolve_PullRequest_baseRefName(args):
    head = args["parent"].get("base", None)
    if head == None:
        return respond(200, "main")
    return respond(200, head.get("ref", "main"))

def resolve_PullRequest_headRefName(args):
    head = args["parent"].get("head", None)
    if head == None:
        return respond(200, "main")
    return respond(200, head.get("ref", "main"))

def resolve_PullRequest_author(args):
    return respond(200, _actor_value(args["parent"].get("user", None)))

def resolve_PullRequest_createdAt(args):
    return respond(200, args["parent"].get("created_at", ""))

def resolve_PullRequest_updatedAt(args):
    return respond(200, args["parent"].get("updated_at", ""))

def resolve_PullRequest_url(args):
    p = args["parent"]
    return respond(200, "https://github.com/" + p.get("repo", _REPO_KEY) + "/pull/" + str(p.get("number", 0)))

# ---------------------------------------------------------------------------
# Label / IssueComment object resolvers
# ---------------------------------------------------------------------------

def resolve_Label_id(args):
    return respond(200, _gid("05:Label", args["parent"].get("name", "")))

def resolve_IssueComment_id(args):
    return respond(200, _gid("04:IssueComment", args["parent"].get("id", "")))

def resolve_IssueComment_author(args):
    return respond(200, _actor_value(args["parent"].get("user", None)))

def resolve_IssueComment_createdAt(args):
    return respond(200, args["parent"].get("created_at", ""))

def resolve_IssueComment_updatedAt(args):
    return respond(200, args["parent"].get("updated_at", ""))

def resolve_IssueComment_url(args):
    p = args["parent"]
    return respond(200, "https://github.com/" + p.get("repo", _REPO_KEY) + "/issues/" + str(p.get("number", 0)) + "#issuecomment-" + str(p.get("id", "")))

# ---------------------------------------------------------------------------
# Mutation root resolvers
# ---------------------------------------------------------------------------

# createIssue(input {repositoryId, title, body, labels}) → CreateIssuePayload.
# Mirrors the REST create: repo-scoped sequential numbers, label projection,
# and the signed issues webhook.
def on_createIssue(args):
    _seed()
    input = args["args"].get("input")
    if input == None:
        input = {}

    if input.get("repositoryId", "") != _repo_gid():
        fail("Could not resolve to a Repository with the global id of '" + str(input.get("repositoryId", "")) + "'")

    title = input.get("title", "")
    if title == None or str(title).strip() == "":
        fail("title is required")

    num = _seed_issue_number(_REPO_OWNER, _REPO_NAME)
    labels_in = input.get("labels", [])
    if labels_in == None:
        labels_in = []
    labels = []
    for l in labels_in:
        labels.append({"name": l, "color": "ededed"})

    issue = {
        "id": _next_id("issue_id"),
        "number": num,
        "repo": _REPO_KEY,
        "title": str(title),
        "body": input.get("body", "") or "",
        "state": "open",
        "user": _actor(),
        "labels": labels,
        "created_at": _now(),
        "updated_at": _now(),
    }
    store_collection("issues").insert(issue)

    _emit_if_subscribed(_REPO_KEY, "issues", _gh_event_payload(_REPO_KEY, "opened", "issue", _issue_rest_view(issue)))
    return respond(200, {"issue": issue})

# updateIssue(input {id, title?, body?, state?, stateReason?}) mirrors the
# REST PATCH: state/state_reason validation, closed_at bookkeeping, issue
# events, and the signed webhook.
def on_updateIssue(args):
    _seed()
    input = args["args"].get("input")
    if input == None:
        input = {}

    key = _gid_key(input.get("id", ""), "04:Issue")
    if key == None:
        fail("Could not resolve to an Issue with the global id of '" + str(input.get("id", "")) + "'")
    doc = _issue_doc(_int_arg(key))
    if doc == None:
        fail("Could not resolve to an Issue with the global id of '" + str(input.get("id", "")) + "'")

    state = input.get("state", None)
    state_lower = None
    if state != None:
        state_lower = _lower(state)
        if state_lower != "open" and state_lower != "closed":
            fail("state is not a valid IssueState")
    state_reason = input.get("stateReason", None)
    reason_lower = None
    if state_reason != None:
        reason_lower = _lower(state_reason)
        if reason_lower not in ["completed", "not_planned", "reopened"]:
            fail("stateReason is not a valid IssueStateReason")

    prev_state = doc.get("state", "open")
    if state_lower != None:
        doc["state"] = state_lower
    if reason_lower != None:
        doc["state_reason"] = reason_lower
    if state_lower == "closed" and prev_state != "closed":
        doc["closed_at"] = _now()
    if state_lower == "open" and prev_state != "open":
        doc["closed_at"] = None
    if input.get("title", None) != None:
        doc["title"] = input["title"]
    if input.get("body", None) != None:
        doc["body"] = input["body"]
    doc["updated_at"] = _now()
    store_collection("issues").update(doc["id"], doc)

    action = "edited"
    if prev_state != doc.get("state", prev_state):
        if doc.get("state", "") == "closed":
            action = "closed"
        else:
            action = "reopened"
        _record_issue_event(_REPO_KEY, doc.get("number", 0), action)
    _emit_if_subscribed(_REPO_KEY, "issues", _gh_event_payload(_REPO_KEY, action, "issue", _issue_rest_view(doc)))
    return respond(200, {"issue": doc})

# addComment(input {subjectId, body}) mirrors the REST comment create: the
# subject may be an issue or a PR (shared number space), and the signed
# issue_comment webhook carries both.
def on_addComment(args):
    _seed()
    input = args["args"].get("input")
    if input == None:
        input = {}

    body = input.get("body", "")
    if body == None or str(body).strip() == "":
        fail("body is required")

    subject = None
    subject_key = "issue"
    key = _gid_key(input.get("subjectId", ""), "04:Issue")
    if key != None:
        subject = _issue_doc(_int_arg(key))
    else:
        key = _gid_key(input.get("subjectId", ""), "04:PullRequest")
        if key != None:
            subject = _pull_doc(_int_arg(key))
            subject_key = "pull_request"
    if subject == None:
        fail("Could not resolve to an Issue or PullRequest with the global id of '" + str(input.get("subjectId", "")) + "'")

    number = subject.get("number", 0)
    comment = {
        "id": _next_id("comment_id"),
        "repo": _REPO_KEY,
        "number": number,
        "body": str(body),
        "user": _actor(),
        "created_at": _now(),
        "updated_at": _now(),
    }
    store_collection("comments").insert(comment)

    payload = _gh_event_payload(_REPO_KEY, "created", "issue", subject)
    payload["comment"] = comment
    _emit_if_subscribed(_REPO_KEY, "issue_comment", payload)
    return respond(200, {
        "commentEdge": {"node": comment},
        "subject": subject,
    })

# ---------------------------------------------------------------------------
# Local helpers
# ---------------------------------------------------------------------------

# _issue_rest_view renders the REST issue shape for webhook payloads (the
# same projection issues.star emits, replicated here because handler-script
# globals are not visible across scripts).
def _issue_rest_view(i):
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

# _pull_state derives the PullRequestState enum from the stored doc
# (GitHub reports MERGED for merged PRs regardless of the REST state field).
def _pull_state(p):
    if p.get("merged", False):
        return "MERGED"
    return str(p.get("state", "open")).upper()

# _issue_matches applies the states + filterBy arguments to an issue doc.
def _issue_matches(doc, states, filter_by):
    if states != None and len(states) > 0:
        wanted = []
        for s in states:
            wanted.append(_lower(s))
        if _lower(doc.get("state", "open")) not in wanted:
            return False
    if filter_by == None or type(filter_by) != "dict":
        return True
    labels = _as_list(filter_by.get("labels", None))
    if labels != None and len(labels) > 0:
        names = []
        for l in doc.get("labels", []):
            names.append(l.get("name", ""))
        for want in labels:
            if want not in names:
                return False
    created_by = filter_by.get("createdBy", None)
    if created_by != None:
        if doc.get("user", {}).get("login", "") != created_by:
            return False
    since = filter_by.get("since", None)
    if since != None:
        if doc.get("updated_at", "") < str(since):
            return False
    return True
