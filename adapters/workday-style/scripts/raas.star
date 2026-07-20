# RaaS (Reports as a Service) handlers — Workday Custom Reports.
#
# Workday's RaaS endpoints expose Custom Reports as REST resources under
# /ccx/v1/{tenant}/RaaS/{report_name}. They return JSON rows (or CSV).
#
# GET  /ccx/v1/{tenant}/RaaS/Custom_Report
#   -> {"Report_Entry": [{...}, {...}]}  (Workday report JSON shape)
#
# POST /ccx/v1/{tenant}/staffing/Create_Worker
#   -> Create worker (RaaS-style custom endpoint)

# Shared helpers from lib.star.

# Synthetic report rows for the default Custom_Report.
_REPORT_ROWS = [
    {
        "WorkerID": "1",
        "WorkerName": "John Smith",
        "Position": "Senior Software Engineer",
        "Location": "San Francisco HQ",
        "Status": "Active",
    },
    {
        "WorkerID": "2",
        "WorkerName": "Mary Johnson",
        "Position": "Product Manager",
        "Location": "San Francisco HQ",
        "Status": "Active",
    },
    {
        "WorkerID": "3",
        "WorkerName": "Bob Wilson",
        "Position": "Financial Analyst",
        "Location": "New York Office",
        "Status": "On Leave",
    },
]

def on_custom_report(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    # Workday RaaS wraps results in a "Report_Entry" key.
    return respond(200, {
        "Report_Entry": _REPORT_ROWS,
    })

def on_create_worker(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)

    # Generate a worker ID.
    worker_id = _next_id("worker")

    worker = {
        "id": worker_id,
        "descriptor": body.get("Worker_Name", body.get("name", "New Worker")),
        "workerID": {"id": worker_id},
        "primaryWorkEmail": body.get("Primary_Work_Email", body.get("email", "")),
        "primaryEmploymentReference": body.get("Primary_Employment_Reference", body.get("employment", {})),
    }

    # Store the worker so it appears in subsequent list calls.
    col = store_collection("workers")
    col.insert(worker)

    # Workday create endpoints return the created resource.
    return respond(200, worker)
