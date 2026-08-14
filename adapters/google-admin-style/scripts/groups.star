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
#   query     -> the documented forms "email=<exact>" and "name:<substring>"
#                (name matching is case-insensitive, like Directory queries)
#   orderBy   -> email
#   sortOrder -> ASCENDING (default) | DESCENDING
def _apply_group_filters(req, groups):
    f = []

    domain = _query_get(req, "domain", "")
    if domain != "":
        f.append(["email", "endswith", "@" + domain])

    user_key = _query_get(req, "userKey", "")
    if user_key != "":
        group_emails = []
        mc = store_collection("members")
        for d in mc.list():
            if d.get("email", "") == user_key or d["id"] == user_key:
                group_emails.append(d.get("groupKey", ""))
        f.append(["email", "in", group_emails])

    query = _query_get(req, "query", "")
    if query != "":
        low = query.lower()
        if low[:6] == "email=":
            term = query[6:].strip()
            if term != "":
                f.append(["email", "=", term])
        elif low[:5] == "name:":
            term = query[5:].strip()
            if term != "":
                kept = []
                tl = term.lower()
                for g in groups:
                    if str(g.get("name", "")).lower().find(tl) >= 0:
                        kept.append(g)
                groups = kept

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
