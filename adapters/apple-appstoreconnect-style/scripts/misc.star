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

    return respond(200, {
        "data": users,
        "links": {
            "self": "/v1/users",
        },
        "meta": {
            "paging": {
                "total": len(users),
                "limit": 50,
            },
        },
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
