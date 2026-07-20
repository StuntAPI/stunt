# Execution handlers — Dune Analytics SQL API.
#
# POST /api/v1/query/{id}/execute → {execution_id, state:"QUERY_STATE_PENDING"}
# POST /api/v1/query/{id}/result  → {execution_id, state:"QUERY_STATE_COMPLETED", result:{rows}}
# GET  /api/v1/execution/{id}/status → {execution_id, state}
# GET  /api/v1/execution/{id}/results → {execution_id, state, result:{rows, metadata}, next_uri}

# Shared helpers (_bearer, _require_auth, _gen_execution_id, _seed_rows,
# _metadata, _to_int) are preloaded.

def on_execute(req):
    if not _require_auth(req):
        return respond(401, {"error": "Invalid API key"})

    query_id = req["params"]["query_id"]
    exec_id = _gen_execution_id()

    ec = store_collection("executions")
    ec.insert({
        "id": exec_id,
        "query_id": query_id,
        "state": "QUERY_STATE_PENDING",
        "get_count": 0,
        "created_at": "2024-01-15T10:00:00.000Z",
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
        "created_at": "2024-01-15T10:00:00.000Z",
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

    # Advance: PENDING → COMPLETED on first GET.
    if doc["state"] == "QUERY_STATE_PENDING":
        doc["state"] = "QUERY_STATE_COMPLETED"
        doc["get_count"] = 1
        ec.update(exec_id, doc)

    return respond(200, {
        "execution_id": doc["id"],
        "state": doc["state"],
    })

def on_get_results(req):
    if not _require_auth(req):
        return respond(401, {"error": "Invalid API key"})

    exec_id = req["params"]["execution_id"]
    ec = store_collection("executions")
    doc = ec.get(exec_id)
    if doc == None:
        return respond(404, {"error": "Execution not found"})

    # Auto-complete if still pending.
    if doc["state"] == "QUERY_STATE_PENDING":
        doc["state"] = "QUERY_STATE_COMPLETED"
        ec.update(exec_id, doc)

    rows = _seed_rows(doc["query_id"])
    return respond(200, {
        "execution_id": doc["id"],
        "state": doc["state"],
        "result": {
            "rows": rows,
            "metadata": _metadata(),
        },
        "next_uri": None,
    })
