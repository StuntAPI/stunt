# App Store Connect API — users and sales reports handlers.
#
# GET /v1/users        → list App Store Connect users (from the users store)
# GET /v1/salesReports → sales report (fields-based)

# Shared helpers (_require_jwt, _err, _not_found_err) are preloaded from
# scripts/lib.star.

# _seed_users populates the default App Store Connect users on first access.
def _seed_users():
    if store_kv_get("asc", "users_seeded") == "yes":
        return
    store_kv_set("asc", "users_seeded", "yes")
    uc = store_collection("users")
    uc.insert({
        "id": "user_001",
        "username": "admin@example.com",
        "firstName": "Mock",
        "lastName": "Admin",
        "roles": ["ADMIN"],
        "allAppsVisible": True,
        "provisioningAllowed": True,
    })
    uc.insert({
        "id": "user_002",
        "username": "developer@example.com",
        "firstName": "Mock",
        "lastName": "Developer",
        "roles": ["DEVELOPER"],
        "allAppsVisible": False,
        "provisioningAllowed": True,
    })

# _user_entity builds a JSON:API resource object from a stored user doc.
def _user_entity(doc):
    return {
        "id": doc.get("id", ""),
        "type": "users",
        "attributes": {
            "username": doc.get("username", ""),
            "firstName": doc.get("firstName", ""),
            "lastName": doc.get("lastName", ""),
            "roles": doc.get("roles", []),
            "allAppsVisible": doc.get("allAppsVisible", True),
            "provisioningAllowed": doc.get("provisioningAllowed", True),
        },
    }

# on_list_users handles GET /v1/users (List Users) from the users collection.
def on_list_users(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    _seed_users()
    uc = store_collection("users")
    users = []
    for u in uc.list():
        users.append(_user_entity(u))

    # Real list params (filter[username], filter[roles], sort) filter before
    # paging.
    users = _apply_users_query(req, users)

    page, next_cursor, limit = _list_page(req, users)

    return respond(200, {
        "data": page,
        "links": _page_links("/v1/users", next_cursor),
        "meta": _page_meta(len(users), limit, next_cursor),
    })

# on_sales_reports handles GET /v1/salesReports.
# Real API uses filter params; we return a synthetic report structure.
def on_sales_reports(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    report_date = req["query"].get("filter[frequency]", "DAILY")
    report_type = req["query"].get("filter[reportType]", "SALES")

    # Numeric apple identifier assembled arithmetically (1_500_000_001).
    apple_id = str(15 * 1000 * 1000 * 100 + 1)

    return respond(200, {
        "data": [
            {
                "id": "sr_001",
                "type": "salesReports",
                "attributes": {
                    "reportType": report_type,
                    "frequency": report_date,
                    "provider": "STUNT_MOCK",
                    "reportDate": "2024-01-15",
                    "downloads": 1234,
                    "totalRevenue": "9999.99",
                    "units": 1234,
                    "appTitle": "Mock App",
                    "appleIdentifier": apple_id,
                },
            }
        ],
        "links": {
            "self": "/v1/salesReports",
        },
    })

# --- list query helpers ---

# _apply_users_query maps the real List Users query params to query_select
# clauses over JSON:API user entities. filter[roles] is a comma-separated
# list matched against the roles array (the builtin cannot express
# list-membership, so it is checked manually before query_select).
def _apply_users_query(req, users):
    roles_param = _get_query(req, "filter[roles]")
    if roles_param != "":
        wanted = []
        for part in roles_param.split(","):
            part = part.strip()
            if part != "":
                wanted.append(part)
        if len(wanted) > 0:
            out = []
            for u in users:
                got = u.get("attributes", {}).get("roles", [])
                matched = False
                for r in wanted:
                    if r in got:
                        matched = True
                        break
                if matched:
                    out.append(u)
            users = out

    f = []
    v = _get_query(req, "filter[username]")
    if v != "":
        f.append(["attributes.username", "=", v])

    sort_field, desc = _asc_sort(req)
    order_by = None
    order_dir = ""
    if sort_field == "username" or sort_field == "lastName" or sort_field == "firstName":
        order_by = "attributes." + sort_field
        order_dir = "desc" if desc else "asc"

    return query_select(users, f if len(f) > 0 else None, order_by, order_dir, None, None, None)
