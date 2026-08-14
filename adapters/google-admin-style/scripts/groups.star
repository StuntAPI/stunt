# Directory API Groups handlers — CRUD + member listing.
#
# Groups use email as the groupKey, and members reference user emails.

# Shared helpers (_bearer, _require_bearer, _contains, _to_int, _pad10) are
# preloaded from scripts/lib.star.

# on_list_groups returns all groups in the directory.
# GET /admin/directory/v1/groups (Bearer)
# Optional query: ?domain=, ?userKey=, ?query=, ?orderBy=, ?sortOrder= (see
# _apply_group_filters), plus maxResults/pageToken paging.
def on_list_groups(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    gc = store_collection("groups")
    docs = gc.list()

    if len(docs) == 0:
        _seed_groups()
        gc = store_collection("groups")
        docs = gc.list()

    groups = []
    for d in docs:
        groups.append(_group_entity(d))

    groups = _apply_group_filters(req, groups)
    page, next_token = _list_page(req, groups)
    result = {
        "kind": "admin#directory#groups",
        "groups": page,
    }
    if next_token != None:
        result["nextPageToken"] = next_token
    return respond(200, result)

# on_create_group creates a new group.
# POST /admin/directory/v1/groups (Bearer)
def on_create_group(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    email = body.get("email", "")
    if email == "":
        return respond(400, {
            "error": {
                "code": 400,
                "message": "email is required",
                "errors": [{
                    "message": "email is required",
                    "domain": "global",
                    "reason": "invalid",
                }],
            },
        })

    # Check for duplicate.
    gc = store_collection("groups")
    docs = gc.list()
    for d in docs:
        if d.get("email", "") == email:
            return respond(409, {
                "error": {
                    "code": 409,
                    "message": "Group already exists.",
                    "errors": [{
                        "message": "Group already exists.",
                        "domain": "global",
                        "reason": "duplicate",
                    }],
                },
            })

    group_doc = {
        "id": "group-" + str(store_kv_incr("gadmin", "group_seq")),
        "email": email,
        "name": body.get("name", email),
        "description": body.get("description", ""),
        "adminCreated": True,
        "directMembersCount": "0",
        "kind": "admin#directory#group",
    }

    gc.insert(group_doc)

    return respond(200, _group_entity(group_doc))

# on_get_group returns a group by email or id.
# GET /admin/directory/v1/groups/{groupKey} (Bearer)
def on_get_group(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    group_key = req["params"].get("groupKey", "")
    doc = _find_group(group_key)
    if doc == None:
        return respond(404, _not_found("Group", group_key))

    return respond(200, _group_entity(doc))

# on_delete_group deletes a group by email or id.
# DELETE /admin/directory/v1/groups/{groupKey} (Bearer)
def on_delete_group(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    group_key = req["params"].get("groupKey", "")
    doc = _find_group(group_key)
    if doc == None:
        return respond(404, _not_found("Group", group_key))

    gc = store_collection("groups")
    gc.delete(doc["id"])

    return respond(204, None)

# on_list_members returns all members of a group.
# GET /admin/directory/v1/groups/{groupKey}/members (Bearer)
# Optional query: ?roles=OWNER,MANAGER (comma-separated role filter, see
# _apply_member_filters), plus maxResults/pageToken paging.
def on_list_members(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    group_key = req["params"].get("groupKey", "")
    doc = _find_group(group_key)
    if doc == None:
        return respond(404, _not_found("Group", group_key))

    mc = store_collection("members")
    docs = mc.list()
    members = []
    for d in docs:
        if d.get("groupKey", "") == doc["email"]:
            members.append({
                "kind": "admin#directory#member",
                "id": d["id"],
                "email": d["email"],
                "role": d.get("role", "MEMBER"),
                "type": d.get("type", "USER"),
                "status": "ACTIVE",
            })

    members = _apply_member_filters(req, members)
    page, next_token = _list_page(req, members)
    result = {
        "kind": "admin#directory#members",
        "members": page,
    }
    if next_token != None:
        result["nextPageToken"] = next_token
    return respond(200, result)

# on_add_member adds a member to a group.
# POST /admin/directory/v1/groups/{groupKey}/members (Bearer)
def on_add_member(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    group_key = req["params"].get("groupKey", "")
    doc = _find_group(group_key)
    if doc == None:
        return respond(404, _not_found("Group", group_key))

    body = req["body"]
    if body == None:
        body = {}

    email = body.get("email", "")
    if email == "":
        return respond(400, _invalid("email is required"))

    member_doc = {
        "id": "member-" + str(store_kv_incr("gadmin", "member_seq")),
        "groupKey": doc["email"],
        "email": email,
        "role": body.get("role", "MEMBER"),
        "type": body.get("type", "USER"),
    }

    mc = store_collection("members")
    mc.insert(member_doc)

    return respond(200, {
        "kind": "admin#directory#member",
        "id": member_doc["id"],
        "email": email,
        "role": member_doc["role"],
        "type": member_doc["type"],
        "status": "ACTIVE",
    })

# on_delete_member removes a member from a group.
# DELETE /admin/directory/v1/groups/{groupKey}/members/{memberKey} (Bearer)
def on_delete_member(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    group_key = req["params"].get("groupKey", "")
    doc = _find_group(group_key)
    if doc == None:
        return respond(404, _not_found("Group", group_key))

    member_key = req["params"].get("memberKey", "")
    mc = store_collection("members")
    docs = mc.list()
    member_doc = None
    for d in docs:
        if d.get("groupKey", "") == doc["email"] and (d["id"] == member_key or d.get("email", "") == member_key):
            member_doc = d
            break
    if member_doc == None:
        return respond(404, _not_found("Member", member_key))

    mc.delete(member_doc["id"])

    return respond(204, None)

# --- helpers ---

# _apply_group_filters maps the real Directory groups.list query params to
# query_select clauses, applied before paging like the real API:
#   domain    -> group email domain suffix
#   userKey   -> only groups the given user (member email or member id) is in
#   query     -> the documented search forms, space-separated and AND'ed:
#                "email=<exact>", "email:<prefix>*", "name=<exact>",
#                "name:<prefix>*", "memberKey=<member email or id>".
#                Text matching is case-insensitive, like Directory queries;
#                unrecognized forms are silently ignored, like Directory.
#   orderBy   -> email
#   sortOrder -> ASCENDING (default) | DESCENDING
def _apply_group_filters(req, groups):
    f = []

    domain = _query_get(req, "domain", "")
    if domain != "":
        f.append(["email", "endswith", "@" + domain])

    user_key = _query_get(req, "userKey", "")
    if user_key != "":
        f.append(["email", "in", _groups_of_member(user_key)])

    query = _query_get(req, "query", "")
    if query != "":
        for part in _split_query_clauses(query):
            groups = _apply_group_query_clause(part, groups, f)

    flt = None
    if len(f) > 0:
        flt = f

    order_by = ""
    if _query_get(req, "orderBy", "") == "email":
        order_by = "email"

    order_dir = ""
    if _query_get(req, "sortOrder", "").upper() == "DESCENDING":
        order_dir = "desc"

    return query_select(groups, flt, order_by, order_dir)

# _split_query_clauses splits a groups search query on spaces, ignoring
# spaces inside single-quoted values (the documented form for values with
# spaces, e.g. name='All Staff').
def _split_query_clauses(q):
    parts = []
    cur = ""
    inq = False
    for i in range(len(q)):
        ch = q[i]
        if ch == "'":
            inq = not inq
            cur = cur + ch
        elif ch == " " and not inq:
            if cur != "":
                parts.append(cur)
                cur = ""
        else:
            cur = cur + ch
    if cur != "":
        parts.append(cur)
    return parts

# _unquote strips one pair of surrounding single quotes, if present.
def _unquote(term):
    if len(term) >= 2 and term[0] == "'" and term[len(term) - 1] == "'":
        return term[1:len(term) - 1]
    return term

# _groups_of_member returns the emails of the groups containing the given
# member (by member email or member id).
def _groups_of_member(member):
    group_emails = []
    mc = store_collection("members")
    for d in mc.list():
        if d.get("email", "") == member or d.get("id", "") == member:
            group_emails.append(d.get("groupKey", ""))
    return group_emails

# _apply_group_query_clause applies one space-separated query clause to the
# groups list. Exact/prefix forms narrow `groups` manually (Directory text
# matching is case-insensitive); memberKey= defers to an email "in" clause in
# f. Unknown forms are ignored, like the real Directory API.
def _apply_group_query_clause(part, groups, f):
    part = part.strip()
    if part == "":
        return groups
    low = part.lower()
    if low[:6] == "email=":
        term = _unquote(part[6:].strip())
        if term != "":
            f.append(["email", "=", term])
    elif low[:6] == "email:":
        term = part[6:].strip()
        if len(term) > 1 and term[len(term) - 1] == "*":
            groups = _ci_startswith(groups, "email", _unquote(term[:len(term) - 1]))
    elif low[:5] == "name=":
        term = _unquote(part[5:].strip())
        if term != "":
            groups = _ci_exact(groups, "name", term)
    elif low[:5] == "name:":
        term = part[5:].strip()
        if len(term) > 1 and term[len(term) - 1] == "*":
            groups = _ci_startswith(groups, "name", _unquote(term[:len(term) - 1]))
        elif term != "":
            # Bare name: keeps the pre-existing case-insensitive substring
            # behavior for clients that relied on it.
            tl = _unquote(term).lower()
            kept = []
            for g in groups:
                if str(g.get("name", "")).lower().find(tl) >= 0:
                    kept.append(g)
            groups = kept
    elif low[:10] == "memberkey=":
        term = _unquote(part[10:].strip())
        if term != "":
            f.append(["email", "in", _groups_of_member(term)])
    return groups

# _ci_startswith keeps only the dicts whose field starts with prefix,
# case-insensitively.
def _ci_startswith(items, field, prefix):
    pl = prefix.lower()
    out = []
    for it in items:
        v = it.get(field, None)
        if v != None and str(v).lower()[:len(pl)] == pl:
            out.append(it)
    return out

# _ci_exact keeps only the dicts whose field equals term, case-insensitively.
def _ci_exact(items, field, term):
    tl = term.lower()
    out = []
    for it in items:
        v = it.get(field, None)
        if v != None and str(v).lower() == tl:
            out.append(it)
    return out

# _apply_member_filters maps the real Directory members.list roles param to a
# query_select clause, applied before paging like the real API:
#   roles -> comma-separated role filter (OWNER, MANAGER, MEMBER)
def _apply_member_filters(req, members):
    roles = _query_get(req, "roles", "")
    if roles == "":
        return members
    wanted = []
    for part in roles.split(","):
        part = part.strip().upper()
        if part != "":
            wanted.append(part)
    if len(wanted) == 0:
        return members
    return query_select(members, [["role", "in", wanted]])

def _find_group(key):
    gc = store_collection("groups")
    docs = gc.list()
    for d in docs:
        if d["id"] == key or d.get("email", "") == key:
            return d
    return None

def _group_entity(d):
    return {
        "kind": "admin#directory#group",
        "id": d["id"],
        "email": d["email"],
        "name": d.get("name", ""),
        "description": d.get("description", ""),
        "adminCreated": d.get("adminCreated", True),
        "directMembersCount": d.get("directMembersCount", "0"),
    }

def _invalid(msg):
    return {
        "error": {
            "code": 400,
            "message": msg,
            "errors": [{
                "message": msg,
                "domain": "global",
                "reason": "invalid",
            }],
        },
    }

def _seed_groups():
    gc = store_collection("groups")
    groups = [
        {
            "id": "group-001",
            "email": "engineering@mock-domain.com",
            "name": "Engineering Team",
            "description": "All engineering staff",
            "adminCreated": True,
            "directMembersCount": "2",
        },
        {
            "id": "group-002",
            "email": "all-staff@mock-domain.com",
            "name": "All Staff",
            "description": "Everyone in the organization",
            "adminCreated": True,
            "directMembersCount": "3",
        },
    ]
    for g in groups:
        gc.insert(g)

    mc = store_collection("members")
    members = [
        {"id": "member-001", "groupKey": "engineering@mock-domain.com", "email": "alice@mock-domain.com", "role": "MEMBER", "type": "USER"},
        {"id": "member-002", "groupKey": "engineering@mock-domain.com", "email": "admin@mock-domain.com", "role": "MANAGER", "type": "USER"},
        {"id": "member-003", "groupKey": "all-staff@mock-domain.com", "email": "admin@mock-domain.com", "role": "OWNER", "type": "USER"},
        {"id": "member-004", "groupKey": "all-staff@mock-domain.com", "email": "alice@mock-domain.com", "role": "MEMBER", "type": "USER"},
    ]
    for m in members:
        mc.insert(m)
