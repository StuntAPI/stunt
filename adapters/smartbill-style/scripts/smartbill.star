# Handlers for the SmartBill-style surface (version-free paths, real field
# names, numeric amounts). All data synthetic.

# ── invoices (sales) ───────────────────────────────────────────────────────

def on_invoice_create(req):
    account, err = require_auth(req)
    if err:
        return err
    body = body_of(req)
    if body == None:
        return bad_body()
    company, err = require_cif(req, account, body.get("companyVatCode", ""))
    if err:
        return err
    doc = {
        "companyVatCode": str(company.get("cif")),
        "seriesName": body.get("seriesName", "FCT"),
        "number": next_num("invoices"),
        "issueDate": body.get("issueDate", clock.now_rfc3339()[:10]),
        "dueDate": body.get("dueDate", ""),
        "currency": body.get("currency", "RON"),
        "exchangeRate": body.get("exchangeRate", 1.0),
        "client": body.get("client", {}),
        "supplier": body.get("supplier", {}),
        "products": body.get("products", []),
        "status": "active",
        "sim_account": account,
        "cif": str(company.get("cif")),
    }
    net, vat = line_totals(doc["products"])
    doc["totalNet"] = net
    doc["totalVAT"] = vat
    doc["invoiceTotalAmount"] = round2(net + vat)
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
    series = q1(req.get("query", {}), "seriesname", "")
    number = q1(req.get("query", {}), "number", "")
    d = find_doc("sb_invoices", account, company.get("cif"), series, number)
    if d == None:
        return api_error(404, "Invoice not found")
    return respond(200, strip_internal(d))

def on_invoice_cancel(req):
    account, err = require_auth(req)
    if err:
        return err
    body = body_of(req)
    if body == None:
        return bad_body()
    company, err = require_cif(req, account, body.get("companyVatCode", ""))
    if err:
        return err
    coll = store_collection("sb_invoices")
    d = find_doc("sb_invoices", account, company.get("cif"), body.get("seriesName", ""), body.get("number", ""))
    if d == None:
        return api_error(404, "Invoice not found")
    patch = {}
    for k in d:
        patch[k] = d[k]
    patch["status"] = "canceled"
    patch["cancellationTax"] = body.get("cancellationTax", 0.0)
    coll.update(d.get("id"), patch)
    return respond(200, None)

def on_invoice_restore(req):
    account, err = require_auth(req)
    if err:
        return err
    body = body_of(req)
    if body == None:
        return bad_body()
    company, err = require_cif(req, account, body.get("companyVatCode", ""))
    if err:
        return err
    coll = store_collection("sb_invoices")
    d = find_doc("sb_invoices", account, company.get("cif"), body.get("seriesName", ""), body.get("number", ""))
    if d == None:
        return api_error(404, "Invoice not found")
    patch = {}
    for k in d:
        patch[k] = d[k]
    patch["status"] = "active"
    coll.update(d.get("id"), patch)
    return respond(200, None)

def on_invoice_paymentstatus(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    series = q1(req.get("query", {}), "seriesname", "")
    number = q1(req.get("query", {}), "number", "")
    d = find_doc("sb_invoices", account, company.get("cif"), series, number)
    if d == None:
        return api_error(404, "Invoice not found")
    total = to_num(d.get("invoiceTotalAmount", 0))
    paid = 0.0
    for p in rows_of("sb_payments", account, company.get("cif")):
        if p.get("canceled", False):
            continue
        for inv in p.get("invoicesList", []):
            if str(inv.get("seriesName", "")) == str(series) and str(inv.get("number", "")) == str(number):
                paid = paid + to_num(p.get("value", 0))
    paid = round2(paid)
    unpaid = round2(total - paid)
    if unpaid < 0:
        unpaid = 0.0
    return respond(200, {
        "invoiceTotalAmount": total,
        "paidAmount": paid,
        "unpaidAmount": unpaid,
        "paid": total > 0 and paid >= total,
    })

# ── estimates (proforma) ───────────────────────────────────────────────────

def on_estimate_create(req):
    account, err = require_auth(req)
    if err:
        return err
    body = body_of(req)
    if body == None:
        return bad_body()
    company, err = require_cif(req, account, body.get("companyVatCode", ""))
    if err:
        return err
    doc = {
        "companyVatCode": str(company.get("cif")),
        "seriesName": body.get("seriesName", "PRO"),
        "number": next_num("estimates"),
        "issueDate": body.get("issueDate", clock.now_rfc3339()[:10]),
        "currency": body.get("currency", "RON"),
        "client": body.get("client", {}),
        "products": body.get("products", []),
        "status": "active",
        "sim_account": account,
        "cif": str(company.get("cif")),
    }
    net, vat = line_totals(doc["products"])
    doc["totalNet"] = net
    doc["totalVAT"] = vat
    doc["invoiceTotalAmount"] = round2(net + vat)
    store_collection("sb_estimates").insert(doc)
    return respond(200, None)

def on_estimate_get(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    series = q1(req.get("query", {}), "seriesname", "")
    number = q1(req.get("query", {}), "number", "")
    d = find_doc("sb_estimates", account, company.get("cif"), series, number)
    if d == None:
        return api_error(404, "Estimate not found")
    return respond(200, strip_internal(d))

def on_estimate_cancel(req):
    account, err = require_auth(req)
    if err:
        return err
    body = body_of(req)
    if body == None:
        return bad_body()
    company, err = require_cif(req, account, body.get("companyVatCode", ""))
    if err:
        return err
    coll = store_collection("sb_estimates")
    d = find_doc("sb_estimates", account, company.get("cif"), body.get("seriesName", ""), body.get("number", ""))
    if d == None:
        return api_error(404, "Estimate not found")
    patch = {}
    for k in d:
        patch[k] = d[k]
    patch["status"] = "canceled"
    coll.update(d.get("id"), patch)
    return respond(200, None)

# ── purchase invoices — the spend side ─────────────────────────────────────

def on_purchase_create(req):
    account, err = require_auth(req)
    if err:
        return err
    body = body_of(req)
    if body == None:
        return bad_body()
    company, err = require_cif(req, account, body.get("companyVatCode", ""))
    if err:
        return err
    doc = {
        "companyVatCode": str(company.get("cif")),
        "seriesName": body.get("seriesName", "ACH"),
        "number": next_num("purchases"),
        "issueDate": body.get("issueDate", clock.now_rfc3339()[:10]),
        "currency": body.get("currency", "RON"),
        "supplier": body.get("supplier", {}),
        "products": body.get("products", []),
        "sim_account": account,
        "cif": str(company.get("cif")),
    }
    net, vat = line_totals(doc["products"])
    doc["totalNet"] = net
    doc["totalVAT"] = vat
    doc["invoiceTotalAmount"] = round2(net + vat)
    store_collection("sb_purchases").insert(doc)
    return respond(200, None)

def on_purchase_get(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    series = q1(req.get("query", {}), "seriesname", "")
    number = q1(req.get("query", {}), "number", "")
    d = find_doc("sb_purchases", account, company.get("cif"), series, number)
    if d == None:
        return api_error(404, "Purchase invoice not found")
    return respond(200, strip_internal(d))

# ── payments ───────────────────────────────────────────────────────────────
# Real shape: POST /payment {"payment": {companyVatCode, value, type, isCash,
# invoicesList: [{seriesName, number}]}}. The identity (paymentId) is a
# simulator-visible extension so the delete flow is driveable.

def on_payment_add(req):
    account, err = require_auth(req)
    if err:
        return err
    body = body_of(req)
    if body == None:
        return bad_body()
    p = body.get("payment", {})
    if p == None or type(p) != "dict":
        return api_error(422, "The payment field is required.")
    company, err = require_cif(req, account, p.get("companyVatCode", ""))
    if err:
        return err
    payment_id = store_kv_incr("ids", "sb_payments_id")
    doc = {
        "paymentId": payment_id,
        "companyVatCode": str(company.get("cif")),
        "value": to_num(p.get("value", 0)),
        "type": p.get("type", "CHITANTA"),
        "isCash": p.get("isCash", True),
        "currency": p.get("currency", "RON"),
        "date": p.get("date", clock.now_rfc3339()[:10]),
        "invoicesList": p.get("invoicesList", []),
        "canceled": False,
        "sim_account": account,
        "cif": str(company.get("cif")),
    }
    store_collection("sb_payments").insert(doc)
    return respond(200, {"paymentId": payment_id})

def on_payment_delete(req):
    account, err = require_auth(req)
    if err:
        return err
    body = body_of(req)
    if body == None:
        return bad_body()
    company, err = require_cif(req, account, body.get("companyVatCode", ""))
    if err:
        return err
    want = _num_key(body.get("paymentId", ""))
    for d in rows_of("sb_payments", account, company.get("cif")):
        if _num_key(d.get("paymentId", "")) == want:
            store_collection("sb_payments").delete(d.get("id"))
            return respond(200, None)
    return api_error(404, "Payment not found")

# ── stocks ─────────────────────────────────────────────────────────────────

def on_stocks_list(req):
    account, err = require_auth(req)
    if err:
        return err
    company, err = require_cif(req, account)
    if err:
        return err
    query = req.get("query", {})
    wh = q1(query, "warehouseName", "")
    pn = q1(query, "productName", "")
    pc = q1(query, "productCode", "")
    rows = rows_of("sb_stocks", account, company.get("cif"))
    by_warehouse = {}
    order = []
    for s in rows:
        warehouse_name = str(s.get("warehouseName", "Main"))
        if wh != "" and warehouse_name != wh:
            continue
        product = {
            "productCode": s.get("productCode", ""),
            "productName": s.get("productName", ""),
            "quantity": to_num(s.get("quantity", 0)),
            "measuringUnit": s.get("measuringUnit", "buc"),
        }
        if pn != "" and str(product["productName"]).find(pn) < 0:
            continue
        if pc != "" and str(product["productCode"]) != pc:
            continue
        if warehouse_name not in by_warehouse:
            by_warehouse[warehouse_name] = []
            order.append(warehouse_name)
        by_warehouse[warehouse_name].append(product)
    out = []
    for warehouse_name in order:
        out.append({
            "warehouse": {
                "warehouseName": warehouse_name,
                "warehouseType": "Depozit",
            },
            "products": by_warehouse[warehouse_name],
        })
    return respond(200, {"list": out})

# ── document send ──────────────────────────────────────────────────────────

def on_document_send(req):
    account, err = require_auth(req)
    if err:
        return err
    body = body_of(req)
    if body == None:
        return bad_body()
    sr = body.get("sendDocumentRequest", {})
    if sr == None or type(sr) != "dict":
        return api_error(422, "The sendDocumentRequest field is required.")
    company, err = require_cif(req, account, sr.get("companyVatCode", ""))
    if err:
        return err
    doc = {
        "companyVatCode": str(company.get("cif")),
        "seriesName": sr.get("seriesName", ""),
        "number": sr.get("number", ""),
        "type": sr.get("type", "factura"),
        "subject": sr.get("subject", ""),
        "to": sr.get("to", ""),
        "cc": sr.get("cc", ""),
        "bcc": sr.get("bcc", ""),
        "bodyText": sr.get("bodyText", ""),
        "sentAt": clock.now_rfc3339(),
        "sim_account": account,
        "cif": str(company.get("cif")),
    }
    store_collection("sb_messages").insert(doc)
    return respond(200, None)

# ── metadata ───────────────────────────────────────────────────────────────

def on_tax_list(req):
    account, err = require_auth(req)
    if err:
        return err
    return respond(200, {"list": [
        {"name": "Normala", "percentage": 19.0},
        {"name": "Redusa", "percentage": 9.0},
        {"name": "Redusa5", "percentage": 5.0},
        {"name": "Scutit", "percentage": 0.0},
    ]})

def on_series_list(req):
    account, err = require_auth(req)
    if err:
        return err
    return respond(200, {"list": [
        {"name": "FCT", "type": "factura"},
        {"name": "PRO", "type": "proforma"},
        {"name": "ACH", "type": "factura_achizitie"},
    ]})

# ── simulator affordances (namespaced, NOT real endpoints) ─────────────────

def on_sim_company_create(req):
    # Registering a cif; the real API assumes the company exists behind the
    # credentials.
    user, err = require_auth(req)
    if err:
        return err
    body = body_of(req)
    if body == None:
        return bad_body()
    doc = {
        "cif": str(body.get("cif", "")),
        "name": body.get("name", ""),
        "sim_account": user,
    }
    if doc["cif"] == "":
        return api_error(422, "cif is required")
    companies().insert(doc)
    return respond(201, strip_internal(doc))

def on_sim_stock_movement(req):
    # Stands in for warehouse operations driven by the SmartBill UI (the API
    # surface only exposes stock reads); applies in/out movements.
    account, err = require_auth(req)
    if err:
        return err
    body = body_of(req)
    if body == None:
        return bad_body()
    company, err = require_cif(req, account, body.get("cif", ""))
    if err:
        return err
    code = body.get("productCode", "")
    wh = body.get("warehouseName", "Main")
    qty = to_num(body.get("quantity", 0))
    mtype = body.get("type", "in")
    coll = store_collection("sb_stocks")
    for s in rows_of("sb_stocks", account, company.get("cif")):
        if s.get("productCode", "") == code and s.get("warehouseName", wh) == wh:
            patch = {}
            for k in s:
                patch[k] = s[k]
            cur = to_num(patch.get("quantity", 0))
            patch["quantity"] = round2(cur + qty) if mtype == "in" else round2(cur - qty)
            coll.update(s.get("id"), patch)
            return respond(200, None)
    if mtype != "in":
        return api_error(422, "Not enough stock")
    coll.insert({
        "productCode": code,
        "productName": body.get("productName", ""),
        "quantity": qty,
        "measuringUnit": body.get("measuringUnit", "buc"),
        "warehouseName": wh,
        "cif": str(company.get("cif")),
        "sim_account": account,
    })
    return respond(200, None)
