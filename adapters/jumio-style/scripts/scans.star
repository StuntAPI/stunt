# Scan handlers — Jumio Netverify API.
#
# POST /netverify/v2/scans
#   JSON {merchantScanReference, country, ...}
#   → {timestamp, scanReference, status:"PENDING"}
# GET  /netverify/v2/scans/{scan_reference}
#   → status PENDING→DONE + {status, extractedData:{...}}
# GET  /netverify/v2/scans/{scan_reference}/data
#   → extractedData

# Shared helpers (_bearer, _require_auth, _err, _gen_scan_ref) are preloaded.

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
    })

    return respond(200, {
        "timestamp": "2024-01-15T10:00:00.000Z",
        "scanReference": scan_ref,
        "merchantScanReference": merchant_ref,
        "status": "PENDING",
    })

def on_get_scan(req):
    if not _require_auth(req):
        return respond(401, _err(401, "Unauthorized"))

    scan_ref = req["params"]["scan_reference"]
    sc = store_collection("scans")
    doc = sc.get(scan_ref)
    if doc == None:
        return respond(404, _err(404, "Scan not found"))

    # Advance: PENDING → DONE on first GET.
    if doc["status"] == "PENDING":
        doc["status"] = "DONE"
        doc["get_count"] = 1
        doc["extractedData"] = _build_extracted(doc)
        sc.update(scan_ref, doc)

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
    sc = store_collection("scans")
    doc = sc.get(scan_ref)
    if doc == None:
        return respond(404, _err(404, "Scan not found"))

    # Auto-advance to DONE if still PENDING.
    if doc["status"] == "PENDING":
        doc["status"] = "DONE"
        doc["extractedData"] = _build_extracted(doc)
        sc.update(scan_ref, doc)

    return respond(200, {
        "scanReference": doc["scanReference"],
        "status": doc["status"],
        "extractedData": doc.get("extractedData", {}),
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
