# Read-only connection probe; a fresh simulator owns no verified domains.
def on_list_domains(req):
    err = _require_auth(req)
    if err != None:
        return err
    return respond(200, {"object": "list", "data": [], "has_more": False})
