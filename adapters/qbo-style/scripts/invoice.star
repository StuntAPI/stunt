# Invoice handlers — create, read, void.
#
# POST /v3/company/{realmId}/invoice       { Line, CustomerRef, ... } -> { Invoice: {...} }
# POST /v3/company/{realmId}/invoice?operation=void  { Id, SyncToken } -> { Invoice: {...} } (VOID)
# GET  /v3/company/{realmId}/invoice/{id}  -> { Invoice: {...} }

# Shared helpers (_bearer, _require_token, _realm_matches, _fault, _now,
# _next_id) from lib.star.

def on_create_invoice(req):
    token_doc, err = _require_token(req)
    if err != None:
        return err

    realm_id = req["params"]["realmId"]
    if not _realm_matches(token_doc, realm_id):
        return _auth_fault()

    body = req["body"]
    if body == None:
        body = {}

    # ?operation=void — QBO's documented void: POST with {Id, SyncToken}.
    q = req.get("query")
    if q != None and q.get("operation", "") == "void":
        return _void_invoice(body)

    line = body.get("Line", [])
    if len(line) == 0:
        return _fault(400, "610", "Required parameter missing", "At least one Line is required")

    # Calculate total from line items.
    total = 0.0
    for li in line:
        amt = li.get("Amount", 0)
        total += amt

    cust_ref = body.get("CustomerRef", {})
    cust_value = ""
    cust_name = ""
    if cust_ref != None:
        cust_value = cust_ref.get("value", "")
        cust_name = cust_ref.get("name", "")

    inv_id = _next_id("inv")
    doc_num = "INV-" + str(store_kv_incr("qbo", "docnum_seq") + 1000)

    doc = {
        "id": inv_id,
        "Id": inv_id,
        "DocNumber": doc_num,
        "TxnDate": body.get("TxnDate", "2024-01-01"),
        "CustomerRef": {"value": cust_value, "name": cust_name},
        "TotalAmt": total,
        "Balance": total,
        "CurrencyRef": {"value": "USD", "name": "US Dollar"},
        "Line": line,
        "DueDate": body.get("DueDate", "2024-02-01"),
        "domain": "QBO",
        "sparse": False,
        "SyncToken": "0",
    }

    c = store_collection("invoices")
    c.insert(doc)

    return respond(200, {"Invoice": doc, "time": _now()})

# _void_invoice implements ?operation=void: the invoice is soft-deleted into
# QBO's terminal VOIDED state — the record is kept, status flips to "Voided"
# and the balance is zeroed — exactly the observable end state of the real
# API's void. An unknown Id is the usual 620 Object Not Found fault; a missing
# Id is a 610 parameter fault.
def _void_invoice(body):
    inv_id = body.get("Id", "")
    if inv_id == "":
        return _fault(400, "610", "Required parameter missing", "Id is required to void an invoice")

    c = store_collection("invoices")
    doc = c.get(inv_id)
    if doc == None:
        return _fault(404, "620", "Object Not Found", "Invoice " + inv_id + " not found")

    doc["status"] = "Voided"
    doc["Balance"] = 0
    doc["SyncToken"] = _bump_sync(doc.get("SyncToken", "0"))
    c.update(inv_id, doc)
    return respond(200, {"Invoice": doc, "time": _now()})

def on_delete_invoice(req):
    token_doc, err = _require_token(req)
    if err != None:
        return err

    realm_id = req["params"]["realmId"]
    if not _realm_matches(token_doc, realm_id):
        return _auth_fault()

    inv_id = req["params"]["id"]
    c = store_collection("invoices")
    doc = c.get(inv_id)
    if doc == None:
        return _fault(404, "620", "Object Not Found", "Invoice " + inv_id + " not found")

    c.delete(inv_id)
    return respond(200, {"Invoice": {"Id": inv_id, "domain": "QBO", "status": "Deleted"}, "time": _now()})

def on_read_invoice(req):
    token_doc, err = _require_token(req)
    if err != None:
        return err

    realm_id = req["params"]["realmId"]
    if not _realm_matches(token_doc, realm_id):
        return _auth_fault()

    inv_id = req["params"]["id"]
    c = store_collection("invoices")
    doc = c.get(inv_id)
    if doc == None:
        return _fault(404, "620", "Object Not Found", "Invoice " + inv_id + " not found")

    return respond(200, {"Invoice": doc, "time": _now()})
