# Subscription handlers — Zuora Billing subscriptions.
#
# GET  /v1/subscriptions/{key}        -> get subscription (advances pending
#                                        end-of-term cancellations on read)
# POST /v1/subscriptions              -> create subscription: validates the
#                                        account + rate plans, computes the
#                                        term, generates the FIRST INVOICE
#                                        from the rate plan charges, and
#                                        increments the account balance
# PUT  /v1/subscriptions/{key}        -> update: add/remove rate plans, change
#                                        terms, deep-merge everything else
# POST /v1/subscriptions/{key}/cancel -> cancel honoring cancellationPolicy:
#                                        EndOfTerm (default) keeps the
#                                        subscription Active until the term
#                                        ends (derive-on-read flip to
#                                        Canceled); Immediate cancels now and
#                                        issues a prorated credit memo for
#                                        the uninvoiced remainder

# Shared helpers from lib.star.

def _subscription_shape(doc):
    end_date = doc.get("endDate", None)
    if end_date == None:
        end_date = None

    plans = doc.get("subscriptionPlans", [])
    if plans == None:
        plans = []

    out = {
        "subscriptionId": doc.get("subscriptionId", doc.get("id", "")),
        "subscriptionNumber": doc.get("subscriptionNumber", ""),
        "accountNumber": doc.get("accountNumber", ""),
        "accountId": doc.get("accountId", ""),
        "status": doc.get("status", "Active"),
        "termType": doc.get("termType", "TERMED"),
        "initialTerm": doc.get("initialTerm", 12),
        "contractEffectiveDate": doc.get("contractEffectiveDate", ""),
        "startDate": doc.get("startDate", ""),
        "endDate": end_date,
        "subscriptionPlans": plans,
    }
    custom = doc.get("customFields", None)
    if custom != None:
        out["customFields"] = custom
    policy = doc.get("cancellationPolicy", "")
    if policy != "" and out["status"] == "Active":
        out["cancellationPolicy"] = policy
        out["cancellationEffectiveDate"] = doc.get("cancellationEffectiveDate", "")
    return out

def on_get_subscription(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    sub_key = req["params"].get("subscriptionKey", "")
    doc = _find_subscription(sub_key)
    if doc == None:
        return _zuora_err(404, "50000000", "Subscription not found")

    # Derive-on-read: flip pending end-of-term cancellations that have taken
    # effect on the synthetic calendar.
    doc = _advance_subscription(doc)
    return respond(200, _subscription_shape(doc))

def on_create_subscription(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)
    account_key = body.get("accountKey", body.get("accountId", ""))
    if account_key == None:
        account_key = ""

    # Look up the account.
    account = _find_account(account_key)
    if account == None:
        return _zuora_err(400, "50000001", "Account not found: " + account_key)

    # Validate the rate plans against the catalog.
    plans = body.get("subscribeToRatePlans", [])
    if plans == None:
        plans = []
    if len(plans) == 0:
        return _zuora_err(400, "50000040", "subscribeToRatePlans is required and must not be empty")

    sub_plans = []
    for i in range(len(plans)):
        p = plans[i]
        if p == None:
            continue
        prp_id = p.get("productRatePlanId", "")
        cat = None
        ov = p.get("chargeOverrides", None)
        has_override = ov != None and len(ov) > 0
        if prp_id != "":
            cat = _catalog_plan(prp_id)
            if cat == None and not has_override:
                return _zuora_err(400, "51000010", "ProductRatePlan not found: " + prp_id)
        charges = _plan_charges(p, cat)
        name = cat["productRatePlanName"] if cat != None else p.get("productRatePlanName", "Rate Plan")
        sub_plans.append({
            "id": "plan-" + str(i + 1),
            "productRatePlanId": prp_id,
            "productRatePlanName": name,
            "charges": charges,
        })

    # Term: TERMED (default 12 months, or an explicit termEndDate) vs EVERGREEN.
    term_type = body.get("termType", "TERMED")
    if term_type == None:
        term_type = "TERMED"
    initial_term = body.get("initialTerm", 12)
    if initial_term == None:
        initial_term = 12
    itn = _zuora_try_num(initial_term)
    if itn == None or itn <= 0:
        itn = 12
    itn = int(itn)

    # Default contractEffectiveDate is today on the simulator's synthetic
    # calendar (clock-anchored), not a fixed date.
    ced = body.get("contractEffectiveDate", _today())
    if ced == None or ced == "":
        ced = _today()

    end_date = body.get("termEndDate", None)
    if end_date == None or end_date == "":
        if term_type == "EVERGREEN":
            end_date = None
        else:
            end_date = _add_days(ced, itn * 30)

    sub_id = _next_id("subscription")
    sub_number = "SUB-SYNTH-" + str(_to_int(sub_id) - 9 * 10000 + 100)

    doc = {
        "id": sub_id,
        "subscriptionId": sub_id,
        "subscriptionNumber": sub_number,
        "accountNumber": account.get("accountNumber", ""),
        "accountId": account.get("accountId", ""),
        "status": "Active",
        "termType": term_type,
        "initialTerm": itn,
        "contractEffectiveDate": ced,
        "startDate": ced,
        "endDate": end_date,
        "subscriptionPlans": sub_plans,
        "customFields": {},
        "cancellationRequested": False,
    }

    col = store_collection("subscriptions")
    col.insert(doc)

    # Generate the first invoice from the rate plan charges and post it to
    # the account balance.
    invoice = _create_invoice_for_subscription(doc, account)

    # Webhook callouts: SubscriptionActivated + InvoicePosted.
    _emit_if_subscribed("SubscriptionActivated", _callout(
        "Subscription",
        "SubscriptionActivated",
        "Subscription",
        sub_id,
        {
            "SubscriptionNumber": sub_number,
            "AccountNumber": doc.get("accountNumber", ""),
            "Status": "Active",
            "TermType": term_type,
        },
    ))
    _emit_if_subscribed("InvoicePosted", _callout(
        "Invoice",
        "InvoicePosted",
        "Invoice",
        invoice.get("invoiceId", ""),
        {
            "InvoiceNumber": invoice.get("invoiceNumber", ""),
            "AccountNumber": doc.get("accountNumber", ""),
            "Amount": invoice.get("amount", 0),
            "Balance": invoice.get("balance", 0),
            "Status": "Posted",
        },
    ))

    return respond(200, {
        "success": True,
        "subscriptionId": sub_id,
        "subscriptionNumber": sub_number,
        "accountNumber": account.get("accountNumber", ""),
        "status": "Active",
        "termType": term_type,
        "initialTerm": itn,
        "contractEffectiveDate": ced,
        "endDate": end_date,
        "invoiceId": invoice.get("invoiceId", ""),
        "invoiceNumber": invoice.get("invoiceNumber", ""),
        "invoiceAmount": invoice.get("amount", 0),
    })

def on_update_subscription(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    sub_key = req["params"].get("subscriptionKey", "")
    doc = _find_subscription(sub_key)
    if doc == None:
        return _zuora_err(404, "50000000", "Subscription not found")

    doc = _advance_subscription(doc)
    if doc.get("status", "") != "Active":
        return _zuora_err(400, "50000050", "Subscription is not active: " + doc.get("status", ""))

    body = _get_body(req)

    # --- add/remove rate plans (real amendment semantics) ---
    added = []
    add_plans = body.get("addRatePlans", [])
    if add_plans == None:
        add_plans = []
    existing = doc.get("subscriptionPlans", [])
    if existing == None:
        existing = []
    next_idx = len(existing) + 1

    for i in range(len(add_plans)):
        p = add_plans[i]
        if p == None:
            continue
        prp_id = p.get("productRatePlanId", "")
        cat = None
        ov = p.get("chargeOverrides", None)
        has_override = ov != None and len(ov) > 0
        if prp_id != "":
            cat = _catalog_plan(prp_id)
            if cat == None and not has_override:
                return _zuora_err(400, "51000010", "ProductRatePlan not found: " + prp_id)
        charges = _plan_charges(p, cat)
        name = cat["productRatePlanName"] if cat != None else p.get("productRatePlanName", "Rate Plan")
        plan_doc = {
            "id": "plan-" + str(next_idx),
            "productRatePlanId": prp_id,
            "productRatePlanName": name,
            "charges": charges,
        }
        existing.append(plan_doc)
        added.append(plan_doc["id"])
        next_idx = next_idx + 1

    removed = []
    remove_plans = body.get("removeRatePlans", [])
    if remove_plans == None:
        remove_plans = []
    if len(remove_plans) > 0:
        kept = []
        for i in range(len(existing)):
            plan = existing[i]
            drop = False
            for j in range(len(remove_plans)):
                r = remove_plans[j]
                if r == plan.get("id", "") or r == plan.get("productRatePlanId", "") or r == plan.get("productRatePlanName", ""):
                    drop = True
                    break
            if drop:
                removed.append(plan.get("id", ""))
            else:
                kept.append(plan)
        existing = kept

    doc["subscriptionPlans"] = existing

    # --- term changes ---
    if "termType" in body:
        doc["termType"] = body.get("termType")
    if "initialTerm" in body:
        itn = _zuora_try_num(body.get("initialTerm"))
        if itn != None and itn > 0:
            doc["initialTerm"] = int(itn)
    if "termEndDate" in body and body.get("termEndDate") != None and body.get("termEndDate") != "":
        doc["endDate"] = body.get("termEndDate")
    elif "initialTerm" in body and doc.get("termType", "TERMED") == "TERMED":
        itn = _zuora_try_num(body.get("initialTerm"))
        if itn != None and itn > 0:
            doc["endDate"] = _add_days(doc.get("contractEffectiveDate", _today()), int(itn) * 30)
    if "contractEffectiveDate" in body and body.get("contractEffectiveDate") != None:
        doc["contractEffectiveDate"] = body.get("contractEffectiveDate")

    # --- everything else: deep-merge into customFields ---
    consumed = ["addRatePlans", "removeRatePlans", "termType", "initialTerm", "termEndDate", "contractEffectiveDate", "renewalSetting", "notes", "status", "customFields"]
    custom = doc.get("customFields", {})
    if custom == None:
        custom = {}
    if "customFields" in body:
        cf = body.get("customFields", {})
        if cf != None:
            _deep_merge(custom, cf)
    extras = {}
    for k in body:
        if k in consumed:
            continue
        extras[k] = body[k]
    if "notes" in body:
        extras["notes"] = body.get("notes")
    if "renewalSetting" in body:
        extras["renewalSetting"] = body.get("renewalSetting")
    if len(extras) > 0:
        _deep_merge(custom, extras)
    doc["customFields"] = custom

    col = store_collection("subscriptions")
    col.update(doc.get("id", ""), doc)

    # Webhook callout: SubscriptionUpdated.
    _emit_if_subscribed("SubscriptionUpdated", _callout(
        "Subscription",
        "SubscriptionUpdated",
        "Subscription",
        doc.get("subscriptionId", ""),
        {
            "SubscriptionNumber": doc.get("subscriptionNumber", ""),
            "AccountNumber": doc.get("accountNumber", ""),
            "Status": doc.get("status", "Active"),
            "AddedRatePlans": added,
            "RemovedRatePlans": removed,
        },
    ))

    return respond(200, {
        "success": True,
        "subscriptionId": doc.get("subscriptionId", ""),
        "subscriptionNumber": doc.get("subscriptionNumber", ""),
        "status": doc.get("status", "Active"),
        "addedRatePlans": added,
        "removedRatePlans": removed,
        "subscriptionPlans": doc.get("subscriptionPlans", []),
    })

def on_cancel_subscription(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    sub_key = req["params"].get("subscriptionKey", "")
    doc = _find_subscription(sub_key)
    if doc == None:
        return _zuora_err(404, "50000000", "Subscription not found")

    doc = _advance_subscription(doc)
    if doc.get("status", "") != "Active":
        return _zuora_err(400, "50000050", "Subscription is not active: " + doc.get("status", ""))

    body = _get_body(req)

    # Normalize the policy: EndOfTerm (Zuora default) | Immediate.
    policy = body.get("cancellationPolicy", body.get("cancellation_policy", "EndOfTerm"))
    if policy == None or policy == "":
        policy = "EndOfTerm"
    pol = _lower(str(policy))
    if pol == "endofterm" or pol == "end_of_term":
        policy = "EndOfTerm"
    elif pol == "immediate":
        policy = "Immediate"
    elif pol == "specificdate":
        policy = "SpecificDate"
    else:
        return _zuora_err(400, "50000051", "Invalid cancellationPolicy: " + str(policy))

    today = _today()

    if policy == "EndOfTerm" or policy == "SpecificDate":
        # EndOfTerm: stays Active until the term ends (the flip to Canceled is
        # derived on read by _advance_subscription). EVERGREEN subscriptions
        # have no term end, so Zuora rejects the policy.
        if policy == "EndOfTerm" and doc.get("termType", "TERMED") == "EVERGREEN":
            return _zuora_err(400, "50000052", "cancellationPolicy EndOfTerm is not supported for EVERGREEN subscriptions")
        eff = doc.get("endDate", None)
        if policy == "SpecificDate":
            eff = body.get("specificCancellationDate", body.get("cancellationEffectiveDate", None))
            if eff == None or eff == "":
                return _zuora_err(400, "50000053", "specificCancellationDate is required for cancellationPolicy SpecificDate")
        if eff == None or eff == "":
            eff = today
        doc["cancellationRequested"] = True
        doc["cancellationPolicy"] = policy
        doc["cancellationEffectiveDate"] = eff
        doc["cancelRequestedAt"] = today
    else:
        # Immediate: cancel now and, for TERMED subscriptions with term
        # remaining, issue a prorated credit memo for the uninvoiced remainder.
        doc["status"] = "Canceled"
        doc["cancellationRequested"] = False
        doc["cancellationPolicy"] = "Immediate"
        doc["cancellationEffectiveDate"] = today
        doc["cancelledAt"] = today

    col = store_collection("subscriptions")
    col.update(doc.get("id", ""), doc)

    credit_number = ""
    credit_amount = 0.0
    if doc.get("cancellationPolicy", "") == "Immediate":
        credit = _cancellation_credit(doc, today)
        if credit != None:
            credit_number = credit.get("creditMemoNumber", "")
            # Report the credit as a positive amount (the memo doc itself is
            # negative-amount).
            credit_amount = -credit.get("amount", 0.0)

    # Webhook callout: SubscriptionCancelled fires immediately for Immediate;
    # for EndOfTerm it fires when _advance_subscription flips the status.
    if doc.get("status", "") == "Canceled":
        _emit_if_subscribed("SubscriptionCancelled", _callout(
            "Subscription",
            "SubscriptionCancelled",
            "Subscription",
            doc.get("subscriptionId", ""),
            {
                "SubscriptionNumber": doc.get("subscriptionNumber", ""),
                "AccountNumber": doc.get("accountNumber", ""),
                "Status": "Canceled",
                "CancellationPolicy": "Immediate",
                "CancellationEffectiveDate": today,
            },
        ))

    return respond(200, {
        "success": True,
        "subscriptionId": doc.get("subscriptionId", ""),
        "subscriptionNumber": doc.get("subscriptionNumber", ""),
        "status": doc.get("status", "Active"),
        "cancellationPolicy": doc.get("cancellationPolicy", ""),
        "cancellationEffectiveDate": doc.get("cancellationEffectiveDate", ""),
        "creditMemoNumber": credit_number,
        "creditMemoAmount": credit_amount,
    })

# _cancellation_credit issues a prorated credit memo for the uninvoiced
# remainder of a TERMED subscription cancelled mid-term (Immediate policy).
# The credit is the charge total scaled by remaining term days / total term
# days. Returns the credit-memo invoice doc or None when nothing is owed.
def _cancellation_credit(doc, today):
    if doc.get("termType", "TERMED") != "TERMED":
        return None
    end = doc.get("endDate", None)
    if end == None or end == "":
        return None
    end_days = _date_to_days(end)
    today_days = _date_to_days(today)
    if end_days <= today_days:
        return None
    start_days = _date_to_days(doc.get("startDate", doc.get("contractEffectiveDate", _today())))
    total_days = end_days - start_days
    if total_days <= 0:
        return None

    charges = []
    total = 0.0
    plans = doc.get("subscriptionPlans", [])
    if plans == None:
        plans = []
    for i in range(len(plans)):
        plan_charges = plans[i].get("charges", [])
        if plan_charges == None:
            continue
        for j in range(len(plan_charges)):
            ch = plan_charges[j]
            amt = _round2(ch.get("price", 0) * ch.get("quantity", 0) * (end_days - today_days) / total_days)
            if amt <= 0:
                continue
            charges.append({
                "chargeName": ch.get("chargeName", ""),
                "quantity": ch.get("quantity", 0),
                "uom": ch.get("uom", "Each"),
                "chargeAmount": -amt,
                "taxAmount": 0,
                "amountWithoutTax": -amt,
                "productRatePlanId": plans[i].get("productRatePlanId", ""),
            })
            total = total + amt
    if total <= 0:
        return None

    account = _find_account(doc.get("accountNumber", ""))
    if account == None:
        account = _find_account(doc.get("accountId", ""))
    currency = "USD"
    if account != None:
        currency = account.get("currency", "USD")

    # Credit memos live in the invoices collection (negative-amount docs), so
    # they draw from the invoice id/number sequence with a CM- prefix.
    credit_id = _next_id("invoice")
    credit_seq = str(_to_int(credit_id) - 9 * 10000 + 100)
    credit = {
        "id": credit_id,
        "invoiceId": credit_id,
        "invoiceNumber": "CM-SYNTH-" + credit_seq,
        "creditMemoNumber": "CM-SYNTH-" + credit_seq,
        "accountId": doc.get("accountId", ""),
        "accountNumber": doc.get("accountNumber", ""),
        "subscriptionId": doc.get("subscriptionId", ""),
        "subscriptionNumber": doc.get("subscriptionNumber", ""),
        "amount": -_round2(total),
        "amountWithoutTax": -_round2(total),
        "taxAmount": 0,
        "balance": -_round2(total),
        "status": "Posted",
        "type": "CreditMemo",
        "invoiceDate": today,
        "dueDate": today,
        "currency": currency,
        "invoiceItems": charges,
        "appliedPayments": [],
    }
    ic = store_collection("invoices")
    ic.insert(credit)

    if account != None:
        ac = store_collection("accounts")
        account["balance"] = _round2(account.get("balance", 0) - total)
        ac.update(account.get("id", ""), account)

    _emit_if_subscribed("CreditMemoPosted", _callout(
        "CreditMemo",
        "CreditMemoPosted",
        "CreditMemo",
        credit_id,
        {
            "CreditMemoNumber": credit.get("creditMemoNumber", ""),
            "AccountNumber": doc.get("accountNumber", ""),
            "Amount": credit.get("amount", 0),
            "Status": "Posted",
        },
    ))
    return credit
