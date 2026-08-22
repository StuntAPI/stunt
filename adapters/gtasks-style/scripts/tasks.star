# Task handlers — Google Tasks API task CRUD + move.
#
# GET    /tasks/v1/lists/{tasklistId}/tasks               → list tasks
# POST   /tasks/v1/lists/{tasklistId}/tasks               → create task
# GET    /tasks/v1/lists/{tasklistId}/tasks/{taskId}       → get task
# PUT    /tasks/v1/lists/{tasklistId}/tasks/{taskId}       → update task
# PATCH  /tasks/v1/lists/{tasklistId}/tasks/{taskId}       → update task
# DELETE /tasks/v1/lists/{tasklistId}/tasks/{taskId}       → delete task
# POST   /tasks/v1/lists/{tasklistId}/tasks/{taskId}/move  → move/reorder task

def on_list_tasks(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    list_id = req["params"]["tasklistId"]
    tc = store_collection("tasks")
    items = []
    for task in tc.list():
        if task.get("tasklistId") == list_id:
            items.append(_task_resource(task))

    # Apply the real tasks.list filter params before paging.
    items = _apply_task_filters(req, items)

    # Apply Google Tasks pagination (maxResults + pageToken) after filtering.
    page, next_cursor = _list_page(req, items)
    if page == None:
        return _g_err(400, "Invalid pageToken", "INVALID_ARGUMENT")

    result = {"items": page}
    if next_cursor != None:
        result["nextPageToken"] = next_cursor

    return respond(200, result)

def on_create_task(req):
    err = _require_bearer(req)
    if err != None:
        return err

    list_id = req["params"]["tasklistId"]
    body = req["body"]
    if body == None:
        body = {}

    title = body.get("title", "")
    if title == None:
        title = ""
    notes = body.get("notes", "")
    if notes == None:
        notes = ""
    due = body.get("due", "")
    if due == None:
        due = ""

    seq = store_kv_incr("gtasks", "task_seq") + 1
    task_id = _gen_id("task", seq)

    task = {
        "id": task_id,
        "tasklistId": list_id,
        "title": title,
        "notes": notes,
        "status": "needsAction",
        "due": due,
        "completed": None,
        "parent": None,
        "position": str(seq * 1000),
        "updated": _now_ms(),
        "selfLink": "https://www.googleapis.com/tasks/v1/lists/" + list_id + "/tasks/" + task_id,
    }

    tc = store_collection("tasks")
    tc.insert(task)

    return respond(200, _task_resource(task))

def on_get_task(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    list_id = req["params"]["tasklistId"]
    task_id = req["params"]["taskId"]
    task = _find_task(list_id, task_id)
    if task == None:
        return _g_err(404, "Task not found.", "NOT_FOUND")

    return respond(200, _task_resource(task))

def on_update_task(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    list_id = req["params"]["tasklistId"]
    task_id = req["params"]["taskId"]
    task = _find_task(list_id, task_id)
    if task == None:
        return _g_err(404, "Task not found.", "NOT_FOUND")

    body = req["body"]
    if body == None:
        body = {}

    # Update fields.
    for field in ["title", "notes", "status", "due", "completed"]:
        val = body.get(field, None)
        if val != None:
            task[field] = val

    # The real API derives completed from status: flipping to completed stamps
    # it, flipping back to needsAction clears it (clients never send it).
    status = task.get("status", None)
    if status == "completed" and task.get("completed", None) == None:
        task["completed"] = _now_ms()
    elif status != "completed":
        task["completed"] = None

    task["updated"] = _now_ms()

    tc = store_collection("tasks")
    tc.update(task.get("id"), task)

    return respond(200, _task_resource(task))

def on_delete_task(req):
    err = _require_bearer(req)
    if err != None:
        return err

    list_id = req["params"]["tasklistId"]
    task_id = req["params"]["taskId"]
    task = _find_task(list_id, task_id)
    if task == None:
        return _g_err(404, "Task not found.", "NOT_FOUND")

    tc = store_collection("tasks")
    tc.delete(task.get("id"))

    return respond(204)

def on_move_task(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    list_id = req["params"]["tasklistId"]
    task_id = req["params"]["taskId"]
    task = _find_task(list_id, task_id)
    if task == None:
        return _g_err(404, "Task not found.", "NOT_FOUND")

    body = req["body"]
    if body == None:
        body = {}

    # The real move endpoint takes parent/previous as QUERY params; the body
    # form is kept as a fallback so direct curl callers keep working.
    parent = req["query"].get("parent", None)
    if parent == None or parent == "":
        parent = body.get("parent", None)
    if parent != None:
        task["parent"] = parent

    previous = req["query"].get("previous", None)
    if previous == None or previous == "":
        previous = body.get("previous", None)
    if previous != None and previous != "":
        # Assign a new position based on the previous task. It draws from the
        # same counter as create so every position stays unique — a private
        # move counter can collide with a task's own position and report a
        # no-op reorder.
        new_pos = store_kv_incr("gtasks", "task_seq") + 1
        task["position"] = str(new_pos * 1000)

    task["updated"] = _now_ms()

    tc = store_collection("tasks")
    tc.update(task.get("id"), task)

    return respond(200, _task_resource(task))

# _apply_task_filters maps the real tasks.list query params to query_select
# clauses, applied before paging like the real API. showCompleted defaults to
# true, so only "false" filters (status needsAction). dueMin/dueMax compare
# the RFC3339 due string (tasks without a due date store "" and never match).
# completedMin/completedMax exclude uncompleted tasks (completed is None),
# like the real API. updatedMin compares the stored RFC3339 updated stamp.
# q is a free-text match over title/notes — its OR shape is not expressible
# as AND'ed triples, so it runs as a manual scan after query_select.
def _apply_task_filters(req, items):
    f = []

    show_completed = req["query"].get("showCompleted", "")
    if show_completed != None and show_completed == "false":
        f.append(["status", "=", "needsAction"])

    due_min = req["query"].get("dueMin", "")
    if due_min != None and due_min != "":
        f.append(["due", ">=", due_min])
    due_max = req["query"].get("dueMax", "")
    if due_max != None and due_max != "":
        f.append(["due", "<=", due_max])

    completed_min = req["query"].get("completedMin", "")
    if completed_min != None and completed_min != "":
        f.append(["completed", "!=", None])
        f.append(["completed", ">=", completed_min])
    completed_max = req["query"].get("completedMax", "")
    if completed_max != None and completed_max != "":
        f.append(["completed", "!=", None])
        f.append(["completed", "<=", completed_max])

    updated_min = req["query"].get("updatedMin", "")
    if updated_min != None and updated_min != "":
        f.append(["updated", ">", updated_min])

    if len(f) > 0:
        items = query_select(items, f)

    q = req["query"].get("q", "")
    if q != None and q != "":
        needle = q.lower()
        filtered = []
        for t in items:
            if needle in t.get("title", "").lower() or needle in t.get("notes", "").lower():
                filtered.append(t)
        items = filtered

    return items

# _find_task returns a task by (list_id, task_id), or None.
def _find_task(list_id, task_id):
    tc = store_collection("tasks")
    for task in tc.list():
        if task.get("tasklistId") == list_id and task.get("id") == task_id:
            return task
    return None

# _task_resource builds the API response shape for a task.
def _task_resource(task):
    return {
        "id": task.get("id", ""),
        "title": task.get("title", ""),
        "notes": task.get("notes", ""),
        "status": task.get("status", "needsAction"),
        "due": task.get("due", ""),
        "completed": task.get("completed", None),
        "parent": task.get("parent", None),
        "position": task.get("position", ""),
        "updated": task.get("updated", ""),
        "selfLink": task.get("selfLink", ""),
    }
