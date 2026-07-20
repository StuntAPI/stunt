# Inquiry handlers — Persona Inquiry API (JSON:API).
#
# POST /api/inquiry/v1/inquiries
#   JSON {template_id, reference_id} → {data:{id, type:"inquiry", attributes:{status:"created", reference_id}}}
# GET  /api/inquiry/v1/inquiries/{id}
#   → status progresses created→pending→completed
# POST /api/inquiry/v1/inquiries/{id}/resume
#   → inquiry reactivated, status "pending"
# GET  /api/inquiry/v1/inquiries/{id}/verifications
#   → {data: [{id, type:"verification", attributes:{name, status, result}}]}

# Shared helpers (_bearer, _require_auth, _gen_id, _jsonapi_ok, _jsonapi_err,
# _advance_status, _seed_verifications) are preloaded from scripts/lib.star.

# on_create_inquiry handles POST /api/inquiry/v1/inquiries.
def on_create_inquiry(req):
    if not _require_auth(req):
        return respond(401, _jsonapi_err(401, "unauthorized", "Missing or invalid API key"))

    body = req["body"]
    if body == None:
        body = {}

    template_id = body.get("template_id", "")
    reference_id = body.get("reference_id", "")
    if template_id == "" or reference_id == "":
        return respond(400, _jsonapi_err(400, "invalid_request", "template_id and reference_id are required"))

    seq = store_kv_incr("persona", "inquiry_seq")
    inquiry_id = _gen_id(seq)

    ic = store_collection("inquiries")
    ic.insert({
        "id": inquiry_id,
        "type": "inquiry",
        "template_id": template_id,
        "reference_id": reference_id,
        "status": "created",
        "get_count": 0,
        "created_at": "2024-01-15T10:00:00.000Z",
    })

    data = {
        "id": inquiry_id,
        "type": "inquiry",
        "attributes": {
            "status": "created",
            "reference_id": reference_id,
            "template_id": template_id,
            "created_at": "2024-01-15T10:00:00.000Z",
        },
    }
    return respond(201, _jsonapi_ok(data))

# on_get_inquiry handles GET /api/inquiry/v1/inquiries/{id}.
# Status progresses: created→pending→completed.
def on_get_inquiry(req):
    if not _require_auth(req):
        return respond(401, _jsonapi_err(401, "unauthorized", "Missing or invalid API key"))

    inquiry_id = req["params"]["inquiry_id"]

    ic = store_collection("inquiries")
    doc = ic.get(inquiry_id)
    if doc == None:
        return respond(404, _jsonapi_err(404, "not_found", "Inquiry not found"))

    # Advance status on each GET.
    get_count = doc.get("get_count", 0) + 1
    current = doc["status"]
    if current != "completed":
        new_status = _advance_status(current)
        doc["status"] = new_status
        doc["get_count"] = get_count
        ic.update(inquiry_id, doc)

        # Seed verifications when inquiry completes.
        if new_status == "completed":
            _seed_verifications(inquiry_id, doc["reference_id"])
    else:
        doc["get_count"] = get_count
        ic.update(inquiry_id, doc)

    return respond(200, _jsonapi_ok({
        "id": doc["id"],
        "type": "inquiry",
        "attributes": {
            "status": doc["status"],
            "reference_id": doc["reference_id"],
            "template_id": doc.get("template_id", ""),
            "created_at": doc.get("created_at", ""),
        },
    }))

# on_resume_inquiry handles POST /api/inquiry/v1/inquiries/{id}/resume.
def on_resume_inquiry(req):
    if not _require_auth(req):
        return respond(401, _jsonapi_err(401, "unauthorized", "Missing or invalid API key"))

    inquiry_id = req["params"]["inquiry_id"]

    ic = store_collection("inquiries")
    doc = ic.get(inquiry_id)
    if doc == None:
        return respond(404, _jsonapi_err(404, "not_found", "Inquiry not found"))

    doc["status"] = "pending"
    doc["get_count"] = 0
    ic.update(inquiry_id, doc)

    return respond(200, _jsonapi_ok({
        "id": doc["id"],
        "type": "inquiry",
        "attributes": {
            "status": "pending",
            "reference_id": doc["reference_id"],
        },
    }))

# on_get_verifications handles GET /api/inquiry/v1/inquiries/{id}/verifications.
def on_get_verifications(req):
    if not _require_auth(req):
        return respond(401, _jsonapi_err(401, "unauthorized", "Missing or invalid API key"))

    inquiry_id = req["params"]["inquiry_id"]

    ic = store_collection("inquiries")
    doc = ic.get(inquiry_id)
    if doc == None:
        return respond(404, _jsonapi_err(404, "not_found", "Inquiry not found"))

    vc = store_collection("verifications")
    all_ver = vc.list()
    data = []
    for v in all_ver:
        if v.get("inquiry_id", "") != inquiry_id:
            continue
        data.append({
            "id": v["id"],
            "type": "verification",
            "attributes": {
                "name": v["name"],
                "status": v["status"],
                "result": v["result"],
                "created_at": v.get("created_at", ""),
            },
        })

    return respond(200, _jsonapi_ok_list(data))

# _jsonapi_ok_list wraps a list in a JSON:API data envelope.
def _jsonapi_ok_list(data_list):
    return {"data": data_list}
