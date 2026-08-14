# App Store Connect API — users and sales reports handlers.
#
# GET /v1/users        → list App Store Connect users
# GET /v1/salesReports → sales report (fields-based)

# Shared helpers (_require_jwt, _err, _not_found_err) are preloaded from
# scripts/lib.star.

# on_list_users handles GET /v1/users.
def on_list_users(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    users = [
        {
            "id": "user_001",
            "type": "users",
            "attributes": {
                "username": "admin@example.com",
                "firstName": "Mock",
                "lastName": "Admin",
                "roles": ["ADMIN"],
                "allAppsVisible": True,
                "provisioningAllowed": True,
            },
        },
        {
            "id": "user_002",
            "type": "users",
            "attributes": {
                "username": "developer@example.com",
                "firstName": "Mock",
                "lastName": "Developer",
                "roles": ["DEVELOPER"],
                "allAppsVisible": False,
                "provisioningAllowed": True,
            },
        },
    ]

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
                    "appleIdentifier": "1500000001",
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
