# Tracking Categories handler.
#
# Requires Bearer + xero-tenant-id.
# GET /api.xro/2.0/TrackingCategories → { Id, Status, TrackingCategories: [...] }

# on_list_tracking returns synthetic tracking categories.
def on_list_tracking(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_tenant(req)
    if err != None:
        return err

    return _envelope("TrackingCategories", [
        {
            "TrackingCategoryID": _guid(501),
            "Name": "Region",
            "Status": "ACTIVE",
            "Options": [
                {"TrackingOptionID": _guid(601), "Name": "North", "Status": "ACTIVE"},
                {"TrackingOptionID": _guid(602), "Name": "South", "Status": "ACTIVE"},
            ],
        },
    ])
