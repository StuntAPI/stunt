# Transactions handlers — create, list, get, void.
#
# Requires auth.
# STATEFUL transactions stored in the "transactions" collection.
#
# POST /v2/transactions/create → { id, code, type, date, totalAmount, totalTax, lines, summary, addresses }
# GET  /v2/transactions        → { value: [...] }
# GET  /v2/transactions/{id}   → { ... }
# POST /v2/transactions/{id}/void → { id, status:"Cancelled" }

# on_create_transaction creates a new AvaTax transaction.
def on_create_transaction(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    company_code = body.get("companyCode", "DEFAULT")
    type_ = body.get("type", "SalesInvoice")
    date = body.get("date", "2024-06-15")
    customer_code = body.get("customerCode", "CUST001")

    addresses = body.get("addresses", {})
    if addresses == None:
        addresses = {}

    lines = body.get("lines", [])
    if lines == None:
        lines = []

    state = _address_state(addresses)
    tax_result = _compute_tax(lines, state)

    # Compute total amount = totalTaxable + totalTax.
    total_taxable = tax_result["totalTaxable"]
    total_tax = tax_result["totalTax"]
    total_amount = _fmt(_to_float(total_taxable) + _to_float(total_tax))

    txn_id = _txn_id()
    code = _txn_code()

    # Build the full addresses response with singleLocation.
    single_loc = addresses.get("singleLocation")
    if single_loc != None:
        addr_resp = {"singleLocation": single_loc}
    else:
        addr_resp = addresses

    doc = {
        "id": txn_id,
        "code": code,
        "companyId": _company_id(),
        "companyCode": company_code,
        "type": type_,
        "date": date,
        "customerCode": customer_code,
        "totalAmount": total_amount,
        "totalTax": total_tax,
        "totalTaxable": total_taxable,
        "lines": tax_result["lines"],
        "summary": tax_result["summary"],
        "addresses": addr_resp,
        "status": "Saved",
    }

    c = store_collection("transactions")
    c.insert(doc)

    return respond(200, {
        "id": doc["id"],
        "code": doc["code"],
        "companyId": doc["companyId"],
        "companyCode": doc["companyCode"],
        "type": doc["type"],
        "date": doc["date"],
        "customerCode": doc["customerCode"],
        "totalAmount": doc["totalAmount"],
        "totalTax": doc["totalTax"],
        "totalTaxable": doc["totalTaxable"],
        "lines": doc["lines"],
        "summary": doc["summary"],
        "addresses": doc["addresses"],
        "status": doc["status"],
    })

# on_list_transactions lists all transactions.
def on_list_transactions(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("transactions")
    docs = c.list()

    value = []
    for doc in docs:
        value.append({
            "id": doc.get("id", ""),
            "code": doc.get("code", ""),
            "type": doc.get("type", "SalesInvoice"),
            "totalAmount": doc.get("totalAmount", "0"),
            "totalTax": doc.get("totalTax", "0"),
            "status": doc.get("status", "Saved"),
        })

    return respond(200, {
        "@recordsetCount": len(value),
        "value": value,
    })

# on_get_transaction returns a single transaction by ID.
def on_get_transaction(req):
    err = _require_auth(req)
    if err != None:
        return err

    txn_id = req["params"].get("id", "")
    if txn_id == None or txn_id == "":
        return _avalara_err(400, "ValidationError", "Transaction ID is required")

    c = store_collection("transactions")
    docs = c.list()

    for doc in docs:
        if doc.get("id", "") == txn_id:
            return respond(200, doc)

    return _avalara_err(404, "NotFound", "Transaction not found: " + txn_id)

# on_void_transaction voids a transaction by ID.
def on_void_transaction(req):
    err = _require_auth(req)
    if err != None:
        return err

    txn_id = req["params"].get("id", "")
    c = store_collection("transactions")
    docs = c.list()

    for doc in docs:
        if doc.get("id", "") == txn_id:
            doc["status"] = "Cancelled"
            c.update(doc.get("id", ""), doc)
            return respond(200, {
                "id": txn_id,
                "status": "Cancelled",
            })

    return _avalara_err(404, "NotFound", "Transaction not found: " + txn_id)
