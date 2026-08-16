# Invoices handlers — list, create, get, payment, void.
#
# Requires Bearer + xero-tenant-id.
# STATEFUL invoices stored in the "invoices" collection, payments in the
# "payments" collection.
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

# _compute_invoice_amounts sums EVERY line item — not just the first — into
# the real API's invoice totals. Per line: net = UnitAmount × Quantity less
# DiscountRate% (a LineAmount-only line is taken as-is), tax = TaxAmount.
# Returns the line items with each computed LineAmount echoed back, plus the
# SubTotal / TotalTax / TotalDiscount cent subtotals.
def _compute_invoice_amounts(line_items):
    out_lines = []
    sub = 0
    tax = 0
    disc = 0
    for li in line_items:
        if li == None:
            li = {}
        net = _line_net_cents(li)
        line = {}
        for k in li:
            line[k] = li[k]
        line["LineAmount"] = _fmt_cents(net)
        out_lines.append(line)
        sub = sub + net
        tax = tax + _line_tax_cents(li)
        disc = disc + _line_discount_cents(li, net)
    return out_lines, sub, tax, disc

# on_put_invoices creates invoices. Totals are derived from all line items:
# SubTotal = Σ line nets, TotalTax = Σ per-line TaxAmount, Total = SubTotal +
# TotalTax, TotalDiscount = Σ per-line discount portions, and the initial
# AmountDue (outstanding balance) equals Total.
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

        line_items, sub, tax, disc = _compute_invoice_amounts(line_items)

        doc = {
            "InvoiceID": invoice_id,
            "InvoiceNumber": inv_in.get("InvoiceNumber", "INV-" + invoice_id[0:6].upper()),
            "Type": inv_in.get("Type", "ACCREC"),
            "Status": inv_in.get("Status", "DRAFT"),
            "Contact": inv_in.get("Contact", {}),
            "Date": inv_in.get("Date", _now_dt()),
            "DueDate": inv_in.get("DueDate", _plus_days_dt(_DEFAULT_TERMS_DAYS)),
            "LineItems": line_items,
            "SubTotal": _fmt_cents(sub),
            "TotalTax": _fmt_cents(tax),
            "Total": _fmt_cents(sub + tax),
            "TotalDiscount": _fmt_cents(disc),
            "AmountDue": _fmt_cents(sub + tax),
            "AmountPaid": "0.00",
            "UpdatedDateUTC": _now_dt(),
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

# _PAYABLE_STATUSES are the invoice states the real API accepts payments
# against (a bill moves DRAFT → SUBMITTED → AUTHORISED before payment).
_PAYABLE_STATUSES = ["AUTHORISED", "SUBMITTED"]

# on_post_payment records a payment against an invoice. Payments ACCUMULATE:
# each application grows AmountPaid and shrinks AmountDue (the outstanding
# balance) on the stored invoice, so 2nd/3rd partial payments decrement the
# balance correctly and the balance can never go negative — an over-payment
# is rejected with the real API's validation error. The invoice flips to
# PAID exactly when the balance reaches zero; payments are only accepted
# against AUTHORISED/SUBMITTED invoices; each payment is persisted in the
# "payments" collection before the response is returned.
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

    amount_c = _amt_cents(body.get("Amount", "0.00"))

    c = store_collection("invoices")
    for doc in c.list():
        if doc.get("InvoiceID", "") != invoice_id:
            continue

        status = doc.get("Status", "DRAFT")
        payable = False
        for s in _PAYABLE_STATUSES:
            if s == status:
                payable = True
        if not payable:
            return _validation_err("Payments can only be made against AUTHORISED documents")

        due_c = _amt_cents(doc.get("AmountDue", "0.00"))
        paid_c = _amt_cents(doc.get("AmountPaid", "0.00"))
        # Over-payment (and a refund larger than what was paid) is rejected;
        # the balance arithmetic below therefore never goes negative.
        if amount_c > due_c or (amount_c < 0 and -amount_c > paid_c):
            return _validation_err("PaymentAmount exceeds the amount outstanding on this document")

        new_due = due_c - amount_c
        doc["AmountPaid"] = _fmt_cents(paid_c + amount_c)
        doc["AmountDue"] = _fmt_cents(new_due)
        if new_due == 0:
            doc["Status"] = "PAID"
        doc["UpdatedDateUTC"] = _now_dt()
        c.update(doc.get("id", invoice_id), doc)

        payment = {
            "PaymentID": _payment_id(),
            "Invoice": {
                "InvoiceID": invoice_id,
                "InvoiceNumber": doc.get("InvoiceNumber", ""),
            },
            "Account": body.get("Account", {}),
            "Amount": _fmt_cents(amount_c),
            "Date": body.get("Date", _now_dt()),
            "Status": "AUTHORISED",
        }
        store_collection("payments").insert(payment)
        return _envelope("Payments", [_payment_public(payment)])

    return _xero_err(404, "NotFound", "NotFound", "The invoice was not found")
