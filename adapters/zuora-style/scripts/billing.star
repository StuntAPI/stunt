# Billing handlers — invoices, payments, payment methods, billing preview.
#
# GET  /v1/invoices/{id}               -> get invoice
# GET  /v1/payments                    -> list payments
# POST /v1/payment-methods/credit-cards-> create payment method (tokenization)
# POST /v1/transactions/billing/preview-> preview a subscription invoice

# Shared helpers from lib.star.

def on_get_invoice(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    invoice_id = req["params"].get("invoiceId", "")
    col = store_collection("invoices")
    docs = col.list()

    for d in docs:
        if d.get("invoiceId", "") == invoice_id or d.get("invoiceNumber", "") == invoice_id:
            return respond(200, {
                "success": True,
                "invoiceId": d.get("invoiceId", ""),
                "invoiceNumber": d.get("invoiceNumber", ""),
                "accountId": d.get("accountId", ""),
                "accountNumber": d.get("accountNumber", ""),
                "amount": d.get("amount", 0),
                "balance": d.get("balance", 0),
                "status": d.get("status", "Posted"),
                "invoiceDate": d.get("invoiceDate", ""),
                "dueDate": d.get("dueDate", ""),
                "currency": d.get("currency", "USD"),
            })

    return _zuora_err(404, "50000000", "Invoice not found")

def on_list_payments(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("payments")
    docs = col.list()

    payments = []
    for d in docs:
        payments.append({
            "paymentId": d.get("paymentId", ""),
            "paymentNumber": d.get("paymentNumber", ""),
            "accountId": d.get("accountId", ""),
            "amount": d.get("amount", 0),
            "status": d.get("status", "Processed"),
            "currency": d.get("currency", "USD"),
        })

    return respond(200, {
        "success": True,
        "payments": payments,
    })

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

def on_preview_billing(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)
    account_key = body.get("accountKey", "")
    if account_key == None:
        account_key = ""

    return respond(200, {
        "success": True,
        "accountId": account_key,
        "currency": "USD",
        "invoice": {
            "amount": 99.00,
            "taxAmount": 9.90,
            "amountWithoutTax": 89.10,
            "invoiceItems": [{
                "chargeName": "Monthly Subscription",
                "quantity": 1,
                "unitOfMeasure": "Each",
                "chargeAmount": 89.10,
                "taxAmount": 9.90,
            }],
        },
    })
