# Microsoft Graph v1.0 — SharePoint handlers.
#
# GET /v1.0/groups/{id}/sites → list sites for a group

# on_list_sites returns SharePoint sites for a group.
# GET /v1.0/groups/{id}/sites (Bearer)
def on_list_sites(req):
    err = _require_bearer(req)
    if err != None:
        return err

    group_id = req["params"].get("id", "mock-group")

    sites = [
        {
            "id": group_id + ",mock-site-communication:/sites/team",
            "displayName": "Team Site",
            "name": "team",
            "webUrl": "https://mock-tenant.sharepoint.com/sites/team",
            "createdDateTime": "2024-01-01T00:00:00Z",
            "siteCollection": {"hostname": "mock-tenant.sharepoint.com"},
        },
        {
            "id": group_id + ",mock-site-docs:/sites/docs",
            "displayName": "Documentation",
            "name": "docs",
            "webUrl": "https://mock-tenant.sharepoint.com/sites/docs",
            "createdDateTime": "2024-02-01T00:00:00Z",
            "siteCollection": {"hostname": "mock-tenant.sharepoint.com"},
        },
    ]

    return respond(200, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#groups('" + group_id + "')/sites",
        "value": sites,
    })
