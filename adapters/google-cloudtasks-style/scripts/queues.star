# projects.locations.queues handlers — CRUD, pause/resume/purge, IAM.
#
# Shared helpers are preloaded from scripts/lib.star. A queue doc is:
#   {id: full name, project, location, queue_id, state,
#    rate_limits, retry_config, http_target?,
#    app_engine_routing_override?, stackdriver_logging_config?,
#    purge_time_unix?}

# _delete_queue_tasks removes every task belonging to the queue (used by
# queues.delete; purge filters by create time instead).
def _delete_queue_tasks(queue_name):
    tc = store_collection("tasks")
    for d in tc.list():
        if d.get("queue", "") == queue_name:
            tc.delete(d["id"])

# on_create_queue creates a queue with the service's default rate limits
# and retry config where the request leaves them unset.
# POST /v2/projects/{project}/locations/{location}/queues?queueId=...
def on_create_queue(req):
    err = _require_bearer(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    location = req["params"].get("location", "")
    if not _location_known(location):
        return _missing("Location", "projects/" + project + "/locations/" + location)

    body, jerr = _json_body(req)
    if jerr != None:
        return jerr
    queue_id = _query_get(req, "queueId", "")
    provided_name = body.get("name", "")
    if queue_id == "":
        if provided_name == "":
            return _invalid("queueId is required unless queue.name is set.")
        prefix = "projects/" + project + "/locations/" + location + "/queues/"
        if provided_name[:len(prefix)] != prefix:
            return _invalid("queue.name must have the form " + prefix + "QUEUE_ID.")
        queue_id = provided_name[len(prefix):]
    if not _queue_id_ok(queue_id):
        return _invalid("The queue ID \"" + queue_id + "\" is invalid: it must contain only letters, numbers, and hyphens, and be at most 100 characters.")
    name = _queue_name(project, location, queue_id)
    if provided_name != "" and provided_name != name:
        return _invalid("queue.name (" + provided_name + ") does not match the request URL (" + name + ").")

    if _find_queue(name) != None:
        return _err(409, "Queue \"" + name + "\" already exists.", "ALREADY_EXISTS")

    rate, rerr = _coerce_rate(body.get("rateLimits", None))
    if rerr != None:
        return rerr
    retry, retry_err = _coerce_retry(body.get("retryConfig", None))
    if retry_err != None:
        return retry_err

    doc = {
        "id": name,
        "project": project,
        "location": location,
        "queue_id": queue_id,
        "state": "RUNNING",
        "rate_limits": rate,
        "retry_config": retry,
    }
    for field in ("httpTarget", "appEngineRoutingOverride", "stackdriverLoggingConfig"):
        if body.get(field, None) != None:
            doc[_snake(field)] = body.get(field)
    store_collection("queues").insert(doc)
    return respond(200, _queue_entity(doc))

def _snake(field):
    # camelCase -> snake_case for the four whitelisted fields (hand-rolled:
    # these are the only mappings needed).
    if field == "httpTarget":
        return "http_target"
    if field == "appEngineRoutingOverride":
        return "app_engine_routing_override"
    return "stackdriver_logging_config"

# on_list_queues lists queues in lexicographical order, with a small filter
# evaluator (name/state equality or containment).
# GET /v2/projects/{project}/locations/{location}/queues
def on_list_queues(req):
    err = _require_bearer(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    location = req["params"].get("location", "")

    # Trailing slash: "locations/us-central1" must not prefix-match
    # "locations/us-central12/...".
    parent = "projects/" + project + "/locations/" + location + "/"
    docs = [d for d in store_collection("queues").list() if d["id"][:len(parent)] == parent]
    docs = sorted(docs, key=lambda d: d["id"])

    filt = _query_get(req, "filter", "")
    if filt != "":
        docs, ferr = _filter_queues(docs, filt)
        if ferr != None:
            return ferr

    page_size = _to_int(_query_get(req, "pageSize", ""))
    if page_size <= 0 or page_size > 9800:
        page_size = 9800
    page, next_token = paginate(docs, page_size, _query_get(req, "pageToken", ""))
    if page == None:
        return _invalid("Invalid pageToken")
    resp = {"queues": [_queue_entity(d) for d in page]}
    if next_token != None:
        resp["nextPageToken"] = next_token
    return respond(200, resp)

# _filter_queues applies "field = value" / "field != value" / "field: value"
# (containment) on the string fields name and state — the subset the real
# filter grammar commonly sees for queues.
def _filter_queues(docs, filt):
    op = None
    field = None
    value = None
    for candidate in ("!=", "= ", ":", "="):
        idx = filt.find(candidate)
        if idx > 0:
            field = filt[:idx].strip()
            value = filt[idx + len(candidate):].strip()
            if candidate == "= ":
                candidate = "="
            op = candidate
            break
    if op == None or field == "":
        return None, _invalid("Invalid filter: " + filt)
    attr = None
    if field == "name":
        attr = "id"
    elif field == "state":
        attr = "state"
    else:
        return None, _invalid("Unsupported filter field: " + field)
    out = []
    for d in docs:
        actual = str(d.get(attr, ""))
        keep = False
        if op == "=":
            keep = actual == value
        elif op == "!=":
            keep = actual != value
        else:
            keep = _contains(actual, value)
        if keep:
            out.append(d)
    return out, None

# on_get_queue returns a queue. GET /v2/.../queues/{queue}
def on_get_queue(req):
    err = _require_bearer(req)
    if err != None:
        return err
    name = _queue_name(req["params"].get("project", ""), req["params"].get("location", ""), req["params"].get("queue", ""))
    doc = _find_queue(name)
    if doc == None:
        return _missing("Queue", name)
    return respond(200, _queue_entity(doc))

# on_patch_queue updates mutable fields per the updateMask (AIP-134: a bare
# path like "rateLimits" replaces the message; a dotted path like
# "rateLimits.maxDispatchesPerSecond" sets one leaf). state is output only.
# PATCH /v2/.../queues/{queue}?updateMask=...
def on_patch_queue(req):
    err = _require_bearer(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    location = req["params"].get("location", "")
    queue = req["params"].get("queue", "")
    name = _queue_name(project, location, queue)
    doc = _find_queue(name)
    if doc == None:
        return _missing("Queue", name)

    body, jerr = _json_body(req)
    if jerr != None:
        return jerr
    provided_name = body.get("name", "")
    if provided_name != "" and provided_name != name:
        return _invalid("Queue.name is immutable (" + provided_name + " vs " + name + ").")

    mask = _query_get(req, "updateMask", "")
    entries = []
    if mask != "":
        entries = [m.strip() for m in mask.split(",") if m.strip() != ""]
    if "state" in entries:
        return _invalid("Queue.state is output only; use queues.pause / queues.resume.")
    if entries == [] or entries == ["*"]:
        entries = [f for f in ("httpTarget", "appEngineRoutingOverride", "rateLimits", "retryConfig", "stackdriverLoggingConfig") if f in body]

    rate = dict(doc["rate_limits"])
    retry = dict(doc["retry_config"])
    for e in entries:
        top = e.split(".")[0]
        if top == "rateLimits" or top == "retryConfig":
            target = rate if top == "rateLimits" else retry
            src = body.get(top, None)
            if src == None:
                continue
            for k in ("maxDispatchesPerSecond", "maxBurstSize", "maxConcurrentDispatches", "maxAttempts", "maxRetryDuration", "minBackoff", "maxBackoff", "maxDoublings"):
                if k in target and src.get(k, None) != None and (e == top or e == top + "." + k):
                    target[k] = src.get(k)
        elif top == "httpTarget" or top == "appEngineRoutingOverride" or top == "stackdriverLoggingConfig":
            v = body.get(top, None)
            key = _snake(top)
            if v == None:
                doc.pop(key, None)
            else:
                doc[key] = v
        elif top == "name":
            continue
        else:
            return _invalid("Unknown field in updateMask: " + e)

    coerced_rate, rerr = _coerce_rate(rate)
    if rerr != None:
        return rerr
    coerced_retry, retry_err = _coerce_retry(retry)
    if retry_err != None:
        return retry_err
    doc["rate_limits"] = coerced_rate
    doc["retry_config"] = coerced_retry

    store_collection("queues").update(name, doc)
    return respond(200, _queue_entity(doc))

# on_delete_queue deletes a queue and its tasks.
# DELETE /v2/.../queues/{queue}
def on_delete_queue(req):
    err = _require_bearer(req)
    if err != None:
        return err
    name = _queue_name(req["params"].get("project", ""), req["params"].get("location", ""), req["params"].get("queue", ""))
    doc = _find_queue(name)
    if doc == None:
        return _missing("Queue", name)
    store_collection("queues").delete(name)
    _delete_queue_tasks(name)
    store_collection("iam_policies").delete(name)
    return respond(200, {})

# on_queue_verb dispatches the POST colon verbs: :pause :resume :purge
# :getIamPolicy :setIamPolicy :testIamPermissions. The verb rides the last
# path segment ({queue_verb} = "<queueId>:<verb>").
def on_queue_verb(req):
    qv = req["params"].get("queue_verb", "")
    colon = qv.find(":")
    if colon < 0:
        return _err(404, "Method not found.", "NOT_FOUND")
    verb = qv[colon + 1:]
    req["params"]["queue"] = qv[:colon]
    if verb == "pause":
        return _set_queue_state(req, "PAUSED")
    if verb == "resume":
        return _set_queue_state(req, "RUNNING")
    if verb == "purge":
        return _purge_queue(req)
    if verb == "getIamPolicy":
        return _get_iam_policy(req)
    if verb == "setIamPolicy":
        return _set_iam_policy(req)
    if verb == "testIamPermissions":
        return _test_iam_permissions(req)
    return _err(404, "Method not found.", "NOT_FOUND")

def _set_queue_state(req, state):
    err = _require_bearer(req)
    if err != None:
        return err
    name = _queue_name(req["params"].get("project", ""), req["params"].get("location", ""), req["params"].get("queue", ""))
    doc = _find_queue(name)
    if doc == None:
        return _missing("Queue", name)
    doc["state"] = state
    store_collection("queues").update(name, doc)
    return respond(200, _queue_entity(doc))

# _purge_queue deletes every task created before the purge moment and
# stamps purgeTime; tasks created after the purge survive (the documented
# semantics — purge is not a wipe of the queue's whole life).
def _purge_queue(req):
    err = _require_bearer(req)
    if err != None:
        return err
    name = _queue_name(req["params"].get("project", ""), req["params"].get("location", ""), req["params"].get("queue", ""))
    doc = _find_queue(name)
    if doc == None:
        return _missing("Queue", name)
    now = clock.now_unix()
    tc = store_collection("tasks")
    for d in tc.list():
        if d.get("queue", "") == name and d.get("create_unix", 0) <= now:
            tc.delete(d["id"])
    doc["purge_time_unix"] = now
    store_collection("queues").update(name, doc)
    return respond(200, {})

# --- IAM (queues carry Cloud Tasks policies) ---

def _fresh_etag():
    n = store_kv_incr("cloudtasks", "etag_seq")
    return "Bw" + crypto.base64_encode("etag-" + str(n))

def _get_iam_policy(req):
    err = _require_bearer(req)
    if err != None:
        return err
    name = _queue_name(req["params"].get("project", ""), req["params"].get("location", ""), req["params"].get("queue", ""))
    if _find_queue(name) == None:
        return _missing("Queue", name)
    stored = store_collection("iam_policies").get(name)
    if stored == None:
        # Materialize the empty default so its etag is stable across reads.
        stored = {"id": name, "version": 1, "bindings": [], "etag": _fresh_etag()}
        _upsert(store_collection("iam_policies"), stored)
    return respond(200, {"version": stored.get("version", 1), "bindings": stored.get("bindings", []), "etag": stored.get("etag", "")})

def _set_iam_policy(req):
    err = _require_bearer(req)
    if err != None:
        return err
    name = _queue_name(req["params"].get("project", ""), req["params"].get("location", ""), req["params"].get("queue", ""))
    if _find_queue(name) == None:
        return _missing("Queue", name)
    body, jerr = _json_body(req)
    if jerr != None:
        return jerr
    policy = body.get("policy", None)
    if policy == None or type(policy) != "dict":
        return _invalid("SetIamPolicyRequest.policy is required.")
    bindings = policy.get("bindings", [])
    if bindings != None and type(bindings) != "list":
        return _invalid("Policy.bindings must be a list.")
    if bindings == None:
        bindings = []

    stored = store_collection("iam_policies").get(name)
    incoming_etag = policy.get("etag", "")
    if stored != None and incoming_etag != "" and incoming_etag != stored.get("etag", ""):
        return _err(409, "There were concurrent policy changes. Please retry.", "ABORTED")

    doc = {"id": name, "version": policy.get("version", 1), "bindings": bindings, "etag": _fresh_etag()}
    _upsert(store_collection("iam_policies"), doc)
    return respond(200, {"version": doc["version"], "bindings": doc["bindings"], "etag": doc["etag"]})

def _test_iam_permissions(req):
    err = _require_bearer(req)
    if err != None:
        return err
    name = _queue_name(req["params"].get("project", ""), req["params"].get("location", ""), req["params"].get("queue", ""))
    if _find_queue(name) == None:
        return _missing("Queue", name)
    body, jerr = _json_body(req)
    if jerr != None:
        return jerr
    permissions = body.get("permissions", [])
    if permissions == None:
        permissions = []
    # The sim holds no ACLs — every permission asked about is granted.
    return respond(200, {"permissions": permissions})
