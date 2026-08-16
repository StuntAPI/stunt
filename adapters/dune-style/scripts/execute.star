# Execution handlers — Dune Analytics SQL API.
#
# POST /api/v1/query/{id}/execute → {execution_id, state:"QUERY_STATE_PENDING"}
#   (query_parameters validated against the query model; a missing required
#   parameter is Dune's documented 400 envelope)
# POST /api/v1/query/{id}/result  → {execution_id, query_id, state:"QUERY_STATE_COMPLETED", result:{rows}}
# GET  /api/v1/execution/{id}/status → {execution_id, query_id, state, ...}
# GET  /api/v1/execution/{id}/results → {execution_id, query_id, state, result:{rows, metadata},
#                                         next_uri, next_offset}  (limit/offset paging)
# GET  /api/v1/execution/{id}/results/csv → CSV text of the same rows (text/csv)
#
# Async lifecycle is derive-on-read: the doc stores _running_at (create + 1s)
# and _done_at (create + 3s) computed from clock.now_unix(); every read
# derives the current state from the injectable clock and persists the
# transition back. PENDING → EXECUTING → COMPLETED (or FAILED when the
# simulator-only simulate_fail flag was set at execute time).
#
# The resolved query_parameters are stored on the execution doc (_params) so
# results, pagination and CSV all regenerate the SAME rows the parameters
# selected — paging through an execution never re-rolls the data.

# Shared helpers (_bearer, _require_auth, _gen_execution_id, _query_model,
# _resolve_params, _seed_rows, _metadata, _rows_csv, _next_uri, _query_int,
# _to_int, _derive_exec_state, _advance_execution) are preloaded.

def on_execute(req):
    if not _require_auth(req):
        return respond(401, {"error": "Invalid API key"})

    query_id = req["params"]["query_id"]
    body = req.get("body")
    if body == None:
        body = {}

    # Validate query_parameters against the query model: executing a query
    # without a required parameter is Dune's documented 400 error envelope.
    params, missing = _resolve_params(query_id, body)
    if len(missing) > 0:
        return respond(400, {"error": "Bad Request"})

    now = clock.now_unix()
    exec_id = _gen_execution_id()

    ec = store_collection("executions")
    ec.insert({
        "id": exec_id,
        "query_id": query_id,
        "state": "QUERY_STATE_PENDING",
        "get_count": 0,
        "created_at": clock.now_rfc3339(),
        "_created_unix": now,
        "_running_at": now + 1,
        "_done_at": now + 3,
        "_params": params,
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
    body = req.get("body")
    if body == None:
        body = {}

    params, missing = _resolve_params(query_id, body)
    if len(missing) > 0:
        return respond(400, {"error": "Bad Request"})

    exec_id = _gen_execution_id()

    ec = store_collection("executions")
    now = clock.now_unix()
    ec.insert({
        "id": exec_id,
        "query_id": query_id,
        "state": "QUERY_STATE_COMPLETED",
        "get_count": 0,
        "created_at": clock.now_rfc3339(),
        # inline results are already terminal; stamps keep the poll path uniform
        "_created_unix": now,
        "_running_at": now,
        "_done_at": now,
        "_params": params,
    })

    rows = _seed_rows(query_id, params)
    return respond(200, {
        "execution_id": exec_id,
        "query_id": _to_int(query_id),
        "state": "QUERY_STATE_COMPLETED",
        "submitted_at": clock.now_rfc3339(),
        "expires_at": _stamp_or_none(now + _RESULT_TTL_SECONDS),
        "execution_started_at": clock.now_rfc3339(),
        "execution_ended_at": clock.now_rfc3339(),
        "is_execution_finished": True,
        "result": {
            "rows": rows,
            "metadata": _metadata(rows, rows, 0, 0),
        },
        "next_uri": None,
        "next_offset": None,
    })

def on_get_status(req):
    if not _require_auth(req):
        return respond(401, {"error": "Invalid API key"})

    exec_id = req["params"]["execution_id"]
    ec = store_collection("executions")
    doc = ec.get(exec_id)
    if doc == None:
        return respond(404, {"error": "Object not found"})

    doc["get_count"] = doc.get("get_count", 0) + 1
    state = _advance_execution(exec_id, doc)
    ec.update(exec_id, doc)

    started = None
    if state != "QUERY_STATE_PENDING":
        started = _stamp_or_none(doc.get("_running_at"))
    ended = None
    if state == "QUERY_STATE_COMPLETED" or state == "QUERY_STATE_FAILED":
        ended = _stamp_or_none(doc.get("_done_at"))

    return respond(200, {
        "execution_id": doc["id"],
        "query_id": _to_int(doc.get("query_id", "")),
        "state": state,
        "submitted_at": doc.get("created_at"),
        "expires_at": _expires_at(doc),
        "execution_started_at": started,
        "execution_ended_at": ended,
        "is_execution_finished": state == "QUERY_STATE_COMPLETED" or state == "QUERY_STATE_FAILED",
    })

# _terminal_results_doc loads an execution and derives its state, returning
# (doc, state, error_response). error_response is non-None when there is
# nothing to fetch (unknown id, still running, or failed), matching Dune's
# 404 behavior on the results endpoints.
def _terminal_results_doc(req):
    exec_id = req["params"]["execution_id"]
    ec = store_collection("executions")
    doc = ec.get(exec_id)
    if doc == None:
        return None, None, respond(404, {"error": "Object not found"})
    state = _advance_execution(exec_id, doc)
    if state == "QUERY_STATE_FAILED":
        return None, None, respond(404, {"error": "Execution failed; no results are available"})
    if state != "QUERY_STATE_COMPLETED":
        return None, None, respond(404, {"error": "Execution is not completed yet"})
    return doc, state, None

def on_get_results(req):
    if not _require_auth(req):
        return respond(401, {"error": "Invalid API key"})

    doc, state, err = _terminal_results_doc(req)
    if err != None:
        return err
    return _results_page(req, doc, state, False)

def on_get_results_csv(req):
    if not _require_auth(req):
        return respond(401, {"error": "Invalid API key"})

    doc, state, err = _terminal_results_doc(req)
    if err != None:
        return err
    return _results_page(req, doc, state, True)

# _results_page regenerates the execution's full row set (query_id + stored
# parameters), slices it by the limit/offset query params and renders either
# the JSON envelope or the CSV body. next_uri / next_offset are set whenever
# rows remain beyond the returned page.
def _results_page(req, doc, state, csv):
    rows = _seed_rows(doc.get("query_id", ""), doc.get("_params"))
    limit = _query_int(req, "limit", _DEFAULT_LIMIT)
    offset = _query_int(req, "offset", 0)

    if offset > len(rows):
        offset = len(rows)
    end = offset + limit
    if end > len(rows):
        end = len(rows)
    page = rows[offset:end]

    next_uri = None
    next_offset = None
    if end < len(rows):
        next_offset = end
        next_uri = _next_uri(req, doc["id"], end, limit)

    if csv:
        return respond(200, _rows_csv(page), {"Content-Type": "text/csv"})

    return respond(200, {
        "execution_id": doc["id"],
        "query_id": _to_int(doc.get("query_id", "")),
        "state": state,
        "submitted_at": doc.get("created_at"),
        "expires_at": _expires_at(doc),
        "execution_started_at": _stamp_or_none(doc.get("_running_at")),
        "execution_ended_at": _stamp_or_none(doc.get("_done_at")),
        "is_execution_finished": True,
        "result": {
            "rows": page,
            "metadata": _metadata(
                page,
                rows,
                _elapsed_ms(doc.get("_running_at", 0), doc.get("_created_unix", 0)),
                _elapsed_ms(doc.get("_done_at", 0), doc.get("_running_at", 0)),
            ),
        },
        "next_uri": next_uri,
        "next_offset": next_offset,
    })
