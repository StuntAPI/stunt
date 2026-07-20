# Task handlers — Anaplan API async import/export tasks.
#
# POST /2/0/workspaces/{wid}/models/{mid}/imports/{importId}/tasks → start import
# POST /2/0/workspaces/{wid}/models/{mid}/exports/{exportId}/tasks → start export
# GET  /2/0/workspaces/{wid}/models/{mid}/tasks/{taskId}             → task status
# GET  /2/0/workspaces/{wid}/models/{mid}/exports                    → list exports

def on_run_import(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    import_id = req["params"]["importId"]
    task_id = _gen_task_id()
    _create_task(task_id, "IMPORT", import_id)

    return respond(200, {
        "task": {
            "taskId": task_id,
            "taskState": "CREATED",
            "creationTime": "2024-01-01T00:00:00.000Z",
        },
    })

def on_run_export(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    export_id = req["params"]["exportId"]
    task_id = _gen_task_id()
    _create_task(task_id, "EXPORT", export_id)

    return respond(200, {
        "task": {
            "taskId": task_id,
            "taskState": "CREATED",
            "creationTime": "2024-01-01T00:00:00.000Z",
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
            # Simulate task progression: always return COMPLETE.
            return respond(200, {
                "taskId": task.get("id", ""),
                "taskState": "COMPLETE",
                "creationTime": task.get("creationTime", ""),
                "completionTime": "2024-01-01T00:00:05.000Z",
                "result": {
                    "successful": True,
                    "totalCount": 100,
                    "successCount": 100,
                    "failureCount": 0,
                },
            })

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

    return respond(200, {
        "meta": {
            "paging": {
                "currentPageSize": len(exports),
                "offset": 0,
                "totalSize": len(exports),
            },
        },
        "items": exports,
    })

# _gen_task_id generates a unique task ID.
def _gen_task_id():
    seq = store_kv_incr("anaplan", "task_seq") + 1
    return "task-" + str(seq)

# _create_task inserts a new task in the collection.
def _create_task(task_id, task_type, entity_id):
    tc = store_collection("tasks")
    tc.insert({
        "id": task_id,
        "type": task_type,
        "entityId": entity_id,
        "creationTime": "2024-01-01T00:00:00.000Z",
        "state": "CREATED",
    })
