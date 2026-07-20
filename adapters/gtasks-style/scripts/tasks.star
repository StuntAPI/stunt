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

    return respond(200, {"items": items})

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
        "updated": "2024-01-01T00:00:00.000Z",
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

    task["updated"] = "2024-01-02T00:00:00.000Z"

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

    # Move updates parent and previous (for positioning in the tree).
    parent = body.get("parent", None)
    if parent != None:
        task["parent"] = parent

    previous = body.get("previous", None)
    if previous != None:
        # Assign a new position based on the previous task.
        new_pos = store_kv_incr("gtasks", "move_seq") + 1
        task["position"] = str(new_pos * 1000)

    task["updated"] = "2024-01-03T00:00:00.000Z"

    tc = store_collection("tasks")
    tc.update(task.get("id"), task)

    return respond(200, _task_resource(task))

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
