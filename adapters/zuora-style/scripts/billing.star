# Billing handlers — invoices, payments, payment methods, billing preview.
#
# GET  /v1/invoices/{id}                -> get invoice (with invoiceItems,
#                                         tax fields, appliedPayments)
# GET  /v1/payments                     -> list payments
# POST /v1/payments                     -> create a payment, optionally
#                                         applied to invoices via
#                                         appliedTo[{invoiceId, appliedAmount}]
#                                         (appliedAmount must be <= the
#                                         invoice balance; the invoice balance
#                                         and account balance are decremented;
#                                         status Processed when fully applied,
#                                         Processed-Partially otherwise)
# GET  /v1/payments/{paymentId}         -> get payment (by id or number)
# POST /v1/payments/{paymentId}/unapply -> unapply amount from the payment,
#                                         restoring the invoice + account
#                                         balances
# POST /v1/payment-methods/credit-cards -> create payment method (tokenization)
# POST /v1/transactions/billing/preview -> preview an invoice computed from
#                                         the subscription rate plans

# Shared helpers from lib.star.

def _invoice_view(d):
    out = {
        "success": True,
        "invoiceId": d.get("invoiceId", ""),
        "invoiceNumber": d.get("invoiceNumber", ""),
        "accountId": d.get("accountId", ""),
        "accountNumber": d.get("accountNumber", ""),
        "subscriptionId": d.get("subscriptionId", ""),
        "subscriptionNumber": d.get("subscriptionNumber", ""),
        "amount": d.get("amount", 0),
        "balance": d.get("balance", 0),
        "status": d.get("status", "Posted"),
        "invoiceDate": d.get("invoiceDate", ""),
        "dueDate": d.get("dueDate", ""),
        "currency": d.get("currency", "USD"),
    }
    if "amountWithoutTax" in d:
        out["amountWithoutTax"] = d.get("amountWithoutTax", 0)
    if "taxAmount" in d:
        out["taxAmount"] = d.get("taxAmount", 0)
    if "invoiceItems" in d:
        out["invoiceItems"] = d.get("invoiceItems", [])
    if "type" in d:
        out["type"] = d.get("type", "Invoice")
    if "creditMemoNumber" in d:
        out["creditMemoNumber"] = d.get("creditMemoNumber", "")
    if "appliedPayments" in d:
        out["appliedPayments"] = d.get("appliedPayments", [])
    return out

def on_get_invoice(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    invoice_id = req["params"].get("invoiceId", "")
    d = _find_invoice(invoice_id)
    if d == None:
        return _zuora_err(404, "50000000", "Invoice not found")
    return respond(200, _invoice_view(d))

def _payment_view(d):
    return {
        "paymentId": d.get("paymentId", ""),
        "paymentNumber": d.get("paymentNumber", ""),
        "accountId": d.get("accountId", ""),
        "accountNumber": d.get("accountNumber", ""),
        "amount": d.get("amount", 0),
        "appliedAmount": d.get("appliedAmount", 0),
        "unappliedAmount": d.get("unappliedAmount", 0),
        "status": d.get("status", "Processed"),
        "currency": d.get("currency", "USD"),
        "type": d.get("type", "External"),
        "createdOn": d.get("createdOn", ""),
    }

def on_list_payments(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("payments")
    docs = col.list()

    payments = []
    for d in docs:
        payments.append(_payment_view(d))

    payments = _apply_zuora_filters(req, payments)
    payments, next_cursor = _list_page(req, payments)
    if payments == None:
        return _zuora_err(400, "INVALID_CURSOR", "The cursor parameter is invalid.")
    payments = _apply_zuora_fields(req, payments)

    resp = {
        "success": True,
        "payments": payments,
    }
    if next_cursor != None:
        resp["nextPage"] = next_cursor
    return respond(200, resp)

def on_get_payment(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    key = req["params"].get("paymentId", "")
    col = store_collection("payments")
    for d in col.list():
        if d.get("paymentId", "") == key or d.get("paymentNumber", "") == key:
            return respond(200, _payment_view(d))
    return _zuora_err(404, "50000000", "Payment not found")

# on_create_payment creates a payment and applies it to invoices.
#
# Real semantics honored:
#   - each appliedTo entry's appliedAmount must be <= the invoice's balance
#   - the invoice must exist and belong to the payment's account
#   - total applied must be <= the payment amount
#   - invoice balance and account balance are decremented by what is applied
#   - fully applied -> status "Processed"; partially applied ->
#     "Processed-Partially" with the remainder in unappliedAmount
def on_create_payment(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)
    account_key = body.get("accountKey", body.get("accountId", body.get("AccountId", "")))
    if account_key == None or account_key == "":
        return _zuora_err(400, "50000040", "accountKey is required")

    account = _find_account(str(account_key))
    if account == None:
        return _zuora_err(404, "50000000", "Account not found: " + str(account_key))

    amount = _zuora_try_num(body.get("amount", body.get("Amount", 0)))
    if amount == None or amount <= 0:
        return _zuora_err(400, "50000040", "amount must be a positive number")

    currency = body.get("currency", account.get("currency", "USD"))
    if currency == None or currency == "":
        currency = "USD"
    pay_type = body.get("type", "External")
    if pay_type == None or pay_type == "":
        pay_type = "External"

    apps = body.get("appliedTo", body.get("applyTo", body.get("AppliedTo", [])))
    if apps == None:
        apps = []
    if type(apps) == type({}):
        apps = [apps]

    # Validate every application before mutating anything.
    ic = store_collection("invoices")
    resolved = []
    total_applied = 0.0
    for i in range(len(apps)):
        a = apps[i]
        if a == None:
            continue
        inv_key = a.get("invoiceId", a.get("invoiceNumber", a.get("InvoiceId", "")))
        if inv_key == None or inv_key == "":
            return _zuora_err(400, "50000040", "appliedTo entries require invoiceId")
        inv = _find_invoice(str(inv_key))
        if inv == None:
            return _zuora_err(400, "50200010", "Invoice not found: " + str(inv_key))
        if inv.get("accountId", "") != account.get("accountId", ""):
            return _zuora_err(400, "50200011", "Invoice " + str(inv_key) + " does not belong to the account")
        applied = _zuora_try_num(a.get("appliedAmount", a.get("AppliedAmount", 0)))
        if applied == None or applied < 0:
            return _zuora_err(400, "50200020", "appliedAmount must be a non-negative number")
        if applied > 0:
            balance = inv.get("balance", 0)
            if applied > balance and not _money_eq(applied, balance):
                return _zuora_err(400, "50200020", "appliedAmount " + str(applied) + " exceeds invoice balance " + str(balance))
            if applied > balance:
                applied = balance
            if total_applied + applied > amount and not _money_eq(total_applied + applied, amount):
                return _zuora_err(400, "50200021", "total appliedAmount exceeds payment amount")
            if total_applied + applied > amount:
                applied = _round2(amount - total_applied)
        resolved.append([inv, applied])
        total_applied = _round2(total_applied + applied)

    pay_id = _next_id("payment")
    pay_number = "PMT-SYNTH-" + str(_to_int(pay_id) - 9 * 10000 + 100)

    # Apply: decrement each invoice's balance, record the application, and
    # decrement the account balance by the total applied.
    for i in range(len(resolved)):
        inv = resolved[i][0]
        applied = resolved[i][1]
        if applied <= 0:
            continue
        inv["balance"] = _round2(inv.get("balance", 0) - applied)
        if _money_eq(inv["balance"], 0):
            inv["balance"] = 0.0
        applied_payments = inv.get("appliedPayments", [])
        if applied_payments == None:
            applied_payments = []
        applied_payments.append({
            "paymentId": pay_id,
            "paymentNumber": pay_number,
            "appliedAmount": applied,
            "appliedDate": _today(),
        })
        inv["appliedPayments"] = applied_payments
        ic.update(inv.get("id", ""), inv)

    ac = store_collection("accounts")
    if total_applied > 0:
        account["balance"] = _round2(account.get("balance", 0) - total_applied)
        ac.update(account.get("id", ""), account)

    status = "Processed"
    if not _money_eq(total_applied, amount):
        status = "Processed-Partially"

    applications = []
    for i in range(len(resolved)):
        inv = resolved[i][0]
        applications.append({
            "invoiceId": inv.get("invoiceId", ""),
            "invoiceNumber": inv.get("invoiceNumber", ""),
            "appliedAmount": resolved[i][1],
        })

    doc = {
        "id": pay_id,
        "paymentId": pay_id,
        "paymentNumber": pay_number,
        "accountId": account.get("accountId", ""),
        "accountNumber": account.get("accountNumber", ""),
        "amount": amount,
        "appliedAmount": total_applied,
        "unappliedAmount": _round2(amount - total_applied),
        "status": status,
        "currency": currency,
        "type": pay_type,
        "paymentMethodId": body.get("paymentMethodId", ""),
        "applications": applications,
        "createdOn": _today(),
    }

    pc = store_collection("payments")
    pc.insert(doc)

    _emit_if_subscribed("PaymentProcessed", _callout(
        "Payment",
        "PaymentProcessed",
        "Payment",
        pay_id,
        {
            "PaymentNumber": pay_number,
            "AccountNumber": account.get("accountNumber", ""),
            "Amount": amount,
            "AppliedAmount": total_applied,
            "Status": status,
        },
    ))

    out = _payment_view(doc)
    out["success"] = True
    out["id"] = pay_id
    return respond(200, out)

# on_unapply_payment reverses payment applications, restoring the invoice and
# account balances. Body: {amount} (defaults to the full applied amount) and
# an optional {invoiceId} to target one application.
def on_unapply_payment(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    key = req["params"].get("paymentId", "")
    pc = store_collection("payments")
    doc = None
    for d in pc.list():
        if d.get("paymentId", "") == key or d.get("paymentNumber", "") == key:
            doc = d
            break
    if doc == None:
        return _zuora_err(404, "50000000", "Payment not found")

    status = doc.get("status", "")
    if status != "Processed" and status != "Processed-Partially":
        return _zuora_err(400, "50300010", "Payment in status " + status + " cannot be unapplied")

    applied = doc.get("appliedAmount", 0)
    if _money_eq(applied, 0):
        return _zuora_err(400, "50300011", "Payment has no applied amount")

    body = _get_body(req)
    requested = _zuora_try_num(body.get("amount", applied))
    if requested == None or requested <= 0:
        return _zuora_err(400, "50000040", "amount must be a positive number")
    if requested > applied and not _money_eq(requested, applied):
        return _zuora_err(400, "50300012", "amount exceeds the payment's applied amount")
    if requested > applied:
        requested = applied

    only_invoice = body.get("invoiceId", body.get("invoiceNumber", ""))
    if only_invoice == None:
        only_invoice = ""

    remaining = requested
    ic = store_collection("invoices")
    applications = doc.get("applications", [])
    if applications == None:
        applications = []
    kept = []
    for i in range(len(applications)):
        app = applications[i]
        if remaining <= 0 or _money_eq(remaining, 0):
            kept.append(app)
            continue
        if only_invoice != "" and app.get("invoiceId", "") != only_invoice and app.get("invoiceNumber", "") != only_invoice:
            kept.append(app)
            continue
        amt = app.get("appliedAmount", 0)
        take = amt
        if take > remaining:
            take = remaining
        inv = _find_invoice(app.get("invoiceId", ""))
        if inv != None:
            inv["balance"] = _round2(inv.get("balance", 0) + take)
            ap_list = inv.get("appliedPayments", [])
            if ap_list != None:
                new_list = []
                for j in range(len(ap_list)):
                    ap_entry = ap_list[j]
                    if ap_entry.get("paymentId", "") == doc.get("paymentId", ""):
                        remain_ap = _round2(ap_entry.get("appliedAmount", 0) - take)
                        if remain_ap > 0 and not _money_eq(remain_ap, 0):
                            ap_entry["appliedAmount"] = remain_ap
                            new_list.append(ap_entry)
                        continue
                    new_list.append(ap_entry)
                inv["appliedPayments"] = new_list
            ic.update(inv.get("id", ""), inv)
        remaining = _round2(remaining - take)
        new_amt = _round2(amt - take)
        if new_amt > 0 and not _money_eq(new_amt, 0):
            app["appliedAmount"] = new_amt
            kept.append(app)

    unapplied = requested
    doc["applications"] = kept
    doc["appliedAmount"] = _round2(applied - unapplied)
    doc["unappliedAmount"] = _round2(doc.get("unappliedAmount", 0) + unapplied)
    doc["status"] = "Processed"
    pc.update(doc.get("id", ""), doc)

    if unapplied > 0:
        account = _find_account(doc.get("accountNumber", ""))
        if account == None:
            account = _find_account(doc.get("accountId", ""))
        if account != None:
            ac = store_collection("accounts")
            account["balance"] = _round2(account.get("balance", 0) + unapplied)
            ac.update(account.get("id", ""), account)

    _emit_if_subscribed("PaymentUnapplied", _callout(
        "Payment",
        "PaymentUnapplied",
        "Payment",
        doc.get("paymentId", ""),
        {
            "PaymentNumber": doc.get("paymentNumber", ""),
            "AccountNumber": doc.get("accountNumber", ""),
            "UnappliedAmount": unapplied,
            "AppliedAmount": doc.get("appliedAmount", 0),
        },
    ))

    out = _payment_view(doc)
    out["success"] = True
    out["unapplied"] = unapplied
    return respond(200, out)

def on_create_payment_method(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)
    account_id = body.get("accountId", "")
    if account_id == None:
        account_id = ""

    pm_id = _next_id("payment_method")

    doc = {
        "id": pm_id,
        "paymentMethodId": pm_id,
        "accountId": account_id,
        "type": "CreditCard",
        "cardType": body.get("cardType", "Visa"),
        "expirationMonth": body.get("expirationMonth", "12"),
        "expirationYear": body.get("expirationYear", "2026"),
        "default": body.get("default", True),
        "status": "Active",
    }

    col = store_collection("payment_methods")
    col.insert(doc)

    return respond(200, {
        "success": True,
        "paymentMethodId": pm_id,
        "type": "CreditCard",
    })

# on_preview_billing computes the next invoice from the subscription's rate
# plans (catalog prices + chargeOverrides) instead of a hardcoded document.
# Body: {accountKey} plus an optional {subscriptionKey} to preview one
# subscription (defaults to every Active subscription on the account).
def on_preview_billing(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)
    account_key = body.get("accountKey", body.get("accountId", ""))
    if account_key == None or account_key == "":
        return _zuora_err(400, "50000040", "accountKey is required")

    account = _find_account(str(account_key))
    if account == None:
        return _zuora_err(404, "50000000", "Account not found: " + str(account_key))

    sub_key = body.get("subscriptionKey", body.get("subscriptionId", ""))
    if sub_key == None:
        sub_key = ""

    subs = []
    sc = store_collection("subscriptions")
    if sub_key != "":
        sub = _find_subscription(str(sub_key))
        if sub == None:
            return _zuora_err(404, "50000000", "Subscription not found: " + str(sub_key))
        subs.append(sub)
    else:
        for d in sc.list():
            if d.get("accountId", "") != account.get("accountId", ""):
                continue
            if d.get("status", "") != "Active":
                continue
            subs.append(d)

    items = []
    for i in range(len(subs)):
        sub = subs[i]
        plans = sub.get("subscriptionPlans", [])
        if plans == None:
            plans = []
        for j in range(len(plans)):
            charges = _plan_charge_list(plans[j])
            for k in range(len(charges)):
                ch = charges[k]
                amt = _round2(ch.get("price", 0) * ch.get("quantity", 0))
                tax = _round2(amt * _TAX_RATE)
                items.append({
                    "subscriptionNumber": sub.get("subscriptionNumber", ""),
                    "chargeName": ch.get("chargeName", ""),
                    "quantity": ch.get("quantity", 0),
                    "unitOfMeasure": ch.get("uom", "Each"),
                    "chargeAmount": amt,
                    "taxAmount": tax,
                })

    without_tax = 0.0
    tax_total = 0.0
    for i in range(len(items)):
        without_tax = without_tax + items[i]["chargeAmount"]
        tax_total = tax_total + items[i]["taxAmount"]

    return respond(200, {
        "success": True,
        "accountId": account.get("accountId", ""),
        "currency": account.get("currency", "USD"),
        "invoice": {
            "amount": _round2(without_tax + tax_total),
            "taxAmount": _round2(tax_total),
            "amountWithoutTax": _round2(without_tax),
            "invoiceItems": items,
        },
    })
