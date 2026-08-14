# Companies handler — list companies.
#
# Requires auth.
# GET /v2/companies → { value: [{ id, companyCode, name, ... }] }

# on_list_companies returns synthetic AvaTax companies.
def on_list_companies(req):
    err = _require_auth(req)
    if err != None:
        return err

    value = [
        {
            "id": _company_id(),
            "companyCode": "DEFAULT",
            "name": "Default Company",
            "defaultLocation": {
                "line1": "100 Main St",
                "city": "San Francisco",
                "region": "CA",
                "country": "US",
                "postalCode": "94016",
            },
        },
        {
            "id": _company_id(),
            "companyCode": "STORE1",
            "name": "Store 1",
            "defaultLocation": {
                "line1": "200 Oak Ave",
                "city": "New York",
                "region": "NY",
                "country": "US",
                "postalCode": "10001",
            },
        },
    ]

    # Apply OData $filter/$orderBy before paging.
    value = _apply_odata_filters(req, value)

    # Apply OData $top/$skip paging.
    page, next_link = _list_page(req, value, "/v2/companies")

    resp = {
        "@recordsetCount": len(value),
        "value": page,
    }
    if next_link != None:
        resp["@odata.nextLink"] = next_link
    return respond(200, resp)
