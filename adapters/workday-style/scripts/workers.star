# Workers handlers — Workday Staffing REST API.
#
# GET /wbs/v40.0/staffing/workers      -> {data:[...], total, more}
# GET /wbs/v40.0/staffing/workers/{id} -> single worker object
#
# The list endpoint honors the documented `search` query param (a
# case-insensitive partial match over descriptor, primaryWorkEmail and
# workerID), applied before limit/offset paging.

# Shared helpers from lib.star.

def on_list_workers(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("workers")
    docs = col.list()
    docs = _apply_worker_search(req, docs)
    return _paginate(req, docs)

# _apply_worker_search applies the `search` query param to the worker list.
# Workday matches on the worker's name/descriptor or email; we also match
# the worker ID since the descriptor embeds it.
def _apply_worker_search(req, docs):
    search = _trim(_get_query(req, "search"))
    if search == "":
        return docs
    low = _lower(search)
    out = []
    for d in docs:
        descriptor = _lower(str(d.get("descriptor", "")))
        email = _lower(str(d.get("primaryWorkEmail", "")))
        wid = ""
        worker_ref = d.get("workerID")
        if worker_ref != None:
            wid = _lower(str(worker_ref.get("id", "")))
        if _contains(descriptor, low) or _contains(email, low) or _contains(wid, low):
            out.append(d)
    return out

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
