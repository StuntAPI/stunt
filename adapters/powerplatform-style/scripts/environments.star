# Environments handler — Microsoft Power Platform API.
#
# GET /v2/environments → OData {value:[{name, id, location, properties}]}

def on_list_environments(req):
    err = _require_bearer(req)
    if err != None:
        return err

    return respond(200, {"value": _ENVS})
