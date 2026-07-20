# Workers handlers for the Cloudflare API.
#
# GET  /accounts/{account_id}/workers/scripts        -> list scripts
# PUT  /accounts/{account_id}/workers/scripts/{name}  -> deploy worker
# GET  /accounts/{account_id}/workers/scripts/{name}  -> get script
#
# Stateful: deployed workers appear in the scripts list.
#
# Shared helpers (_require_auth, _cf_ok, _cf_err, _gen_id) are preloaded
# from scripts/lib.star.

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

    return _cf_ok_with_info(result, len(result))

# on_deploy_script deploys (creates or updates) a Worker script.
# PUT /accounts/{account_id}/workers/scripts/{script_name}
# Body is multipart/form-data (real CF) but we accept JSON or raw too.
def on_deploy_script(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    script_name = req["params"]["script_name"]

    if script_name == "":
        return _cf_err(400, 10043, "Missing Worker script name.")

    # Extract script content — from body if available
    body = req.get("body")
    script_content = ""
    if body != None:
        main_module = body.get("main_module", None)
        if main_module != None:
            script_content = str(main_module)
        else:
            # Store the body as string (could be multipart form parsed)
            script_content = str(body)

    wc = store_collection("workers")

    # Check if script already exists -> update
    existing_id = None
    for w in wc.list():
        if w.get("name", "") == script_name and w.get("account_id", "") == account_id:
            existing_id = w.get("id", "")
            break

    doc = {
        "name": script_name,
        "account_id": account_id,
        "script": script_content,
        "created_on": _iso8601(),
        "modified_on": _iso8601(),
    }
    if existing_id != None and existing_id != "":
        wc.update(existing_id, doc)
    else:
        wc.insert(doc)

    return _cf_ok({
        "script": script_name,
        "modified_on": _iso8601(),
        "created_on": _iso8601(),
        "id": _gen_id("worker"),
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
        return _cf_err(404, 10043, "Worker script not found.")

    return _cf_ok(_worker_result(worker))

# ====================================================================
# Helpers
# ====================================================================

# _worker_result returns a clean worker object for the API response.
def _worker_result(w):
    return {
        "id": _gen_id("worker"),
        "name": w.get("name", ""),
        "created_on": w.get("created_on", _iso8601()),
        "modified_on": w.get("modified_on", _iso8601()),
        "usage_model": "bundled",
        "logpush": False,
    }
