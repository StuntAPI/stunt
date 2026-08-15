# Pipelines handlers — Azure DevOps Pipelines (v7.1).
#
# GET  /{org}/{project}/_apis/pipelines                        → {value:[...], count}
# GET  /{org}/{project}/_apis/pipelines/{pipelineId}           → pipeline
# GET  /{org}/{project}/_apis/pipelines/{pipelineId}/runs      → {value:[...], count}
# POST /{org}/{project}/_apis/pipelines/{pipelineId}/runs      → queues a run (200)
# GET  /{org}/{project}/_apis/pipelines/{pipelineId}/runs/{runId} → run
#
# ASYNC LIFECYCLE (derive-on-read): a queued run reports state "queued" at
# POST time; every read derives the real state from the clock — "inProgress"
# after 1s, "completed" with result "succeeded" (or "failed" with failure
# injection) after 3s — persisting each transition and firing the
# ms.vss-pipelines.run-state-changed service hook once per NEW state, so
# lists agree with single-run polls.

_RUN_INPROGRESS_AFTER = 1  # seconds
_RUN_COMPLETED_AFTER = 3   # seconds

def on_list_pipelines(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    plc = store_collection("pipelines")
    items = []
    for p in plc.list():
        items.append(_pipeline_resource(p))

    page, continuation = _list_page(req, items)
    resp = {"value": page, "count": len(page)}
    if continuation != None:
        resp["continuationToken"] = continuation
    return respond(200, resp)

def on_get_pipeline(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    plc = store_collection("pipelines")
    p = plc.get(str(_to_int(req["params"]["pipelineId"])))
    if p == None:
        return _pipeline_fault(404, "Pipeline not found", "PipelineNotFoundException")
    return respond(200, _pipeline_resource(p))

def on_list_runs(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    pid = _to_int(req["params"]["pipelineId"])
    plc = store_collection("pipelines")
    if plc.get(str(pid)) == None:
        return _pipeline_fault(404, "Pipeline not found", "PipelineNotFoundException")

    rc = store_collection("runs")
    items = []
    for r in rc.list():
        if r.get("pipeline_id", 0) != pid:
            continue
        items.append(_run_resource(_advance_run(r)))

    page, continuation = _list_page(req, items)
    resp = {"value": page, "count": len(page)}
    if continuation != None:
        resp["continuationToken"] = continuation
    return respond(200, resp)

# on_queue_run queues a new run for the pipeline. Body (all optional):
#   {"resources": {"repositories": {"self": {"refName": "refs/heads/..."}}},
#    "templateParameters": {...}}
# Failure injection: templateParameters.simulate_fail = "true" (or a
# top-level simulate_fail truthy body flag) drives result "failed".
def on_queue_run(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    pid = _to_int(req["params"]["pipelineId"])
    plc = store_collection("pipelines")
    pipe = plc.get(str(pid))
    if pipe == None:
        return _pipeline_fault(404, "Pipeline not found", "PipelineNotFoundException")

    body = req["body"]
    if body == None:
        body = {}

    resources = body.get("resources", None)
    if resources == None:
        resources = {"repositories": {"self": {"refName": "refs/heads/main", "repository": "self"}}}

    fail_mode = ""
    tp = body.get("templateParameters", None)
    if tp != None and str(tp.get("simulate_fail", "")) == "true":
        fail_mode = "failed"
    if body.get("simulate_fail", False):
        fail_mode = "failed"

    run_id = store_kv_incr("azure-devops", "run_seq") + 1
    run_no = store_kv_incr("azure-devops", "runno_" + str(pid)) + 1
    # Run name is the real date-sequence form (yyyymmdd.N), built at runtime.
    today = _now_iso()[:10].replace("-", "")
    name = today + "." + str(run_no)

    now_iso = _now_iso()
    run = {
        "id": str(run_id),
        "run_id": run_id,
        "pipeline_id": pid,
        "name": name,
        "state": "queued",
        "result": None,
        "createdDate": now_iso,
        "finishedDate": None,
        "resources": resources,
        "_t0": clock.now_unix(),
        "_stage": 0,
        "_fail_mode": fail_mode,
    }

    rc = store_collection("runs")
    rc.insert(run)

    return respond(200, _run_resource(run))

def on_get_run(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    rc = store_collection("runs")
    run = rc.get(req["params"]["runId"])
    if run == None:
        return _pipeline_fault(404, "Run not found", "RunNotFoundException")
    return respond(200, _run_resource(_advance_run(run)))

# --- lifecycle ---

# _advance_run derives the run's state from the clock, persists each
# transition, and fires the ms.vss-pipelines.run-state-changed service hook
# exactly once per NEW state. Returns the updated doc.
def _advance_run(run):
    stage = run.get("_stage", 0)
    t0 = run.get("_t0", 0)
    now = clock.now_unix()
    target = 0
    if now - t0 >= _RUN_COMPLETED_AFTER:
        target = 2
    elif now - t0 >= _RUN_INPROGRESS_AFTER:
        target = 1
    if target <= stage:
        return run

    rc = store_collection("runs")
    while stage < target:
        stage = stage + 1
        if stage == 1:
            run["state"] = "inProgress"
        else:
            run["state"] = "completed"
            if run.get("_fail_mode", "") != "":
                run["result"] = "failed"
            else:
                run["result"] = "succeeded"
            run["finishedDate"] = _now_iso()
        run["_stage"] = stage
        rc.update(run["id"], run)
        _emit_service_hook(
            "ms.vss-pipelines.run-state-changed",
            "Run " + run.get("name", "") + " is " + run["state"],
            _run_resource(run),
        )
    return run

# --- resource shapes ---

def _pipeline_resource(p):
    pid = _as_int(p.get("pipeline_id", p.get("id", 0)))
    return {
        "id": pid,
        "name": p.get("name", ""),
        "folder": p.get("folder", "\\"),
        "url": "https://dev.azure.com/mock-org/MyFirstProject/_apis/pipelines/" + str(pid),
        "configuration": p.get("configuration", {}),
        "_links": {
            "self": {"href": "https://dev.azure.com/mock-org/MyFirstProject/_apis/pipelines/" + str(pid)},
            "web": {"href": "https://dev.azure.com/mock-org/MyFirstProject/_build?definitionId=" + str(pid)},
        },
    }

def _run_resource(run):
    pid = _as_int(run.get("pipeline_id", 0))
    rid = _as_int(run.get("run_id", 0))
    url = "https://dev.azure.com/mock-org/MyFirstProject/_apis/pipelines/" + str(pid) + "/runs/" + str(rid)
    pipe = store_collection("pipelines").get(str(pid))
    pipe_name = pipe.get("name", "") if pipe != None else ""
    return {
        "id": rid,
        "name": run.get("name", ""),
        "pipeline": {
            "id": pid,
            "name": pipe_name,
            "folder": "\\",
            "url": "https://dev.azure.com/mock-org/MyFirstProject/_apis/pipelines/" + str(pid),
        },
        "state": run.get("state", "queued"),
        "result": run.get("result", None),
        "createdDate": run.get("createdDate", ""),
        "finishedDate": run.get("finishedDate", None),
        "url": url,
        "resources": run.get("resources", {}),
        "_links": {
            "self": {"href": url},
            "web": {"href": "https://dev.azure.com/mock-org/MyFirstProject/_build/results/" + str(rid)},
        },
    }

def _pipeline_fault(status, message, type_key):
    return respond(status, {
        "$id": "1",
        "innerException": None,
        "message": message,
        "typeName": "Microsoft.VisualStudio.Services.Pipelines.WebApi." + type_key,
        "typeKey": type_key,
        "errorCode": 0,
        "eventId": 4128,
    })
