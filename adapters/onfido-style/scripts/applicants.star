# Applicant handlers — Onfido API.
#
# POST /v3.6/applicants {first_name, last_name, dob} → applicant object
# GET  /v3.6/applicants/{id} → applicant object

# Shared helpers (_token, _require_auth, _err, _gen_id) are preloaded.

def on_create_applicant(req):
    if not _require_auth(req):
        return respond(401, _err("authorization_error", "Invalid API token", None))

    body = req["body"]
    if body == None:
        body = {}

    first_name = body.get("first_name", "")
    last_name = body.get("last_name", "")
    dob = body.get("dob", "")

    if first_name == "" or last_name == "":
        return respond(422, _err("validation_error", "first_name and last_name are required", {
            "first_name": ["can't be blank"],
        }))

    seq = store_kv_incr("onfido", "applicant_seq")
    applicant_id = _gen_id("app", seq)

    ac = store_collection("applicants")
    ac.insert({
        "id": applicant_id,
        "first_name": first_name,
        "last_name": last_name,
        "dob": dob,
        "email": body.get("email", None),
        "created_at": "2024-01-15T10:00:00.000Z",
        "href": "/v3.6/applicants/" + applicant_id,
    })

    return respond(201, {
        "id": applicant_id,
        "first_name": first_name,
        "last_name": last_name,
        "dob": dob,
        "email": body.get("email", None),
        "created_at": "2024-01-15T10:00:00.000Z",
        "href": "/v3.6/applicants/" + applicant_id,
    })

def on_get_applicant(req):
    if not _require_auth(req):
        return respond(401, _err("authorization_error", "Invalid API token", None))

    applicant_id = req["params"]["applicant_id"]
    ac = store_collection("applicants")
    doc = ac.get(applicant_id)
    if doc == None:
        return respond(404, _err("not_found", "Applicant not found", None))

    return respond(200, {
        "id": doc["id"],
        "first_name": doc["first_name"],
        "last_name": doc["last_name"],
        "dob": doc.get("dob", ""),
        "email": doc.get("email", None),
        "created_at": doc.get("created_at", ""),
        "href": doc.get("href", ""),
    })
