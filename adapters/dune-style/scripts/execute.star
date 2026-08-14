# Execution handlers — Dune Analytics SQL API.
#
# POST /api/v1/query/{id}/execute → {execution_id, state:"QUERY_STATE_PENDING"}
# POST /api/v1/query/{id}/result  → {execution_id, state:"QUERY_STATE_COMPLETED", result:{rows}}
# GET  /api/v1/execution/{id}/status → {execution_id, state}
# GET  /api/v1/execution/{id}/results → {execution_id, state, result:{rows, metadata}, next_uri}
#
# Async lifecycle is derive-on-read: the doc stores _running_at (create + 1s)
# and _done_at (create + 3s) computed from clock.now_unix(); every read
# derives the current state from the injectable clock and persists the
# transition back. PENDING → EXECUTING → COMPLETED (or FAILED when the
# simulator-only simulate_fail flag was set at execute time).

# Shared helpers (_bearer, _require_auth, _gen_execution_id, _seed_rows,
# _metadata, _to_int, _derive_exec_state, _advance_execution) are preloaded.

def on_execute(req):
    if not _require_auth(req):
        return respond(401, {"error": "Invalid API key"})

    query_id = req["params"]["query_id"]
    body = req.get("body")
    if body == None:
        body = {}

    now = clock.now_unix()
    exec_id = _gen_execution_id()

    ec = store_collection("executions")
    ec.insert({
        "id": exec_id,
        "query_id": query_id,
        "state": "QUERY_STATE_PENDING",
        "get_count": 0,
        "created_at": clock.now_rfc3339(),
        "_running_at": now + 1,
        "_done_at": now + 3,
        "_fail": body.get("simulate_fail", False),
    })

    return respond(200, {
        "execution_id": exec_id,
        "state": "QUERY_STATE_PENDING",
    })

def on_inline_result(req):
    if not _require_auth(req):
        return respond(401, {"error": "Invalid API key"})

    query_id = req["params"]["query_id"]
    exec_id = _gen_execution_id()

    ec = store_collection("executions")
    ec.insert({
        "id": exec_id,
        "query_id": query_id,
        "state": "QUERY_STATE_COMPLETED",
        "get_count": 0,
        "created_at": clock.now_rfc3339(),
    })

    rows = _seed_rows(query_id)
    return respond(200, {
        "execution_id": exec_id,
        "state": "QUERY_STATE_COMPLETED",
        "result": {
            "rows": rows,
            "metadata": _metadata(),
        },
        "next_uri": None,
    })

def on_get_status(req):
    if not _require_auth(req):
        return respond(401, {"error": "Invalid API key"})

    exec_id = req["params"]["execution_id"]
    ec = store_collection("executions")
    doc = ec.get(exec_id)
    if doc == None:
        return respond(404, {"error": "Execution not found"})

    doc["get_count"] = doc.get("get_count", 0) + 1
    state = _advance_execution(exec_id, doc)
    ec.update(exec_id, doc)

    return respond(200, {
        "execution_id": doc["id"],
        "state": state,
    })

def on_get_results(req):
    if not _require_auth(req):
        return respond(401, {"error": "Invalid API key"})

    exec_id = req["params"]["execution_id"]
    ec = store_collection("executions")
    doc = ec.get(exec_id)
    if doc == None:
        return respond(404, {"error": "Execution not found"})

    state = _advance_execution(exec_id, doc)

    # Dune errors when results are requested before the execution finishes
    # (or after it failed): there is nothing to fetch yet.
    if state == "QUERY_STATE_FAILED":
        return respond(404, {"error": "Execution failed; no results are available"})
    if state != "QUERY_STATE_COMPLETED":
        return respond(404, {"error": "Execution is not completed yet"})

    rows = _seed_rows(doc["query_id"])
    return respond(200, {
        "execution_id": doc["id"],
        "state": state,
        "result": {
            "rows": rows,
            "metadata": _metadata(),
        },
        "next_uri": None,
    })
