# Work tracking handler — Azure DevOps iterations endpoint.
#
# GET /{org}/{project}/_apis/work/teamsettings/iterations → {value:[...]}

def on_iterations(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    items = [
        {
            "id": "22222222-0000-0000-0000-000000000001",
            "name": "Sprint 1",
            "path": "MyFirstProject\\Sprint 1",
            "attributes": {
                "startDate": "2024-01-01",
                "finishDate": "2024-01-14",
                "timeFrame": "current",
            },
        },
        {
            "id": "22222222-0000-0000-0000-000000000002",
            "name": "Sprint 2",
            "path": "MyFirstProject\\Sprint 2",
            "attributes": {
                "startDate": "2024-01-15",
                "finishDate": "2024-01-28",
                "timeFrame": "future",
            },
        },
    ]

    # Apply OData $top/$skip paging.
    page, continuation = _list_page(req, items)
    if page == None:
        return respond(400, {"message": "Invalid continuation token."})

    resp = {"value": page, "count": len(page)}
    if continuation != None:
        resp["continuationToken"] = continuation
    return respond(200, resp)
