# Template handlers — list + create + update (approval lifecycle).
#
# GET  /v21.0/{waba_id}/message_templates → {data:[...]}
# POST /v21.0/{waba_id}/message_templates → {id, name, status:"PENDING"}
# POST /v21.0/{template_id}               → {id, status}  (update lifecycle)
#
# Template approval lifecycle: PENDING → APPROVED or PENDING → REJECTED.
# New templates are created with status PENDING (matching the real 24h+
# review process). The POST /v21.0/{template_id} endpoint simulates the
# approval/rejection decision.
#
# Requires Bearer access token.

# Shared helpers (_require_auth, _wa_unauthorized, _wa_not_found, _next_id,
# _now, _seed) are preloaded from scripts/lib.star.

# on_list_templates returns all message templates for a WABA.
def on_list_templates(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    tc = store_collection("templates")
    all_tmpls = tc.list()
    result = []
    for t in all_tmpls:
        result.append(_template_view(t))

    return respond(200, {"data": result})

# on_create_template creates a new message template (status PENDING).
def on_create_template(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    body = req["body"]
    if body == None:
        body = {}

    name = body.get("name", "")
    if name == None:
        name = ""
    language = body.get("language", "en_US")
    if language == None:
        language = "en_US"
    category = body.get("category", "MARKETING")
    if category == None:
        category = "MARKETING"
    components = body.get("components", [])
    if components == None:
        components = []

    tmpl_id = _next_id("template")
    tmpl = {
        "id": tmpl_id,
        "name": name,
        "language": language,
        "status": "PENDING",
        "category": category,
        "components": components,
        "created_at": _now(),
    }

    tc = store_collection("templates")
    tc.insert(tmpl)

    return respond(200, _template_view(tmpl))

# on_update_template updates a template's status (PENDING → APPROVED/REJECTED).
def on_update_template(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    tmpl_id = req["params"]["template_id"]
    body = req["body"]
    if body == None:
        body = {}

    new_status = body.get("status", "")
    if new_status == None:
        new_status = ""

    tc = store_collection("templates")
    tmpl = tc.get(tmpl_id)
    if tmpl == None:
        return _wa_not_found("template")

    # Only valid status transitions are allowed.
    if new_status == "APPROVED" or new_status == "REJECTED":
        tmpl["status"] = new_status
        tc.update(tmpl_id, tmpl)
        return respond(200, _template_view(tmpl))

    return respond(200, _template_view(tmpl))

# --- helpers ---

def _template_view(t):
    return {
        "id": t["id"],
        "name": t.get("name", ""),
        "language": t.get("language", ""),
        "status": t.get("status", "PENDING"),
        "category": t.get("category", "MARKETING"),
        "components": t.get("components", []),
        "created_at": t.get("created_at", _now()),
    }
