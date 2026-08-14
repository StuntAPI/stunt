# Task handlers — Anaplan API async import/export tasks.
#
# POST /2/0/workspaces/{wid}/models/{mid}/imports/{importId}/tasks → start import
# POST /2/0/workspaces/{wid}/models/{mid}/exports/{exportId}/tasks → start export
# GET  /2/0/workspaces/{wid}/models/{mid}/tasks/{taskId}             → task status
# GET  /2/0/workspaces/{wid}/models/{mid}/exports                    → list exports
#
# Async lifecycle is derive-on-read: the task doc stores _running_at
# (create + 1s) and _done_at (create + 3s) computed from clock.now_unix();
# every status read derives the current taskState from the injectable clock
# and persists the transition back. Anaplan's real task state machine is
# NOT_STARTED → IN_PROGRESS → COMPLETE; failure is expressed (as in the real
# API) as a COMPLETE task whose result.successful is false, via the
# simulator-only simulate_fail flag set at task creation.

def on_run_import(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    import_id = req["params"]["importId"]
    task_id = _gen_task_id()
    created = _create_task(task_id, "IMPORT", import_id, req.get("body"))

    return respond(200, {
        "task": {
            "taskId": task_id,
            "taskState": "CREATED",
            "creationTime": created,
        },
    })

def on_run_export(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    export_id = req["params"]["exportId"]
    task_id = _gen_task_id()
    created = _create_task(task_id, "EXPORT", export_id, req.get("body"))

    return respond(200, {
        "task": {
            "taskId": task_id,
            "taskState": "CREATED",
            "creationTime": created,
        },
    })

def on_get_task(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    task_id = req["params"]["taskId"]
    tc = store_collection("tasks")
    for task in tc.list():
        if task.get("id") == task_id:
            state = _advance_task(task_id, task)
            resp = {
                "taskId": task.get("id", ""),
                "taskState": state,
                "creationTime": task.get("creationTime", ""),
            }
            if state == "COMPLETE":
                resp["completionTime"] = clock.unix_to_rfc3339(task["_done_at"])
                resp["result"] = _task_result(task)
            return respond(200, resp)

    return respond(404, {
        "status": "FAILURE",
        "statusMessage": "Task " + task_id + " not found",
    })

def on_list_exports(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    exports = [
        {"id": "exp001", "name": "Revenue Export", "type": "FLAT", "format": "CSV"},
        {"id": "exp002", "name": "Expense Export", "type": "FLAT", "format": "CSV"},
    ]

    page, next_cursor = _list_page(req, exports)
    paging = {
        "currentPageSize": len(page),
        "offset": _to_int(req.get("query", {}).get("offset", "")),
        "totalSize": len(exports),
    }
    if next_cursor != None:
        paging["nextCursor"] = next_cursor

    return respond(200, {
        "meta": {
            "paging": paging,
        },
        "items": page,
    })

# _gen_task_id generates a unique task ID.
def _gen_task_id():
    seq = store_kv_incr("anaplan", "task_seq") + 1
    return "task-" + str(seq)

# _create_task inserts a new task in the collection with the derive-on-read
# timestamps (body may carry the simulator-only simulate_fail flag) and
# returns the creationTime echoed in the POST response.
def _create_task(task_id, task_type, entity_id, body):
    if body == None:
        body = {}
    now = clock.now_unix()
    created = clock.now_rfc3339()
    tc = store_collection("tasks")
    tc.insert({
        "id": task_id,
        "type": task_type,
        "entityId": entity_id,
        "creationTime": created,
        "state": "NOT_STARTED",
        "_running_at": now + 1,
        "_done_at": now + 3,
        "_fail": body.get("simulate_fail", False),
    })
    return created

# _derive_task_state maps the clock onto Anaplan's real taskState vocabulary.
def _derive_task_state(doc):
    now = clock.now_unix()
    if now < doc["_running_at"]:
        return "NOT_STARTED"
    if now < doc["_done_at"]:
        return "IN_PROGRESS"
    return "COMPLETE"

# _advance_task derives the current state and persists the transition back to
# the collection so repeated polls and any list view agree. Returns the state.
def _advance_task(task_id, doc):
    state = _derive_task_state(doc)
    if doc.get("state") != state:
        doc["state"] = state
        tc = store_collection("tasks")
        tc.update(task_id, doc)
    return state

# _task_result builds the terminal result block. Anaplan reports failure as a
# COMPLETE task with result.successful = false and a nonzero failureCount.
def _task_result(doc):
    total = 100
    if doc.get("_fail", False):
        return {
            "successful": False,
            "totalCount": total,
            "successCount": 0,
            "failureCount": total,
        }
    return {
        "successful": True,
        "totalCount": total,
        "successCount": total,
        "failureCount": 0,
    }
