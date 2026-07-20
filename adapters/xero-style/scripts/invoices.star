# Invoices handlers — list, create, get, payment.
#
# Requires Bearer + xero-tenant-id.
# STATEFUL invoices stored in the "invoices" collection.
#
# GET  /api.xro/2.0/Invoices           → { Id, Status, Invoices: [...] }
# PUT  /api.xro/2.0/Invoices           → { Id, Status, Invoices: [...] }
# GET  /api.xro/2.0/Invoices/{id}      → { Id, Status, Invoices: [...] }
# POST /api.xro/2.0/Invoices/{id}/Payments → { Id, Status, Payments: [...] }

# on_list_invoices lists all invoices.
def on_list_invoices(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_tenant(req)
    if err != None:
        return err

    c = store_collection("invoices")
    docs = c.list()

    invoices = []
    for doc in docs:
        invoices.append(_invoice_public(doc))

    return _envelope("Invoices", invoices)

# on_put_invoices creates invoices.
def on_put_invoices(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_tenant(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    invoices_in = body.get("Invoices")
    if invoices_in == None:
        invoices_in = [body]

    result = []
    c = store_collection("invoices")
    for inv_in in invoices_in:
        invoice_id = _invoice_id()
        line_items = inv_in.get("LineItems", [])
        if line_items == None:
            line_items = []

        # Compute total from line items.
        total = "0.00"
        if len(line_items) > 0:
            li0 = line_items[0]
            total = li0.get("LineAmount", "100.00")

        doc = {
            "InvoiceID": invoice_id,
            "InvoiceNumber": inv_in.get("InvoiceNumber", "INV-" + invoice_id[0:6].upper()),
            "Type": inv_in.get("Type", "ACCREC"),
            "Status": inv_in.get("Status", "DRAFT"),
            "Contact": inv_in.get("Contact", {}),
            "Date": inv_in.get("Date", "2024-06-15T00:00:00"),
            "DueDate": inv_in.get("DueDate", "2024-07-15T00:00:00"),
            "LineItems": line_items,
            "Total": total,
            "AmountDue": total,
            "AmountPaid": "0.00",
        }
        c.insert(doc)
        result.append(_invoice_public(doc))

    return _envelope("Invoices", result)

# on_get_invoice returns a single invoice by ID.
def on_get_invoice(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_tenant(req)
    if err != None:
        return err

    invoice_id = req["params"].get("id", "")
    if invoice_id == None or invoice_id == "":
        return _xero_err(400, "BadRequest", "ValidationError", "Invoice ID is required")

    c = store_collection("invoices")
    docs = c.list()

    for doc in docs:
        if doc.get("InvoiceID", "") == invoice_id:
            return _envelope("Invoices", [_invoice_public(doc)])

    return _xero_err(404, "NotFound", "NotFound", "The invoice was not found")

# on_post_payment records a payment against an invoice.
def on_post_payment(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_tenant(req)
    if err != None:
        return err

    invoice_id = req["params"].get("id", "")
    if invoice_id == None or invoice_id == "":
        return _xero_err(400, "BadRequest", "ValidationError", "Invoice ID is required")

    body = req.get("body")
    if body == None:
        body = {}

    amount = body.get("Amount", "0.00")
    payment_id = _payment_id()

    # Find the invoice and update amounts.
    c = store_collection("invoices")
    docs = c.list()
    for doc in docs:
        if doc.get("InvoiceID", "") == invoice_id:
            paid = amount
            doc["AmountPaid"] = paid
            doc["AmountDue"] = "0.00"
            doc["Status"] = "PAID"
            c.update(doc.get("id", doc.get("InvoiceID", "")), doc)
            break

    payment = {
        "PaymentID": payment_id,
        "Invoice": {"InvoiceID": invoice_id},
        "Amount": amount,
        "Date": body.get("Date", "2024-06-15T00:00:00"),
    }

    return _envelope("Payments", [payment])
