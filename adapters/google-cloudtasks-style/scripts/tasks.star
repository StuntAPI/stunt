# projects.locations.queues.tasks handlers — CRUD, run, buffer.
#
# Shared helpers are preloaded from scripts/lib.star. A task doc is:
#   {id: full name, queue, task_id, seq, create_unix, create_time,
#    schedule_time, dispatch_deadline?, dispatch_count, response_count,
#    first_attempt?, last_attempt?, message_type: "http"|"appengine",
#    http_request? | app_engine_http_request?}
#
# The sim never opens outbound connections: the worker the task targets is
# not actually called. tasks.run models the dispatch outcome instead — by
# default the unseen worker "succeeds" (2xx) and the task completes; under
# the adapter's failing-worker profile it fails and reschedules per the
# queue's retryConfig (see README).

# _view_from picks responseView from the request body, else the query param
# (gRPC transcoding surfaces proto fields either way; body wins).
def _view_from(req, body):
    v = body.get("responseView", None)
    if v == None:
        v = _query_get(req, "responseView", "")
    return _view_of(v)

# _normalize_http validates and normalizes an httpRequest. Returns
# (message, None) or (None, error).
def _normalize_http(hr):
    if type(hr) != "dict":
        return None, _invalid("task.httpRequest must be an object.")
    url = hr.get("url", "")
    if url == "" or url[:7] != "http://" and url[:8] != "https://":
        return None, _invalid("HttpRequest.url must start with \"http://\" or \"https://\".")
    method = hr.get("httpMethod", None)
    if method == None or method == "HTTP_METHOD_UNSPECIFIED":
        method = "POST"
    if hr.get("body", None) != None and method not in ("POST", "PUT", "PATCH"):
        return None, _invalid("HttpRequest.body is only allowed when httpMethod is POST, PUT, or PATCH.")
    msg = dict(hr)
    msg["httpMethod"] = method
    return msg, None

# _normalize_appengine validates an appEngineHttpRequest the same way.
def _normalize_appengine(ae):
    if type(ae) != "dict":
        return None, _invalid("task.appEngineHttpRequest must be an object.")
    uri = ae.get("relativeUri", "")
    if uri == "" or uri[:1] != "/":
        return None, _invalid("AppEngineHttpRequest.relativeUri must begin with \"/\".")
    method = ae.get("httpMethod", None)
    if method == None or method == "HTTP_METHOD_UNSPECIFIED":
        method = "POST"
    if ae.get("body", None) != None and method not in ("POST", "PUT"):
        return None, _invalid("AppEngineHttpRequest.body is only allowed when httpMethod is POST or PUT.")
    msg = dict(ae)
    msg["httpMethod"] = method
    return msg, None

# _insert_task validates a Task payload and stores it, shared by
# tasks.create and tasks.buffer. Returns (task_doc, None) or (None, error).
def _insert_task(queue_name, task, task_id):
    qc = store_collection("queues")
    qdoc = qc.get(queue_name)
    if qdoc == None:
        return None, _missing("Queue", queue_name)

    hr = task.get("httpRequest", None)
    ae = task.get("appEngineHttpRequest", None)
    if hr != None and ae != None:
        return None, _invalid("The task must set exactly one of httpRequest or appEngineHttpRequest.")
    if hr == None and ae == None:
        return None, _invalid("The task must set one of httpRequest or appEngineHttpRequest.")

    now = clock.now_unix()
    if task_id == None:
        task_id = task.get("name", "")
        if task_id != "":
            prefix = queue_name + "/tasks/"
            if task_id[:len(prefix)] != prefix:
                return None, _invalid("task.name must have the form " + prefix + "TASK_ID.")
            task_id = task_id[len(prefix):]
    if task_id == "":
        task_id = _gen_task_id(store_kv_incr("cloudtasks", "task_seq"))
    if not _task_id_ok(task_id):
        return None, _invalid("The task ID \"" + task_id + "\" is invalid: it must contain only letters, numbers, hyphens, and underscores, and be at most 500 characters.")

    name = _task_name(queue_name, task_id)
    tc = store_collection("tasks")
    if tc.get(name) != None or _tombstone_live(name, now):
        return None, _err(409, "Requested entity already exists", "ALREADY_EXISTS")

    if hr != None:
        msg, merr = _normalize_http(hr)
        if merr != None:
            return None, merr
        message_type = "http"
    else:
        msg, merr = _normalize_appengine(ae)
        if merr != None:
            return None, merr
        message_type = "appengine"

    schedule_time = task.get("scheduleTime", "")
    # Absent or past-due scheduleTime is clamped to now (documented).
    if schedule_time == "" or schedule_time <= clock.now_rfc3339():
        schedule_time = clock.unix_to_rfc3339(now)

    doc = {
        "id": name,
        "queue": queue_name,
        "task_id": task_id,
        "seq": store_kv_incr("cloudtasks", "task_ins"),
        "create_unix": now,
        "create_time": clock.unix_to_rfc3339(now),
        "schedule_time": schedule_time,
        "dispatch_count": 0,
        "response_count": 0,
        "message_type": message_type,
    }
    if message_type == "http":
        doc["http_request"] = msg
    else:
        doc["app_engine_http_request"] = msg
    if task.get("dispatchDeadline", None) != None:
        doc["dispatch_deadline"] = task.get("dispatchDeadline")
    tc.insert(doc)
    return doc, None

# on_create_task adds a task to a queue.
# POST /v2/.../queues/{queue}/tasks  body {task, responseView}
def on_create_task(req):
    err = _require_bearer(req)
    if err != None:
        return err

    queue_name = _queue_name(req["params"].get("project", ""), req["params"].get("location", ""), req["params"].get("queue", ""))
    body, jerr = _json_body(req)
    if jerr != None:
        return jerr
    task = body.get("task", None)
    if task == None or type(task) != "dict":
        return _invalid("CreateTaskRequest.task is required.")

    doc, ierr = _insert_task(queue_name, task, None)
    if ierr != None:
        return ierr
    return respond(200, _task_entity(doc, _view_from(req, body)))

# on_list_tasks lists a queue's tasks in creation order (the real service
# guarantees no particular order; creation order makes runs reproducible).
# GET /v2/.../queues/{queue}/tasks?responseView=&pageSize=&pageToken=
def on_list_tasks(req):
    err = _require_bearer(req)
    if err != None:
        return err

    queue_name = _queue_name(req["params"].get("project", ""), req["params"].get("location", ""), req["params"].get("queue", ""))
    if _find_queue(queue_name) == None:
        return _missing("Queue", queue_name)

    docs = [d for d in store_collection("tasks").list() if d.get("queue", "") == queue_name]
    docs = sorted(docs, key=lambda d: d.get("seq", 0))

    page_size = _to_int(_query_get(req, "pageSize", ""))
    if page_size <= 0 or page_size > 1000:
        page_size = 1000
    page, next_token = paginate(docs, page_size, _query_get(req, "pageToken", ""))
    if page == None:
        return _invalid("Invalid pageToken")
    view = _view_of(_query_get(req, "responseView", ""))
    resp = {"tasks": [_task_entity(d, view) for d in page]}
    if next_token != None:
        resp["nextPageToken"] = next_token
    return respond(200, resp)

# on_get_task returns a task. GET /v2/.../tasks/{task}?responseView=
def on_get_task(req):
    err = _require_bearer(req)
    if err != None:
        return err
    name = _task_name(_queue_name(req["params"].get("project", ""), req["params"].get("location", ""), req["params"].get("queue", "")), req["params"].get("task", ""))
    doc = store_collection("tasks").get(name)
    if doc == None:
        return _missing("Task", name)
    return respond(200, _task_entity(doc, _view_of(_query_get(req, "responseView", ""))))

# on_delete_task deletes a task; the name is tombstoned (the real service
# holds deleted IDs for up to 24h before reuse is allowed).
# DELETE /v2/.../tasks/{task}
def on_delete_task(req):
    err = _require_bearer(req)
    if err != None:
        return err
    name = _task_name(_queue_name(req["params"].get("project", ""), req["params"].get("location", ""), req["params"].get("queue", "")), req["params"].get("task", ""))
    doc = store_collection("tasks").get(name)
    if doc == None:
        return _missing("Task", name)
    store_collection("tasks").delete(name)
    _tombstone_set(name, clock.now_unix())
    return respond(200, {})

# on_task_verb dispatches the POST colon verbs on the task path:
# {task}:run and {taskId}:buffer.
def on_task_verb(req):
    tv = req["params"].get("task_verb", "")
    colon = tv.find(":")
    if colon < 0:
        return _err(404, "Method not found.", "NOT_FOUND")
    verb = tv[colon + 1:]
    task_id = tv[:colon]
    req["params"]["task"] = task_id
    if verb == "run":
        return _run_task(req)
    if verb == "buffer":
        return _buffer(req, task_id)
    return _err(404, "Method not found.", "NOT_FOUND")

# on_buffer_task handles the generated-ID form of tasks.buffer (no task ID
# in the path); the body's HttpBody bytes become the task's HTTP payload.
# POST /v2/.../queues/{queue}/tasks:buffer  body {body: HttpBody}
def on_buffer_task(req):
    err = _require_bearer(req)
    if err != None:
        return err
    return _buffer(req, None)

# _buffer implements tasks.buffer: build an HTTP task from the queue's
# httpTarget (uriOverride for the URL, httpMethod defaulting to POST) with
# the HttpBody payload.
def _buffer(req, task_id):
    queue_name = _queue_name(req["params"].get("project", ""), req["params"].get("location", ""), req["params"].get("queue", ""))
    qdoc = _find_queue(queue_name)
    if qdoc == None:
        return _missing("Queue", queue_name)
    ht = qdoc.get("http_target", None)
    if ht == None:
        return _invalid("The queue does not have an HTTP target.")

    body, jerr = _json_body(req)
    if jerr != None:
        return jerr
    http_body = body.get("body", None)
    payload = ""
    content_type = ""
    if http_body != None and type(http_body) == "dict":
        payload = http_body.get("data", "")
        content_type = http_body.get("contentType", "")
        if payload == None:
            payload = ""

    uri = ""
    uo = ht.get("uriOverride", None)
    if uo != None:
        scheme = uo.get("scheme", "HTTPS")
        if scheme == "HTTP":
            scheme = "http"
        else:
            scheme = "https"
        host = uo.get("host", "")
        if host == "":
            return _invalid("HttpTarget.uriOverride.host is required for tasks.buffer.")
        port = str(uo.get("port", ""))
        uri = scheme + "://" + host
        if port != "" and port != "0":
            uri = uri + ":" + port
        po = uo.get("pathOverride", None)
        if po != None and po.get("path", "") != "":
            uri = uri + po.get("path")
        qo = uo.get("queryOverride", None)
        if qo != None and qo.get("queryParams", "") != "":
            uri = uri + "?" + qo.get("queryParams")

    hr = {"url": uri}
    method = ht.get("httpMethod", None)
    if method == None or method == "HTTP_METHOD_UNSPECIFIED":
        method = "POST"
    hr["httpMethod"] = method
    headers = {}
    if content_type != "":
        headers["Content-Type"] = content_type
    if len(headers) > 0:
        hr["headers"] = headers
    if payload != "":
        hr["body"] = payload

    task = {"httpRequest": hr}
    doc, ierr = _insert_task(queue_name, task, task_id)
    if ierr != None:
        return ierr
    return respond(200, {"task": _task_entity(doc, "FULL")})

# _run_task forces a dispatch now, ignoring scheduleTime, queue state, and
# rate limits (the documented RunTask semantics).
#
# Outcome model: the unseen worker answers 2xx by default, so the task
# completes and is deleted (matching "the task will be deleted" on success
# and NOT_FOUND on later runs). Under the failing-worker profile the worker
# answers 500: the attempt is recorded, and the task reschedules per the
# queue's retryConfig until it exhausts maxAttempts/maxRetryDuration, at
# which point it is permanently failed (deleted with a tombstone).
def _run_task(req):
    name = _task_name(_queue_name(req["params"].get("project", ""), req["params"].get("location", ""), req["params"].get("queue", "")), req["params"].get("task", ""))
    tc = store_collection("tasks")
    doc = tc.get(name)
    if doc == None:
        return _missing("Task", name)
    body, jerr = _json_body(req)
    if jerr != None:
        return jerr

    now = clock.now_unix()
    now_str = clock.unix_to_rfc3339(now)
    doc["dispatch_count"] = int(doc.get("dispatch_count", 0)) + 1
    scheduled = doc.get("schedule_time", "")
    if doc.get("first_attempt", None) == None:
        doc["first_attempt"] = {"scheduleTime": scheduled, "dispatchTime": now_str}
        doc["first_dispatch_unix"] = now

    if profile_active() == "failing-worker":
        qdoc = _find_queue(doc["queue"])
        retry = qdoc["retry_config"] if qdoc != None else dict(_DEFAULT_RETRY)
        doc["response_count"] = int(doc.get("response_count", 0)) + 1
        doc["last_attempt"] = {
            "scheduleTime": scheduled,
            "dispatchTime": now_str,
            "responseTime": now_str,
            "responseStatus": {"code": 500, "message": "simulated worker failure (profile: failing-worker)"},
        }
        attempts = doc["dispatch_count"]
        max_attempts = retry.get("maxAttempts", 100)
        age_limit = _parse_duration(retry.get("maxRetryDuration"))
        expired = age_limit != None and age_limit > 0.0 and (now - doc.get("first_dispatch_unix", now)) >= int(age_limit)
        if (max_attempts != -1 and attempts >= max_attempts) or expired:
            rendered = _task_entity(doc, _view_from(req, body))
            tc.delete(name)
            _tombstone_set(name, now)
            return respond(200, rendered)
        delay = _retry_delay(retry, attempts)
        doc["schedule_time"] = clock.unix_to_rfc3339(now + delay)
        tc.update(name, doc)
        return respond(200, _task_entity(doc, _view_from(req, body)))

    # Success path: the returned task carries the post-dispatch,
    # pre-response status (dispatchTime set, no responseStatus).
    doc["last_attempt"] = {"scheduleTime": scheduled, "dispatchTime": now_str}
    rendered = _task_entity(doc, _view_from(req, body))
    tc.delete(name)
    _tombstone_set(name, now)
    return respond(200, rendered)
