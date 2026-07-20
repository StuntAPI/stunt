# Workers handlers — Workday Staffing REST API.
#
# GET /wbs/v40.0/staffing/workers      -> {data:[...], total, more}
# GET /wbs/v40.0/staffing/workers/{id} -> single worker object

# Shared helpers from lib.star.

def on_list_workers(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("workers")
    docs = col.list()
    return _paginate(req, docs)

def on_get_worker(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    worker_id = req["params"].get("id", "")
    if worker_id == "":
        return _workday_error(400, "INVALID_REQUEST", "A worker ID is required.")

    col = store_collection("workers")
    doc = col.get(worker_id)
    if doc == None:
        return _workday_error(404, "RESOURCE_NOT_FOUND",
            "Worker '" + worker_id + "' not found.")

    return respond(200, doc)
