# Workers handlers for the Cloudflare API.
#
# GET  /accounts/{account_id}/workers/scripts        -> list scripts
# PUT  /accounts/{account_id}/workers/scripts/{name}  -> deploy worker
# GET  /accounts/{account_id}/workers/scripts/{name}  -> get script
# GET  /accounts/{account_id}/workers/scripts/{name}/deployments -> list deployments
#
# Stateful: deployed workers appear in the scripts list, and each PUT records
# a deployment whose rollout status is derived on read (see
# on_list_deployments).
#
# Shared helpers (_require_auth, _cf_ok, _cf_err, _gen_id) are preloaded
# from scripts/lib.star.

# Cloudflare's Workers script error code, assembled (no 5+ digit literals
# in scripts).
_WORKERS_ERR = 10 * 1000 + 43

# on_list_scripts returns the list of deployed Worker scripts.
def on_list_scripts(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    wc = store_collection("workers")

    # Filter by account_id
    result = []
    for w in wc.list():
        if w.get("account_id", "") == account_id:
            result.append(_worker_result(w))

    page, next_cursor = _list_page(req, result)
    return _cf_ok_with_info(page, len(result), next_cursor)

# on_deploy_script deploys (creates or updates) a Worker script.
# PUT /accounts/{account_id}/workers/scripts/{script_name}
#
# Real Cloudflare uploads the script as multipart/form-data: a "metadata"
# part (JSON with main_module etc.) plus one part per module whose part name
# is the module path (e.g. "worker.js"). The module bytes are stored in the
# blob store ("cf-worker-scripts") and referenced by blob name from the
# worker doc. A JSON body with main_module (the previous simulator shape)
# is still accepted.
def on_deploy_script(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    script_name = req["params"]["script_name"]

    if script_name == "":
        return _cf_err(400, _WORKERS_ERR, "Missing Worker script name.")

    script_content = ""
    fail = False
    main_module = ""
    ct = ""
    headers = req.get("headers")
    if headers != None:
        ct = headers.get("Content-Type", "")
        if ct == None:
            ct = ""

    if _has_prefix(ct, "multipart/"):
        parts, perr = parse_multipart(ct, req["raw_body"])
        if perr != None:
            return _cf_err(400, _WORKERS_ERR, "Malformed multipart body: " + perr)
        for p in parts:
            if p["filename"] != None:
                # The module part: its name is the module path.
                script_content = p["data"]
                main_module = p["name"]
            elif p["name"] == "metadata":
                if p.get("data", "").find('"') < 0:
                    return _cf_err(400, 10001, "metadata part must be a JSON object")
                meta = json.decode(p["data"])
                if meta != None:
                    mm = meta.get("main_module", None)
                    if mm != None:
                        main_module = str(mm)
                    sf = meta.get("simulate_fail", None)
                    if sf == True:
                        fail = True
        if script_content == "":
            return _cf_err(400, _WORKERS_ERR, "multipart body has no script module part")
    else:
        body = req.get("body")
        if body != None:
            mm = body.get("main_module", None)
            if mm != None:
                main_module = str(mm)
            else:
                main_module = script_name
            sf = body.get("simulate_fail", None)
            if sf == True:
                fail = True
            script_content = json.encode(body)
        else:
            script_content = req["raw_body"]
            main_module = script_name

    # Store the script content in the blob store; the worker doc references
    # the blob name (per-deployment so every upload is retained).
    seq = store_kv_incr("cf", "worker_blob_seq")
    blob_name = script_name + "-" + str(seq)
    bs = store_blob("cf-worker-scripts")
    bs.put(blob_name, script_content, "application/javascript+module")

    wc = store_collection("workers")

    # Check if script already exists -> update
    existing_id = None
    old_blob = ""
    for w in wc.list():
        if w.get("name", "") == script_name and w.get("account_id", "") == account_id:
            existing_id = w.get("id", "")
            old_blob = w.get("blob_name", "")
            break

    worker_id = ""
    if existing_id != None and existing_id != "":
        worker_id = w.get("worker_id", "")
        doc = {
            "id": existing_id,
            "worker_id": worker_id,
            "name": script_name,
            "account_id": account_id,
            "main_module": main_module,
            "blob_name": blob_name,
            "created_on": w.get("created_on", _iso8601()),
            "modified_on": _iso8601(),
        }
        wc.update(existing_id, doc)
        if old_blob != "" and old_blob != blob_name:
            bs.delete(old_blob)
    else:
        worker_id = _gen_id("worker")
        wc.insert({
            "worker_id": worker_id,
            "name": script_name,
            "account_id": account_id,
            "main_module": main_module,
            "blob_name": blob_name,
            "created_on": _iso8601(),
            "modified_on": _iso8601(),
        })

    # Record a deployment for this upload with the derive-on-read
    # timestamps (see on_list_deployments).
    now = clock.now_unix()
    dc = store_collection("deployments")
    dc.insert({
        "id": _gen_id("deploy"),
        "account_id": account_id,
        "script_name": script_name,
        "strategy": "percentage",
        "versions": [{"version_id": _gen_id("wver"), "percentage": 100}],
        "status": "active",
        "created_on": _iso8601(),
        "_running_at": now + 1,
        "_done_at": now + 3,
        "_fail": fail,
    })

    return _cf_ok({
        "script": script_name,
        "modified_on": _iso8601(),
        "created_on": _iso8601(),
        "id": worker_id,
    })

# on_get_script returns a single Worker script.
def on_get_script(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    script_name = req["params"]["script_name"]

    wc = store_collection("workers")
    worker = None
    for w in wc.list():
        if w.get("name", "") == script_name and w.get("account_id", "") == account_id:
            worker = w
            break
    if worker == None:
        return _cf_err(404, _WORKERS_ERR, "Worker script not found.")

    return _cf_ok(_worker_result(worker))

# on_delete_script deletes a Worker script by name.
# DELETE /accounts/{account_id}/workers/scripts/{script_name}
# Real Cloudflare returns 200 with a success envelope (result: null).
def on_delete_script(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    script_name = req["params"]["script_name"]

    wc = store_collection("workers")
    target = None
    for w in wc.list():
        if w.get("name", "") == script_name and w.get("account_id", "") == account_id:
            target = w
            break
    if target == None:
        return _cf_err(404, _WORKERS_ERR, "Worker script not found.")

    wc.delete(target.get("id", ""))
    return _cf_ok(None)

# ====================================================================
# Helpers
# ====================================================================

# on_list_deployments returns the deployments recorded for a Worker script.
# GET /accounts/{account_id}/workers/scripts/{script_name}/deployments
# Each PUT of the script records a deployment whose rollout status is a
# derive-on-read state machine: active (prior version still serving) ->
# in_progress (rollout underway) -> deployed (100% of traffic). The real
# Cloudflare Deployment object has no status field (versions carry rollout
# percentages); exposing `status` is a simulator extension for polling.
def on_list_deployments(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    script_name = req["params"]["script_name"]

    dc = store_collection("deployments")
    result = []
    for d in dc.list():
        if d.get("account_id", "") != account_id or d.get("script_name", "") != script_name:
            continue
        _advance_deployment(d, dc)
        result.append(_deployment_result(d))

    return _cf_ok(result)

# _derive_deployment_status maps the clock onto the rollout status
# vocabulary: active -> in_progress -> deployed, or failed when the upload
# was flagged simulate_fail.
def _derive_deployment_status(d):
    if d.get("_done_at", None) == None:
        return d.get("status", "deployed")
    now = clock.now_unix()
    if now < d.get("_running_at", 0):
        return "active"
    if now < d["_done_at"]:
        return "in_progress"
    if d.get("_fail", False):
        return "failed"
    return "deployed"

# _advance_deployment derives the current status and persists the transition
# back to the deployments collection so repeated polls agree. Cloudflare has
# no deployment webhook, so no events are emitted.
def _advance_deployment(d, dc):
    status = _derive_deployment_status(d)
    if d.get("status", "") == status:
        return status
    d["status"] = status
    dc.update(d.get("id", ""), d)
    return status

# _deployment_result returns a clean deployment object for the API response.
def _deployment_result(d):
    return {
        "id": d.get("id", ""),
        "script_name": d.get("script_name", ""),
        "status": d.get("status", "active"),
        "strategy": d.get("strategy", "percentage"),
        "versions": d.get("versions", []),
        "created_on": d.get("created_on", _iso8601()),
    }

# _worker_result returns a clean worker object for the API response.
def _worker_result(w):
    return {
        "id": w.get("worker_id", _gen_id("worker")),

        "name": w.get("name", ""),
        "created_on": w.get("created_on", _iso8601()),
        "modified_on": w.get("modified_on", _iso8601()),
        "usage_model": "bundled",
        "logpush": False,
    }
