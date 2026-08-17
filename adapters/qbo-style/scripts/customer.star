# Customer handlers — create, update, read, deactivate.
#
# POST /v3/company/{realmId}/customer          { DisplayName, ... } -> { Customer: {...} }
#                                               POST with Id performs an UPDATE (QBO upsert).
# GET  /v3/company/{realmId}/customer?id=X     -> { Customer: {...} }
# GET  /v3/company/{realmId}/customer/{id}     -> { Customer: {...} }
# DELETE /v3/company/{realmId}/customer/{id}   -> deactivate (Active=false, soft delete)

# Shared helpers (_bearer, _require_token, _realm_matches, _fault, _now,
# _next_id) from lib.star.

# on_create_customer creates (no Id) or updates (Id present) a customer, like
# QBO's POST upsert semantics. A full or sparse ({"sparse": true}) body is
# merged over the stored object; SyncToken is bumped on every update.
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

    c = store_collection("customers")

    # UPDATE path: POST with an Id addresses an existing customer.
    upd_id = body.get("Id") or ""
    if upd_id != "":
        doc = c.get(upd_id)
        if doc == None:
            return _fault(404, "620", "Object Not Found", "Customer " + upd_id + " not found")
        for k in body:
            if k != "SyncToken" and k != "sparse":
                doc[k] = body[k]
        doc["sparse"] = False
        doc["SyncToken"] = _bump_sync(doc.get("SyncToken", "0"))
        c.update(upd_id, doc)
        return respond(200, {"Customer": doc, "time": _now()})

    display_name = body.get("DisplayName") or ""
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

    c.insert(doc)

    return respond(200, {"Customer": doc, "time": _now()})

# on_read_customer handles GET /customer?id=X (single read) and the bare
# GET /customer (list). The bare list returns only ACTIVE customers, like
# QBO's read-all default; deactivated customers stay readable by id (both
# here and via the query endpoint's WHERE Active = False).
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

    # No id → return all ACTIVE customers (like a query).
    docs = query_select(c.list(), [["Active", "=", True]])
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

# on_delete_customer_by_id DEACTIVATES the customer (Active=false) instead of
# destroying the object — QBO has no hard delete for customers; integrations
# deactivate via an update with Active=false, and the record remains readable
# by id (and via the query endpoint with WHERE Active = False) while being
# excluded from default reads. Invoices keep their CustomerRef (no orphans).
def on_delete_customer_by_id(req):
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

    doc["Active"] = False
    doc["SyncToken"] = _bump_sync(doc.get("SyncToken", "0"))
    c.update(cust_id, doc)
    return respond(200, {"Customer": {"Id": cust_id, "domain": "QBO", "Active": False, "status": "Deleted"}, "time": _now()})
