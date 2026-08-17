# Task handlers — Anaplan API async import/export tasks.
#
# POST /2/0/workspaces/{wid}/models/{mid}/imports/{importId}/tasks → run import
# POST /2/0/workspaces/{wid}/models/{mid}/imports/{importId}/jobs  → run import (alias)
# POST /2/0/workspaces/{wid}/models/{mid}/exports/{exportId}/tasks → run export
# POST /2/0/workspaces/{wid}/models/{mid}/exports/{exportId}/jobs  → run export (alias)
# GET  /2/0/workspaces/{wid}/models/{mid}/tasks/{taskId}           → task status
# GET  /2/0/workspaces/{wid}/models/{mid}/exports                  → list exports
#
# Async lifecycle is derive-on-read: the task doc stores _running_at
# (create + 1s) and _done_at (create + 3s) computed from clock.now_unix();
# every status read derives the current taskState from the injectable clock
# and persists the transition back. Anaplan's real task state machine is
# NOT_STARTED → IN_PROGRESS → COMPLETE; failure is expressed (as in the real
# API) as a COMPLETE task whose result.successful is false, via the
# simulator-only simulate_fail flag set at task creation.
#
# An import READS the uploaded file content and applies it to the modeled
# data: the first read past the completion window parses the import's file
# (CSV rows via _csv_rows), stores the rows in the modelData collection, and
# freezes the result counts (row count) on the task. Exports are symmetric:
# on completion they render the model's current data rows to CSV in the blob
# store, where GET .../files/{exportFileId} downloads them.

# Shared helpers (_require_auth, _to_int, _num, _list_page, _file_key,
# _BLOB_NS, _seed_catalog, _csv_rows, _csv_render) are preloaded from
# scripts/lib.star.

def on_run_import(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    _seed_catalog()

    ws = req["params"]["workspaceId"]
    mid = req["params"]["modelId"]
    import_id = req["params"]["importId"]

    # The import must exist in the model's action catalog.
    imp = store_collection("imports").get(_file_key(ws, mid, import_id))
    if imp == None:
        return respond(404, {
            "status": "FAILURE",
            "statusMessage": "Import " + import_id + " not found",
        })

    task_id = _gen_task_id()
    created = _create_task(task_id, "IMPORT", import_id, req.get("body"))
    tc = store_collection("tasks")
    doc = tc.get(task_id)
    doc["workspaceId"] = ws
    doc["modelId"] = mid
    doc["fileId"] = imp.get("fileId", "")
    tc.update(task_id, doc)

    return respond(200, {
        "task": {
            "taskId": task_id,
            "taskState": "CREATED",
            "creationTime": created,
        },
    })

def on_run_export(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    _seed_catalog()

    ws = req["params"]["workspaceId"]
    mid = req["params"]["modelId"]
    export_id = req["params"]["exportId"]

    # The export must exist in the model's action catalog.
    exp = store_collection("exports").get(_file_key(ws, mid, export_id))
    if exp == None:
        return respond(404, {
            "status": "FAILURE",
            "statusMessage": "Export " + export_id + " not found",
        })

    task_id = _gen_task_id()
    created = _create_task(task_id, "EXPORT", export_id, req.get("body"))
    tc = store_collection("tasks")
    doc = tc.get(task_id)
    doc["workspaceId"] = ws
    doc["modelId"] = mid
    doc["fileId"] = exp.get("fileId", "")
    tc.update(task_id, doc)

    return respond(200, {
        "task": {
            "taskId": task_id,
            "taskState": "CREATED",
            "creationTime": created,
        },
    })

def on_get_task(req):
    _, err = _require_auth(req)
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
                resp["completionTime"] = clock.unix_to_rfc3339(_num(task.get("_done_at", 0)))
                resp["result"] = _task_result(task)
            return respond(200, resp)

    return respond(404, {
        "status": "FAILURE",
        "statusMessage": "Task " + task_id + " not found",
    })

def on_list_exports(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    _seed_catalog()

    ws = req["params"]["workspaceId"]
    mid = req["params"]["modelId"]
    prefix = ws + ":" + mid + ":"

    exports = []
    for e in store_collection("exports").list():
        if e.get("id", "")[:len(prefix)] != prefix:
            continue
        exports.append({
            "id": e.get("entityId", ""),
            "name": e.get("name", ""),
            "type": e.get("type", "FLAT"),
            "format": e.get("format", "CSV"),
        })

    page, next_cursor = _list_page(req, exports)
    if page == None:
        return respond(400, {"status": "FAILURE", "statusMessage": "Invalid offset parameter."})
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
    if now < _num(doc.get("_running_at", 0)):
        return "NOT_STARTED"
    if now < _num(doc.get("_done_at", 0)):
        return "IN_PROGRESS"
    return "COMPLETE"

# _advance_task derives the current state and persists the transition back to
# the collection so repeated polls and any list view agree. The first
# transition into COMPLETE also finalizes the task's side effects (import
# applies file rows to the model data; export renders its output file) and
# freezes the result. Returns the state.
def _advance_task(task_id, doc):
    state = _derive_task_state(doc)
    if doc.get("state") != state:
        doc["state"] = state
        if state == "COMPLETE":
            _finalize_task(doc)
        tc = store_collection("tasks")
        tc.update(task_id, doc)
    return state

# _model_rows reads the model's applied data rows (imports) from the
# modelData collection.
def _model_rows(ws, mid):
    dc = store_collection("modelData")
    doc = dc.get("data:" + ws + ":" + mid)
    if doc == None:
        return []
    rows = doc.get("rows", [])
    if rows == None:
        return []
    return rows

# _finalize_task performs the task's terminal side effects exactly once:
#   IMPORT → parse the import's uploaded file, store the rows as the model's
#            data, and freeze result counts from the row count;
#   EXPORT → render the model's current data rows to CSV in the blob store
#            (the file the export task makes downloadable).
# Failure injection overrides the success flag without touching the data.
def _finalize_task(doc):
    task_type = doc.get("type", "")
    ws = doc.get("workspaceId", "")
    mid = doc.get("modelId", "")
    fail = doc.get("_fail", False)

    if task_type == "IMPORT":
        b = store_blob(_BLOB_NS)
        content = b.get(_blob_key(ws, mid, doc.get("fileId", "")))
        rows = _csv_rows(content)
        if len(rows) > 0:
            dc = store_collection("modelData")
            ddoc = dc.get("data:" + ws + ":" + mid)
            if ddoc == None:
                dc.insert({
                    "id": "data:" + ws + ":" + mid,
                    "columns": _csv_header(content),
                    "rows": rows,
                })
            else:
                ddoc["columns"] = _csv_header(content)
                ddoc["rows"] = rows
                dc.update("data:" + ws + ":" + mid, ddoc)
        doc["_result"] = _import_result(rows, fail, content)
        return

    if task_type == "EXPORT":
        dc = store_collection("modelData")
        ddoc = dc.get("data:" + ws + ":" + mid)
        rows = _model_rows(ws, mid)
        columns = []
        if ddoc != None and ddoc.get("columns", None) != None:
            columns = ddoc["columns"]
        csv = _csv_render(rows, columns)
        if csv == "":
            csv = "No Data\n"
        b = store_blob(_BLOB_NS)
        b.put(_blob_key(ws, mid, doc.get("fileId", "")), csv, "text/csv")
        fc = store_collection("files")
        key = _file_key(ws, mid, doc.get("fileId", ""))
        fdoc = fc.get(key)
        if fdoc == None:
            fc.insert({
                "id": key,
                "fileId": doc.get("fileId", ""),
                "name": doc.get("entityId", "") + ".csv",
                "contentType": "text/csv",
                "size": len(csv),
                "chunks": [{"index": 0, "offset": 0, "size": len(csv)}],
            })
        else:
            fdoc["size"] = len(csv)
            fdoc["chunks"] = [{"index": 0, "offset": 0, "size": len(csv)}]
            fc.update(key, fdoc)
        doc["_result"] = {
            "successful": True,
            "totalCount": len(rows),
            "successCount": len(rows),
            "failureCount": 0,
        }
        return

    # Unknown task type: keep the legacy synthetic counts.
    doc["_result"] = {
        "successful": not fail,
        "totalCount": 100,
        "successCount": 0 if fail else 100,
        "failureCount": 100 if fail else 0,
    }

# _import_result builds the frozen result block for an import task from the
# rows parsed out of its uploaded file. Anaplan reports failure as a COMPLETE
# task with result.successful = false and a nonzero failureCount.
def _import_result(rows, fail, content):
    total = len(rows)
    if content == None or content == "":
        # No file content was ever uploaded: the import cannot run.
        return {
            "successful": False,
            "totalCount": 0,
            "successCount": 0,
            "failureCount": 1,
            "failureReason": "No file contents uploaded for this import",
        }
    if fail:
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

# _task_result builds the terminal result block: the frozen _result stamped
# at completion when present, else derived on the fly.
def _task_result(doc):
    frozen = doc.get("_result", None)
    if frozen != None:
        return frozen
    return _import_result([], doc.get("_fail", False), "seeded")
