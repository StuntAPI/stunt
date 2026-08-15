# Bulk Lead Extract handlers — Marketo bulk export jobs (derive-on-read).
#
# POST /bulk/v1/leads/export/create(.json)      -> enqueue export job (Queued)
# GET  /bulk/v1/leads/export/{exportId}/status(.json)  -> poll job status
# GET  /bulk/v1/leads/export/{exportId}/file(.json)    -> download the CSV
# POST /bulk/v1/leads/export/{exportId}/cancel(.json)  -> cancel the job
#
# ASYNC LIFECYCLE (derive-on-read): a job is created "Queued" and every status
# poll derives the real Marketo state from the clock — Processing after 1s,
# Completed after 3s. The transition is persisted so polls agree, and at the
# moment a job first reaches Completed the CSV result is ENQUEUED (rendered
# and stored once) and a signed webhook fires exactly once. Cancelling a job
# that has not completed pins it to "Cancelled"; cancelling a Completed job
# fails. Fetching the file before completion fails with code 1030.
#
# Real Marketo status vocabulary: Queued / Processing / Completed / Cancelled.

# Shared helpers from lib.star.

# _EXPORT_RUN_AFTER / _EXPORT_DONE_AFTER: seconds after creation at which a
# job becomes Processing / Completed (clock-derived at CREATE time, so tests
# can sleep through the window deterministically).
_EXPORT_RUN_AFTER = 1
_EXPORT_DONE_AFTER = 3

# on_export_create enqueues a bulk lead extract job.
# Body: {fields: ["id","email",...], filter: {...}} (both optional).
def on_export_create(req):
    ok, err = _require_auth(req)
    if not ok:
        return err
    if _check_quota():
        return _quota_err()

    body = _get_body(req)
    cols = []
    fields = body.get("fields", None)
    if fields != None:
        for f in fields:
            cols.append(str(f))
    if len(cols) == 0:
        cols = ["id", "email", "firstName", "lastName", "createdAt", "updatedAt"]

    seq = store_kv_incr("marketo", "export_seq")
    export_id = str(seq)
    now = clock.now_unix()

    job = {
        "id": export_id,
        "fields": cols,
        "status": "Queued",
        "format": "CSV",
        "createdAt": now,
        "fileSize": 0,
        "fileChecksum": "",
        # Retention window (7 days), computed — never a literal epoch.
        "expiresAt": now + 3600 * 24 * 7,
        "_processing_at": now + _EXPORT_RUN_AFTER,
        "_completed_at": now + _EXPORT_DONE_AFTER,
        "_webhook_fired": False,
        "_cancelled": False,
    }
    store_collection("exports").insert(job)

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": [{"exportId": export_id, "status": "Queued"}],
        "moreResult": False,
    })

# on_export_status polls a job. Derives the clock-based status, persists the
# transition, and — on first reaching Completed — enqueues the CSV file
# result and fires the signed completion webhook once.
def on_export_status(req):
    ok, err = _require_auth(req)
    if not ok:
        return err
    if _check_quota():
        return _quota_err()

    export_id = req["params"].get("exportId", "")
    col = store_collection("exports")
    job = col.get(export_id)
    if job == None:
        return _export_not_found(export_id)

    job = _derive_export(job)
    col.update(export_id, job)

    item = {"exportId": export_id, "status": job["status"]}
    if job["status"] == "Completed":
        item["fileSize"] = job["fileSize"]
        item["fileChecksum"] = job["fileChecksum"]
        item["expiresAt"] = job["expiresAt"]
    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": [item],
        "moreResult": False,
    })

# on_export_file streams the CSV result. Only available once the job has
# reached Completed (otherwise Marketo error 1030).
def on_export_file(req):
    ok, err = _require_auth(req)
    if not ok:
        return err
    if _check_quota():
        return _quota_err()

    export_id = req["params"].get("exportId", "")
    col = store_collection("exports")
    job = col.get(export_id)
    if job == None:
        return _export_not_found(export_id)

    job = _derive_export(job)
    col.update(export_id, job)

    if job["status"] != "Completed":
        return _marketo_err_status(400, 1030, "Job status is not Complete yet")

    csv = store_kv_get("mkto_export_csv", export_id)
    if csv == None:
        # Defensive: derive+enqueue if the file was never rendered.
        csv = _export_csv(job["fields"])
        store_kv_set("mkto_export_csv", export_id, csv)

    return respond(200, csv, {"Content-Type": "text/csv"})

# on_export_cancel cancels a queued/processing job. Cancelling an already
# Completed job fails with 1030.
def on_export_cancel(req):
    ok, err = _require_auth(req)
    if not ok:
        return err
    if _check_quota():
        return _quota_err()

    export_id = req["params"].get("exportId", "")
    col = store_collection("exports")
    job = col.get(export_id)
    if job == None:
        return _export_not_found(export_id)

    job = _derive_export(job)
    if job["status"] == "Completed":
        return _marketo_err_status(400, 1030, "Job already completed")

    job["_cancelled"] = True
    job["status"] = "Cancelled"
    col.update(export_id, job)

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": [{"exportId": export_id, "status": "Cancelled"}],
        "moreResult": False,
    })

# --- helpers ---

# _derive_export returns the job with its status derived from the clock
# (Cancelled pinned; otherwise Queued -> Processing -> Completed). On the
# first transition to Completed it enqueues the CSV result (rendered once,
# stored in KV) and fires the signed webhook exactly once.
def _derive_export(job):
    if job.get("_cancelled", False):
        job["status"] = "Cancelled"
        return job
    now = clock.now_unix()
    if now >= job.get("_completed_at", 0):
        if job["status"] != "Completed":
            job["status"] = "Completed"
            _finalize_export(job)
    elif now >= job.get("_processing_at", 0):
        job["status"] = "Processing"
    else:
        job["status"] = "Queued"
    return job

# _finalize_export runs exactly once per job, at the completion transition:
# render + store the CSV, record its size/checksum, fire the webhook.
def _finalize_export(job):
    csv = _export_csv(job["fields"])
    store_kv_set("mkto_export_csv", job["id"], csv)
    job["fileSize"] = len(csv)
    job["fileChecksum"] = "sha256:" + crypto.sha256(csv)
    if not job.get("_webhook_fired", False):
        job["_webhook_fired"] = True
        _signed_emit("export.completed", {
            "exportId": job["id"],
            "status": "Completed",
            "fileSize": job["fileSize"],
        })

# _export_csv renders the export file for a column list from the leads store.
def _export_csv(cols):
    lines = [",".join(cols)]
    for d in store_collection("leads").list():
        vals = []
        for c in cols:
            v = d.get(c, "")
            if v == None:
                v = ""
            vals.append(str(v))
        lines.append(",".join(vals))
    return "\n".join(lines)

# _export_not_found returns the 404 Marketo envelope for an unknown job.
def _export_not_found(export_id):
    return respond(404, {
        "requestId": _request_id(),
        "success": False,
        "errors": [{"code": "604", "message": "Export job not found: " + export_id}],
        "moreResult": False,
    })
