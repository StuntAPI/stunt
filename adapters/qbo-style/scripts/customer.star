# Customer handlers — create and read.
#
# POST /v3/company/{realmId}/customer          { DisplayName, ... } -> { Customer: {...} }
# GET  /v3/company/{realmId}/customer?id=X     -> { Customer: {...} }
# GET  /v3/company/{realmId}/customer/{id}     -> { Customer: {...} }

# Shared helpers (_bearer, _require_token, _realm_matches, _fault, _now,
# _next_id) from lib.star.

def on_create_customer(req):
    token_doc, err = _require_token(req)
    if err != None:
        return err

    realm_id = req["params"]["realmId"]
    if not _realm_matches(token_doc, realm_id):
        return _auth_fault()

    body = req["body"]
    if body == None:
        body = {}

    display_name = body.get("DisplayName", "")
    if display_name == "":
        return _fault(400, "610", "Required parameter missing", "DisplayName is required")

    cust_id = _next_id("cust")
    sync_token = "0"

    doc = {
        "id": cust_id,
        "Id": cust_id,
        "DisplayName": display_name,
        "GivenName": body.get("GivenName", ""),
        "FamilyName": body.get("FamilyName", ""),
        "PrimaryEmailAddr": body.get("PrimaryEmailAddr", {"Address": ""}),
        "PrimaryPhone": body.get("PrimaryPhone", {"FreeFormNumber": ""}),
        "BillAddr": body.get("BillAddr", {}),
        "Balance": 0,
        "Active": True,
        "SyncToken": sync_token,
        "domain": "QBO",
        "sparse": False,
    }

    c = store_collection("customers")
    c.insert(doc)

    return respond(200, {"Customer": doc, "time": _now()})

def on_read_customer(req):
    token_doc, err = _require_token(req)
    if err != None:
        return err

    realm_id = req["params"]["realmId"]
    if not _realm_matches(token_doc, realm_id):
        return _auth_fault()

    # GET /customer?id=X
    q = req.get("query")
    cust_id = ""
    if q != None:
        cust_id = q.get("id", "")

    c = store_collection("customers")
    if cust_id != "":
        doc = c.get(cust_id)
        if doc == None:
            return _fault(404, "620", "Object Not Found", "Customer " + cust_id + " not found")
        return respond(200, {"Customer": doc, "time": _now()})

    # No id → return all (like a query).
    docs = c.list()
    return respond(200, {"QueryResponse": {"Customer": docs, "maxResults": len(docs)}, "time": _now()})

def on_read_customer_by_id(req):
    token_doc, err = _require_token(req)
    if err != None:
        return err

    realm_id = req["params"]["realmId"]
    if not _realm_matches(token_doc, realm_id):
        return _auth_fault()

    cust_id = req["params"]["id"]
    c = store_collection("customers")
    doc = c.get(cust_id)
    if doc == None:
        return _fault(404, "620", "Object Not Found", "Customer " + cust_id + " not found")

    return respond(200, {"Customer": doc, "time": _now()})
