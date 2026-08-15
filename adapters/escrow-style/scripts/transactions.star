# Transaction handlers. All data synthetic.
#
# Lifecycle modelled: created -> both parties agree -> buyer funds (secured)
# -> seller ships / buyer receives -> buyer accepts -> closed.

def on_customer_me(req):
    return respond(200, {
        "id": 1,
        "email": "me@sim.invalid",
        "first_name": "Sandbox",
        "last_name": "Customer",
        "customer_type": "business",
    })

def on_transaction_create(req):
    body = body_of(req)
    parties = body.get("parties", [])
    if len(parties) < 2:
        return respond(400, {"errors": ["Transaction must have a buyer and a seller"]})
    if party_by_role(body, "seller") == None:
        return respond(400, {"errors": ["Transaction must have 1 seller"]})
    if party_by_role(body, "buyer") == None:
        return respond(400, {"errors": ["Transaction must have 1 buyer"]})

    id = next_id("transactions")
    stored_parties = []
    for p in parties:
        stored_parties.append({
            "id": next_id("parties"),
            "role": p.get("role"),
            "customer": p.get("customer"),
            "agreed": False,
        })

    buyer = party_by_role(body, "buyer")
    stored_items = []
    for item in body.get("items", []):
        schedule = []
        for s in item.get("schedule", []):
            schedule.append({
                "amount": s.get("amount"),
                "payer_customer": s.get("payer_customer"),
                "beneficiary_customer": s.get("beneficiary_customer"),
                "type": s.get("type", "deposit"),
                "status": {"secured": False, "secured_at": None},
            })
        amount = 0.0
        for s in schedule:
            amount = amount + float(s.get("amount", 0))
        stored_items.append({
            "id": next_id("items"),
            "title": item.get("title"),
            "description": item.get("description"),
            "type": item.get("type", "general_merchandise"),
            "inspection_period": item.get("inspection_period", 259200),
            "quantity": item.get("quantity", 1),
            "schedule": schedule,
            # Honour a caller-supplied fee split (escrow.com lets you say who
            # pays); otherwise default the whole escrow fee to the buyer.
            "fees": item.get("fees", [{
                "type": "escrow",
                "amount": amount * ESCROW_FEE_RATE / 10000.0,
                "payer_customer": buyer.get("customer"),
            }]),
            "status": {
                "accepted": False,
                "received": False,
                "shipped": False,
                "rejected": False,
                "canceled": False,
                "in_dispute": False,
            },
        })

    tx = {
        # The store assigns its own document id on insert, so the escrow-facing
        # numeric id lives in ref_id and is presented as "id" in responses.
        # stored as a string: the store round-trips numbers through JSON as
        # floats, so str(2) would come back "2.0" and never match a path param.
        "ref_id": str(id),
        "reference": body.get("reference"),
        "currency": body.get("currency", "usd"),
        "description": body.get("description"),
        "parties": stored_parties,
        "items": stored_items,
        "status": {
            "the_bad_boys": False,
            "in_dispute": False,
            "claimed": False,
            "secured": False,
            "closed": False,
        },
    }
    store_collection("transactions").insert(tx)
    return respond(201, present(tx))

def _find(id_str):
    for tx in store_collection("transactions").list():
        if str(tx.get("ref_id")) == str(id_str):
            return tx
    return None

def on_transaction_get(req):
    tx = _find(req.get("params", {}).get("id", ""))
    if tx == None:
        return respond(404, {"errors": ["Transaction not found"]})
    return respond(200, present(tx))

def on_transaction_by_reference(req):
    ref = req.get("params", {}).get("reference", "")
    for tx in store_collection("transactions").list():
        if tx.get("reference") == ref:
            return respond(200, present(tx))
    return respond(404, {"errors": ["Transaction not found"]})

def on_transaction_action(req):
    body = body_of(req)
    action = body.get("action", "")
    id_str = req.get("params", {}).get("id", "")
    txs = store_collection("transactions")
    tx = _find(id_str)
    if tx == None:
        return respond(404, {"errors": ["Transaction not found"]})

    # The sim identifies the acting party by the customer given in the body;
    # the real API infers it from the authenticated user.
    actor = body.get("customer")

    if action == "agree":
        for p in tx.get("parties", []):
            if actor == None or p.get("customer") == actor:
                p["agreed"] = True
    elif action == "accept":
        if not is_secured(tx):
            return respond(400, {"errors": ["Transaction is not secured"]})
        for item in tx.get("items", []):
            item["status"]["accepted"] = True
        tx["status"]["closed"] = True
    elif action == "ship":
        for item in tx.get("items", []):
            item["status"]["shipped"] = True
    elif action == "receive":
        for item in tx.get("items", []):
            item["status"]["received"] = True
    elif action == "cancel":
        for item in tx.get("items", []):
            item["status"]["canceled"] = True
    else:
        return respond(400, {"errors": ["Unknown action: " + action]})

    txs.update(tx.get("id"), tx)
    return respond(200, present(tx))

def on_sim_fund(req):
    """Stands in for the buyer wiring funds on escrow.com. Not a real endpoint."""
    txs = store_collection("transactions")
    tx = _find(req.get("params", {}).get("id", ""))
    if tx == None:
        return respond(404, {"errors": ["Transaction not found"]})
    if not all_agreed(tx):
        return respond(400, {"errors": ["All parties must agree before funding"]})
    for item in tx.get("items", []):
        for s in item.get("schedule", []):
            s["status"] = {"secured": True, "secured_at": "2026-01-01T00:00:00Z"}
    tx["status"]["secured"] = True
    txs.update(tx.get("id"), tx)
    return respond(200, present(tx))
