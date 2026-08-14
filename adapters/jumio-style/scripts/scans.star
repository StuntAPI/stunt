# Scan handlers — Jumio Netverify API.
#
# POST /netverify/v2/scans
#   JSON {merchantScanReference, country, ...}
#   → {timestamp, scanReference, status:"PENDING"}
# GET  /netverify/v2/scans/{scan_reference}
#   → status PENDING (0-3s) → DONE (+3s) | FAILED (+3s if simulate_fail)
# GET  /netverify/v2/scans/{scan_reference}/data
#   → extractedData (only once DONE)

# Shared helpers (_bearer, _require_auth, _err, _gen_scan_ref,
# _derive_scan_status, _signed_emit) are preloaded.

def on_create_scan(req):
    if not _require_auth(req):
        return respond(401, _err(401, "Unauthorized"))

    body = req["body"]
    if body == None:
        body = {}

    merchant_ref = body.get("merchantScanReference", "")
    country = body.get("country", "USA")
    doc_type = body.get("type", "DRIVING_LICENSE")
    front_image = body.get("frontsideImage", "")
    back_image = body.get("backsideImage", "")

    seq = store_kv_incr("jumio", "scan_seq")
    scan_ref = _gen_scan_ref(seq)

    # Async lifecycle: derive-on-read timestamps (see lib.star).
    now = clock.now_unix()

    sc = store_collection("scans")
    sc.insert({
        "id": scan_ref,
        "scanReference": scan_ref,
        "merchantScanReference": merchant_ref,
        "country": country,
        "type": doc_type,
        "status": "PENDING",
        "get_count": 0,
        "timestamp": "2024-01-15T10:00:00.000Z",
        "extractedData": None,
        "_running_at": now + 1,
        "_done_at": now + 3,
        "_fail": body.get("simulate_fail", False) == True,
    })

    return respond(200, {
        "timestamp": "2024-01-15T10:00:00.000Z",
        "scanReference": scan_ref,
        "merchantScanReference": merchant_ref,
        "status": "PENDING",
    })

# _advance_scan derives the scan's current status from the clock, persists the
# transition, and fires once-only side effects (signed webhook + extracted
# data seeding) at the terminal transitions Jumio notifies.
def _advance_scan(scan_ref):
    sc = store_collection("scans")
    doc = sc.get(scan_ref)
    if doc == None:
        return None

    new_status = _derive_scan_status(doc)
    if new_status != doc["status"]:
        doc["status"] = new_status
        if new_status == "DONE":
            doc["extractedData"] = _build_extracted(doc)
        sc.update(scan_ref, doc)
        if new_status == "DONE":
            _signed_emit("scan.completed", {
                "scanReference": scan_ref,
                "status": "DONE",
            })
        elif new_status == "FAILED":
            _signed_emit("scan.failed", {
                "scanReference": scan_ref,
                "status": "FAILED",
            })
    return doc

def on_get_scan(req):
    if not _require_auth(req):
        return respond(401, _err(401, "Unauthorized"))

    scan_ref = req["params"]["scan_reference"]
    doc = _advance_scan(scan_ref)
    if doc == None:
        return respond(404, _err(404, "Scan not found"))

    doc["get_count"] = doc.get("get_count", 0) + 1
    store_collection("scans").update(scan_ref, doc)

    return respond(200, {
        "timestamp": doc.get("timestamp", ""),
        "scanReference": doc["scanReference"],
        "merchantScanReference": doc.get("merchantScanReference", ""),
        "status": doc["status"],
    })

def on_get_scan_data(req):
    if not _require_auth(req):
        return respond(401, _err(401, "Unauthorized"))

    scan_ref = req["params"]["scan_reference"]
    doc = _advance_scan(scan_ref)
    if doc == None:
        return respond(404, _err(404, "Scan not found"))

    if doc["status"] == "FAILED":
        return respond(409, _err(409, "Scan failed; no extracted data available"))

    return respond(200, {
        "scanReference": doc["scanReference"],
        "status": doc["status"],
        "extractedData": doc.get("extractedData", None),
    })

# _build_extracted creates synthetic extracted document data.
def _build_extracted(doc):
    return {
        "firstName": "JOHN",
        "lastName": "DOE",
        "dob": "1990-01-15",
        "expiry": "2030-06-20",
        "documentNumber": "D1234567" + str(doc.get("get_count", 0)),
        "country": doc.get("country", "USA"),
        "usState": "CA",
        "address": "123 MAIN ST, ANYTOWN, CA 90210",
    }
