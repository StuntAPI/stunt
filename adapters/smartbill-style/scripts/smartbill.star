# Handlers for the SmartBill-style v1 surface. All data synthetic.

INVOICE_DEFAULTS = {
    "series": "FCT",
    "number": "",
    "issueDate": "",
    "dueDate": "",
    "currency": "RON",
    "exchangeRate": "1.00",
    "buyerName": "",
    "buyerCif": "",
    "products": [],
    "status": "active",
}

PURCHASE_DEFAULTS = {
    "series": "ACH",
    "number": "",
    "issueDate": "",
    "dueDate": "",
    "currency": "RON",
    "supplierName": "",
    "supplierCif": "",
    # Each product line carries the book classification.
    "products": [],
}

ESTIMATE_DEFAULTS = {
    "series": "PRO",
    "number": "",
    "issueDate": "",
    "currency": "RON",
    "buyerName": "",
    "products": [],
    "status": "active",
}

# ── company ────────────────────────────────────────────────────────────────

def on_company(req):
    account, err = require_auth(req)
    if err:
        return err
    rows = [strip_internal(c) for c in companies_of(account)]
    return respond(200, {"companies": rows})

def on_company_create(req):
    # Simulator bootstrap (registering a cif); the real API assumes the
    # company exists behind the credentials. POST /sim/company.
    user, err = require_auth(req)
    if err:
        return err
    body = body_of(req)
    doc = {
        "cif": str(body.get("cif", "")),
        "name": body.get("name", ""),
        "sim_account": user,
    }
    if doc["cif"] == "":
        return api_error(422, "validation", "cif is required")
    companies().insert(doc)
    return respond(201, strip_internal(doc))

# ── invoices (sales) ───────────────────────────────────────────────────────

def on_invoice_create(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    body = body_of(req)
    doc = {}
    for k in INVOICE_DEFAULTS:
        doc[k] = INVOICE_DEFAULTS[k]
    for k in body:
        doc[k] = body[k]
    doc["number"] = next_id("invoices")
    doc["sim_account"] = account
    doc["cif"] = str(company.get("cif"))
    store_collection("sb_invoices").insert(doc)
    # The real API returns an empty body on create.
    return respond(200, None)

def on_invoice_get(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    series = q1(req.get("query", {}), "series", "")
    number = q1(req.get("query", {}), "number", "")
    for d in rows_of("sb_invoices", account, company.get("cif")):
        if str(d.get("series", "")) == series and str(d.get("number", "")) == number:
            return respond(200, strip_internal(d))
    return api_error(404, "not_found", "Invoice not found")

def on_invoice_list(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    wanted = date_wanted(req)
    rows = [strip_internal(d) for d in rows_of("sb_invoices", account, company.get("cif")) if wanted(d)]
    rows = by_date(rows)
    chunk, meta = page_slice(rows, req)
    body = {"invoices": chunk}
    for k in meta:
        body[k] = meta[k]
    return respond(200, body)

def on_invoice_cancel(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    series = q1(req.get("query", {}), "series", "")
    number = q1(req.get("query", {}), "number", "")
    coll = store_collection("sb_invoices")
    for d in rows_of("sb_invoices", account, company.get("cif")):
        if str(d.get("series", "")) == series and str(d.get("number", "")) == number:
            patch = {}
            for k in d:
                patch[k] = d[k]
            patch["status"] = "canceled"
            coll.update(d.get("id"), patch)
            return respond(200, None)
    return api_error(404, "not_found", "Invoice not found")

# ── invoice payments ───────────────────────────────────────────────────────

def on_invoice_payment_add(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    body = body_of(req)
    coll = store_collection("sb_payments")
    doc = {
        "id": next_id("payments"),
        "cif": str(company.get("cif")),
        "series": body.get("series", ""),
        "number": body.get("number", ""),
        "amount": str(body.get("amount", "0.00")),
        "currency": body.get("currency", "RON"),
        "date": body.get("date", ""),
        "type": body.get("type", "custom"),
        "sim_account": account,
    }
    coll.insert(doc)
    return respond(200, None)

def on_invoice_payment_delete(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    ident = q1(req.get("query", {}), "id", "")
    coll = store_collection("sb_payments")
    for d in rows_of("sb_payments", account, company.get("cif")):
        if str(d.get("id")) == ident:
            coll.delete(d.get("id"))
            return respond(200, None)
    return api_error(404, "not_found", "Payment not found")

# ── estimates (proforma) ───────────────────────────────────────────────────

def on_estimate_create(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    body = body_of(req)
    doc = {}
    for k in ESTIMATE_DEFAULTS:
        doc[k] = ESTIMATE_DEFAULTS[k]
    for k in body:
        doc[k] = body[k]
    doc["number"] = next_id("estimates")
    doc["sim_account"] = account
    doc["cif"] = str(company.get("cif"))
    store_collection("sb_estimates").insert(doc)
    return respond(200, None)

def on_estimate_list(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    wanted = date_wanted(req)
    rows = [strip_internal(d) for d in rows_of("sb_estimates", account, company.get("cif")) if wanted(d)]
    rows = by_date(rows)
    chunk, meta = page_slice(rows, req)
    body = {"estimates": chunk}
    for k in meta:
        body[k] = meta[k]
    return respond(200, body)

def on_estimate_cancel(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    series = q1(req.get("query", {}), "series", "")
    number = q1(req.get("query", {}), "number", "")
    coll = store_collection("sb_estimates")
    for d in rows_of("sb_estimates", account, company.get("cif")):
        if str(d.get("series", "")) == series and str(d.get("number", "")) == number:
            patch = {}
            for k in d:
                patch[k] = d[k]
            patch["status"] = "canceled"
            coll.update(d.get("id"), patch)
            return respond(200, None)
    return api_error(404, "not_found", "Estimate not found")

# ── purchase invoices — the spend side ─────────────────────────────────────

def on_purchase_create(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    body = body_of(req)
    doc = {}
    for k in PURCHASE_DEFAULTS:
        doc[k] = PURCHASE_DEFAULTS[k]
    for k in body:
        doc[k] = body[k]
    doc["number"] = next_id("purchases")
    doc["sim_account"] = account
    doc["cif"] = str(company.get("cif"))
    store_collection("sb_purchases").insert(doc)
    return respond(200, None)

def on_purchase_list(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    wanted = date_wanted(req)
    rows = [strip_internal(d) for d in rows_of("sb_purchases", account, company.get("cif")) if wanted(d)]
    rows = by_date(rows)
    chunk, meta = page_slice(rows, req)
    body = {"invoices": chunk}
    for k in meta:
        body[k] = meta[k]
    return respond(200, body)

# ── stocks ─────────────────────────────────────────────────────────────────

def on_stocks_list(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    rows = [strip_internal(d) for d in rows_of("sb_stocks", account, company.get("cif"))]
    return respond(200, {"stocks": rows})

def on_stocks_movement(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    body = body_of(req)
    doc = {
        "id": next_id("stock_movements"),
        "cif": str(company.get("cif")),
        "product": body.get("product", ""),
        "quantity": str(body.get("quantity", "0")),
        "warehouse": body.get("warehouse", "Main"),
        "date": body.get("date", ""),
        "type": body.get("type", "in"),
        "sim_account": account,
    }
    store_collection("sb_stock_movements").insert(doc)
    return respond(200, None)

# ── messages ───────────────────────────────────────────────────────────────

def on_message_email(req):
    # Records the message; there is no delivery to simulate observably.
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    body = body_of(req)
    doc = {
        "id": next_id("messages"),
        "cif": str(company.get("cif")),
        "to": body.get("to", ""),
        "subject": body.get("subject", ""),
        "sim_account": account,
    }
    store_collection("sb_messages").insert(doc)
    return respond(200, None)
