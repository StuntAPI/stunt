# Invoices handlers — list, create, get, payment, void.
#
# Requires Bearer + xero-tenant-id.
# STATEFUL invoices stored in the "invoices" collection.
#
# GET    /api.xro/2.0/Invoices           → { Id, Status, Invoices: [...] }
# PUT    /api.xro/2.0/Invoices           → { Id, Status, Invoices: [...] }
# GET    /api.xro/2.0/Invoices/{id}      → { Id, Status, Invoices: [...] }
# DELETE /api.xro/2.0/Invoices/{id}      → 204 No Content (VOID, not destroy)
# POST   /api.xro/2.0/Invoices/{id}/Payments → { Id, Status, Payments: [...] }

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

    invoices = _apply_invoice_filters(req, invoices)
    invoices, next_page = _list_page(req, invoices)
    return _envelope("Invoices", invoices, next_page)

# _apply_invoice_filters maps the real Xero GET /Invoices query params to
# query_select clauses, applied before paging like the real API:
#   Statuses=DRAFT,AUTHORISED (comma list)   InvoiceNumber=INV-001,INV-002
#   ContactID=<guid>                         where=Type=="ACCREC"
#   order=InvoiceNumber DESC
def _apply_invoice_filters(req, invoices):
    f = _coerce_triples(_where_triples(_get_query(req, "where")), invoices)

    statuses = _get_query(req, "Statuses")
    if statuses != "":
        vals = []
        for part in _split(statuses, ","):
            part = _trim(part)
            if part != "":
                vals.append(part)
        if len(vals) > 0:
            f.append(["Status", "in", vals])

    numbers = _get_query(req, "InvoiceNumber")
    if numbers != "":
        vals = []
        for part in _split(numbers, ","):
            part = _trim(part)
            if part != "":
                vals.append(part)
        if len(vals) > 0:
            f.append(["InvoiceNumber", "in", vals])

    contact_id = _get_query(req, "ContactID")
    if contact_id != "":
        f.append(["Contact.ContactID", "=", contact_id])

    order = _order_parts(req)
    if len(f) == 0 and order[0] == "":
        return invoices
    filt = None
    if len(f) > 0:
        filt = f
    return query_select(invoices, filt, order[0], order[1], None, None, None)

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
            "Date": inv_in.get("Date", _now_dt()),
            "DueDate": inv_in.get("DueDate", _plus_days_dt(_DEFAULT_TERMS_DAYS)),
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

# on_delete_invoice VOIDS a single invoice by ID (real Xero invoices are not
# destroyed — the terminal soft-delete state is VOIDED). Like the real API's
# void, the record is kept with Status "VOIDED" and the outstanding balance
# zeroed; a PAID or already-VOIDED invoice cannot be voided again (400
# ValidationError), and the invoice remains retrievable by id afterwards.
def on_delete_invoice(req):
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
            status = doc.get("Status", "DRAFT")
            if status == "VOIDED" or status == "PAID":
                return _xero_err(400, "BadRequest", "ValidationError", "Invoice with InvoiceID " + invoice_id + " is not deletable (status " + status + ")")
            doc["Status"] = "VOIDED"
            doc["AmountDue"] = "0.00"
            doc["UpdatedDateUTC"] = _now_dt()
            c.update(doc.get("id", doc.get("InvoiceID", "")), doc)
            return respond(204)

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
        "Date": body.get("Date", _now_dt()),
    }

    return _envelope("Payments", [payment])
