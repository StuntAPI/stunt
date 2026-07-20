# Users + Organizations + Groups + Views + Triggers handlers.
#
# GET /api/v2/users          -> {users:[{id, name, email, role, active}]}
# GET /api/v2/organizations  -> {organizations:[...]}
# GET /api/v2/groups         -> {groups:[...]}
# GET /api/v2/views          -> {views:[...]}
# GET /api/v2/triggers       -> {triggers:[...]}

# Shared helpers from lib.star.

def on_list_users(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("users")
    docs = col.list()

    users = []
    for d in docs:
        users.append({
            "id": d.get("id", ""),
            "name": d.get("name", ""),
            "email": d.get("email", ""),
            "role": d.get("role", "end-user"),
            "active": d.get("active", True),
        })

    return respond(200, {"users": users})

def on_list_organizations(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("organizations")
    docs = col.list()

    orgs = []
    for d in docs:
        orgs.append({
            "id": d.get("id", ""),
            "name": d.get("name", ""),
            "domain": d.get("domain", ""),
            "details": d.get("details", ""),
            "created_at": d.get("created_at", _now()),
        })

    return respond(200, {"organizations": orgs})

def on_list_groups(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("groups")
    docs = col.list()

    groups = []
    for d in docs:
        groups.append({
            "id": d.get("id", ""),
            "name": d.get("name", ""),
            "description": d.get("description", ""),
            "default": d.get("default", False),
        })

    return respond(200, {"groups": groups})

def on_list_views(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    return respond(200, {"views": [
        {"id": "1", "title": "Unassigned tickets", "active": True, "position": 1},
        {"id": "2", "title": "Recently updated", "active": True, "position": 2},
        {"id": "3", "title": "My assigned tickets", "active": True, "position": 3},
    ]})

def on_list_triggers(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    return respond(200, {"triggers": [
        {"id": "1", "title": "Notify assignee of assignment", "active": True},
        {"id": "2", "title": "Auto-close resolved tickets after 4 days", "active": True},
        {"id": "3", "title": "Escalate priority tickets", "active": False},
    ]})
