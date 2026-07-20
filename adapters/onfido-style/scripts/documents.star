# Document + Live Photo handlers — Onfido API.
#
# POST /v3.6/documents  (multipart, applicant_id required)
#   → document object {id, applicant_id, type, side, file_name}
# POST /v3.6/live_photos (multipart, applicant_id required)
#   → live photo object {id, applicant_id, file_name}

# Shared helpers (_token, _require_auth, _err, _gen_id) are preloaded.

def on_upload_document(req):
    if not _require_auth(req):
        return respond(401, _err("authorization_error", "Invalid API token", None))

    body = req["body"]
    if body == None:
        body = {}

    applicant_id = body.get("applicant_id", "")
    if applicant_id == "":
        return respond(422, _err("validation_error", "applicant_id is required", {
            "applicant_id": ["can't be blank"],
        }))

    # Verify applicant exists.
    ac = store_collection("applicants")
    if ac.get(applicant_id) == None:
        return respond(404, _err("not_found", "Applicant not found", None))

    doc_type = body.get("type", "passport")
    side = body.get("side", "front")
    file_name = body.get("file_name", "document.jpg")

    seq = store_kv_incr("onfido", "document_seq")
    doc_id = _gen_id("doc", seq)

    dc = store_collection("documents")
    dc.insert({
        "id": doc_id,
        "applicant_id": applicant_id,
        "type": doc_type,
        "side": side,
        "file_name": file_name,
        "created_at": "2024-01-15T10:00:05.000Z",
    })

    return respond(201, {
        "id": doc_id,
        "applicant_id": applicant_id,
        "type": doc_type,
        "side": side,
        "file_name": file_name,
        "created_at": "2024-01-15T10:00:05.000Z",
    })

def on_upload_live_photo(req):
    if not _require_auth(req):
        return respond(401, _err("authorization_error", "Invalid API token", None))

    body = req["body"]
    if body == None:
        body = {}

    applicant_id = body.get("applicant_id", "")
    if applicant_id == "":
        return respond(422, _err("validation_error", "applicant_id is required", {
            "applicant_id": ["can't be blank"],
        }))

    ac = store_collection("applicants")
    if ac.get(applicant_id) == None:
        return respond(404, _err("not_found", "Applicant not found", None))

    file_name = body.get("file_name", "selfie.jpg")

    seq = store_kv_incr("onfido", "photo_seq")
    photo_id = _gen_id("lph", seq)

    pc = store_collection("live_photos")
    pc.insert({
        "id": photo_id,
        "applicant_id": applicant_id,
        "file_name": file_name,
        "created_at": "2024-01-15T10:00:10.000Z",
    })

    return respond(201, {
        "id": photo_id,
        "applicant_id": applicant_id,
        "file_name": file_name,
        "created_at": "2024-01-15T10:00:10.000Z",
    })
