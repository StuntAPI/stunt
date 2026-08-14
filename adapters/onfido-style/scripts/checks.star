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

    # Async lifecycle: derive-on-read timestamps (see lib.star).
    now = clock.now_unix()

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
        "_running_at": now + 1,
        "_done_at": now + 3,
        "_fail": body.get("simulate_fail", False) == True,
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

    # Derive current status from the clock and persist any transition so
    # lists/webhooks agree with the poll.
    new_status = _derive_check_status(doc)
    if new_status != doc["status"]:
        doc["status"] = new_status
        doc["result"] = _derive_check_result(doc)
        cc.update(check_id, doc)

        # Side effects fire exactly once, at the transition Onfido notifies.
        if new_status == "complete":
            _signed_emit("check.completed", {
                "payload": {
                    "resource_type": "check",
                    "action": "check.completed",
                    "object": {
                        "id": check_id,
                        "status": "complete",
                        "result": doc["result"],
                        "href": doc.get("href", ""),
                    },
                },
            })
    elif doc["status"] == "complete" and doc.get("result", None) == None:
        doc["result"] = _derive_check_result(doc)
        cc.update(check_id, doc)

    doc["get_count"] = doc.get("get_count", 0) + 1
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
        result["breakdown"] = _build_breakdown(doc.get("report_names", []), doc.get("result", "clear"))

    return respond(200, result)

# _build_breakdown creates a synthetic check breakdown; each report matches
# the overall check result.
def _build_breakdown(report_names, overall):
    breakdown = {}
    for r in report_names:
        breakdown[r] = {
            "result": overall,
            "sub_checks": [],
        }
    return breakdown
