# Compensation handler — Workday Compensation REST API.
#
# GET /wbs/v40.0/compensation/workers/{id}/compensation
# -> {data:[{workerID, compensation components...}], total, more}

# Shared helpers from lib.star.

# Synthetic compensation data keyed by worker ID.
_COMP_SHELF = {
    "1": [
        {
            "compensationComponent": {"id": "1", "descriptor": "Base Salary"},
            "amount": "145000.00",
            "currency": {"id": "1", "descriptor": "USD"},
            "frequency": {"id": "1", "descriptor": "Annual"},
        },
        {
            "compensationComponent": {"id": "2", "descriptor": "Annual Bonus"},
            "amount": "15000.00",
            "currency": {"id": "1", "descriptor": "USD"},
            "frequency": {"id": "1", "descriptor": "Annual"},
        },
    ],
    "2": [
        {
            "compensationComponent": {"id": "1", "descriptor": "Base Salary"},
            "amount": "78000.00",
            "currency": {"id": "1", "descriptor": "USD"},
            "frequency": {"id": "2", "descriptor": "Hourly"},
        },
    ],
    "3": [
        {
            "compensationComponent": {"id": "1", "descriptor": "Base Salary"},
            "amount": "95000.00",
            "currency": {"id": "1", "descriptor": "USD"},
            "frequency": {"id": "3", "descriptor": "Per Project"},
        },
    ],
}

def on_worker_compensation(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    worker_id = req["params"].get("id", "")
    if worker_id == "":
        return _workday_error(400, "INVALID_REQUEST", "A worker ID is required.")

    comps = _COMP_SHELF.get(worker_id, [])
    if comps == None:
        comps = []

    return respond(200, {
        "data": comps,
        "total": len(comps),
        "more": False,
    })
