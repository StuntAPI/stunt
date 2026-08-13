# Environments handler — Microsoft Power Platform API.
#
# GET /v2/environments → OData {value:[{name, id, location, properties}]}

def on_list_environments(req):
    err = _require_bearer(req)
    if err != None:
        return err

    page, next_link = _list_page(req, _ENVS, "/v2/environments")

    resp = {"value": page}
    if next_link != None:
        resp["@odata.nextLink"] = next_link
    return respond(200, resp)
