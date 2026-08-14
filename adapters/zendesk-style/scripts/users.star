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

    # Real list params: sort/sort_order (e.g. ?sort=name&sort_order=desc),
    # applied before paging like the real API.
    users = _zd_sorted(req, users, "sort")

    page_size = _to_int(_get_query(req, "per_page", "100"))
    paged, next_cursor = _list_page(req, users)

    resp = {
        "users": paged,
        "meta": {"has_more": next_cursor != None},
    }
    if next_cursor != None:
        resp["links"] = {"next": _next_link("/api/v2/users", next_cursor, page_size)}
    else:
        resp["links"] = {"next": None}
    return respond(200, resp)

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

    # Real list params: sort/sort_order, applied before paging.
    orgs = _zd_sorted(req, orgs, "sort")

    page_size = _to_int(_get_query(req, "per_page", "100"))
    paged, next_cursor = _list_page(req, orgs)

    resp = {
        "organizations": paged,
        "meta": {"has_more": next_cursor != None},
    }
    if next_cursor != None:
        resp["links"] = {"next": _next_link("/api/v2/organizations", next_cursor, page_size)}
    else:
        resp["links"] = {"next": None}
    return respond(200, resp)

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

    # Real list params: sort/sort_order, applied before paging.
    groups = _zd_sorted(req, groups, "sort")

    page_size = _to_int(_get_query(req, "per_page", "100"))
    paged, next_cursor = _list_page(req, groups)

    resp = {
        "groups": paged,
        "meta": {"has_more": next_cursor != None},
    }
    if next_cursor != None:
        resp["links"] = {"next": _next_link("/api/v2/groups", next_cursor, page_size)}
    else:
        resp["links"] = {"next": None}
    return respond(200, resp)

def on_list_views(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    views = [
        {"id": "1", "title": "Unassigned tickets", "active": True, "position": 1},
        {"id": "2", "title": "Recently updated", "active": True, "position": 2},
        {"id": "3", "title": "My assigned tickets", "active": True, "position": 3},
    ]

    page_size = _to_int(_get_query(req, "per_page", "100"))
    paged, next_cursor = _list_page(req, views)

    resp = {
        "views": paged,
        "meta": {"has_more": next_cursor != None},
    }
    if next_cursor != None:
        resp["links"] = {"next": _next_link("/api/v2/views", next_cursor, page_size)}
    else:
        resp["links"] = {"next": None}
    return respond(200, resp)

def on_list_triggers(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    triggers = [
        {"id": "1", "title": "Notify assignee of assignment", "active": True},
        {"id": "2", "title": "Auto-close resolved tickets after 4 days", "active": True},
        {"id": "3", "title": "Escalate priority tickets", "active": False},
    ]

    # Real list params: sort/sort_order (e.g. ?sort=title), applied before
    # paging.
    triggers = _zd_sorted(req, triggers, "sort")

    page_size = _to_int(_get_query(req, "per_page", "100"))
    paged, next_cursor = _list_page(req, triggers)

    resp = {
        "triggers": paged,
        "meta": {"has_more": next_cursor != None},
    }
    if next_cursor != None:
        resp["links"] = {"next": _next_link("/api/v2/triggers", next_cursor, page_size)}
    else:
        resp["links"] = {"next": None}
    return respond(200, resp)
