# Subscription handlers — Zuora Billing subscriptions.
#
# GET  /v1/subscriptions/{key}        -> get subscription
# POST /v1/subscriptions              -> create subscription (complex billing model)
# PUT  /v1/subscriptions/{key}        -> update/renew subscription
# POST /v1/subscriptions/{key}/cancel -> cancel subscription

# Shared helpers from lib.star.

def _subscription_shape(doc):
    end_date = doc.get("endDate", None)
    if end_date == None:
        end_date = None

    plans = doc.get("subscriptionPlans", [])
    if plans == None:
        plans = []

    return {
        "subscriptionId": doc.get("subscriptionId", doc.get("id", "")),
        "subscriptionNumber": doc.get("subscriptionNumber", ""),
        "accountNumber": doc.get("accountNumber", ""),
        "accountId": doc.get("accountId", ""),
        "status": doc.get("status", "Active"),
        "termType": doc.get("termType", "TERMED"),
        "contractEffectiveDate": doc.get("contractEffectiveDate", ""),
        "startDate": doc.get("startDate", ""),
        "endDate": end_date,
        "subscriptionPlans": plans,
    }

def on_get_subscription(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    sub_key = req["params"].get("subscriptionKey", "")
    col = store_collection("subscriptions")
    docs = col.list()

    for d in docs:
        if d.get("subscriptionId", "") == sub_key or d.get("subscriptionNumber", "") == sub_key:
            return respond(200, _subscription_shape(d))

    return _zuora_err(404, "50000000", "Subscription not found")

def on_create_subscription(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)
    account_key = body.get("accountKey", "")
    if account_key == None:
        account_key = ""

    # Look up the account.
    ac = store_collection("accounts")
    adocs = ac.list()
    account = None
    for a in adocs:
        if a.get("accountId", "") == account_key or a.get("accountNumber", "") == account_key:
            account = a
            break

    if account == None:
        return _zuora_err(400, "50000001", "Account not found: " + account_key)

    sub_id = _next_id("subscription")
    sub_number = "SUB-SYNTH-" + str(_to_int(sub_id) - 90000 + 100)

    # Build subscription plans from the request.
    plans = body.get("subscribeToRatePlans", [])
    if plans == None:
        plans = []
    sub_plans = []
    for i in range(len(plans)):
        p = plans[i]
        sub_plans.append({
            "id": "plan-" + str(i + 1),
            "productRatePlanId": p.get("productRatePlanId", ""),
            "productRatePlanName": p.get("productRatePlanName", "Standard Plan"),
        })

    term_type = body.get("termType", "TERMED")
    if term_type == None:
        term_type = "TERMED"

    doc = {
        "id": sub_id,
        "subscriptionId": sub_id,
        "subscriptionNumber": sub_number,
        "accountNumber": account.get("accountNumber", ""),
        "accountId": account.get("accountId", ""),
        "status": "Active",
        "termType": term_type,
        "contractEffectiveDate": body.get("contractEffectiveDate", "2024-01-01"),
        "startDate": body.get("contractEffectiveDate", "2024-01-01"),
        "endDate": body.get("termEndDate", None),
        "subscriptionPlans": sub_plans,
    }

    col = store_collection("subscriptions")
    col.insert(doc)

    return respond(200, {
        "success": True,
        "subscriptionId": sub_id,
        "subscriptionNumber": sub_number,
        "accountNumber": account.get("accountNumber", ""),
        "status": "Active",
    })

def on_update_subscription(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    sub_key = req["params"].get("subscriptionKey", "")
    col = store_collection("subscriptions")
    docs = col.list()

    doc = None
    for d in docs:
        if d.get("subscriptionId", "") == sub_key or d.get("subscriptionNumber", "") == sub_key:
            doc = d
            break

    if doc == None:
        return _zuora_err(404, "50000000", "Subscription not found")

    body = _get_body(req)

    updated = {}
    for k in doc:
        updated[k] = doc[k]
    if "contractEffectiveDate" in body:
        updated["contractEffectiveDate"] = body.get("contractEffectiveDate")
    if "termType" in body:
        updated["termType"] = body.get("termType")

    col.update(doc.get("id", ""), updated)
    return respond(200, {
        "success": True,
        "subscriptionId": updated.get("subscriptionId", ""),
        "subscriptionNumber": updated.get("subscriptionNumber", ""),
        "status": updated.get("status", "Active"),
    })

def on_cancel_subscription(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    sub_key = req["params"].get("subscriptionKey", "")
    col = store_collection("subscriptions")
    docs = col.list()

    doc = None
    for d in docs:
        if d.get("subscriptionId", "") == sub_key or d.get("subscriptionNumber", "") == sub_key:
            doc = d
            break

    if doc == None:
        return _zuora_err(404, "50000000", "Subscription not found")

    updated = {}
    for k in doc:
        updated[k] = doc[k]
    updated["status"] = "Canceled"

    col.update(doc.get("id", ""), updated)
    return respond(200, {
        "success": True,
        "subscriptionId": updated.get("subscriptionId", ""),
        "subscriptionNumber": updated.get("subscriptionNumber", ""),
        "status": "Canceled",
    })
