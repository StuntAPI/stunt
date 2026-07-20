# Check handlers — Onfido API.
#
# POST /v3.6/checks {applicant_id, report_names} → check (in_progress)
# GET  /v3.6/checks/{id} → check (in_progress→complete, result clear|consider)

# Shared helpers (_token, _require_auth, _err, _gen_id, _advance_check_status)
# are preloaded.

def on_create_check(req):
    if not _require_auth(req):
        return respond(401, _err("authorization_error", "Invalid API token", None))

    body = req["body"]
    if body == None:
        body = {}

    applicant_id = body.get("applicant_id", "")
    report_names = body.get("report_names", [])

    if applicant_id == "":
        return respond(422, _err("validation_error", "applicant_id is required", {
            "applicant_id": ["can't be blank"],
        }))
    if len(report_names) == 0:
        return respond(422, _err("validation_error", "report_names must not be empty", {
            "report_names": ["can't be blank"],
        }))

    ac = store_collection("applicants")
    if ac.get(applicant_id) == None:
        return respond(404, _err("not_found", "Applicant not found", None))

    seq = store_kv_incr("onfido", "check_seq")
    check_id = _gen_id("chk", seq)

    cc = store_collection("checks")
    cc.insert({
        "id": check_id,
        "applicant_id": applicant_id,
        "report_names": report_names,
        "status": "in_progress",
        "result": None,
        "get_count": 0,
        "created_at": "2024-01-15T10:00:15.000Z",
        "href": "/v3.6/checks/" + check_id,
    })

    return respond(201, {
        "id": check_id,
        "applicant_id": applicant_id,
        "report_names": report_names,
        "status": "in_progress",
        "result": None,
        "created_at": "2024-01-15T10:00:15.000Z",
        "href": "/v3.6/checks/" + check_id,
    })

def on_get_check(req):
    if not _require_auth(req):
        return respond(401, _err("authorization_error", "Invalid API token", None))

    check_id = req["params"]["check_id"]
    cc = store_collection("checks")
    doc = cc.get(check_id)
    if doc == None:
        return respond(404, _err("not_found", "Check not found", None))

    # Advance: in_progress → complete on first GET.
    if doc["status"] == "in_progress":
        doc["status"] = "complete"
        doc["result"] = "clear"
        doc["get_count"] = 1
        cc.update(check_id, doc)

    result = {
        "id": doc["id"],
        "applicant_id": doc["applicant_id"],
        "report_names": doc.get("report_names", []),
        "status": doc["status"],
        "result": doc["result"],
        "created_at": doc.get("created_at", ""),
        "href": doc.get("href", ""),
    }

    # Include breakdown when complete.
    if doc["status"] == "complete":
        result["breakdown"] = _build_breakdown(doc.get("report_names", []))

    return respond(200, result)

# _build_breakdown creates a synthetic check breakdown.
def _build_breakdown(report_names):
    breakdown = {}
    for r in report_names:
        breakdown[r] = {
            "result": "clear",
            "sub_checks": [],
        }
    return breakdown
