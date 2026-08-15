# Transaction handlers. All data synthetic.
#
# Lifecycle modelled (real 2017-09-01 shapes): create (initiating party
# auto-agrees) -> each party agrees -> buyer funds on the hosted page (the
# /sim/.../fund affordance) -> schedule entries secure -> ship/receive ->
# accept closes the transaction. Funding state lives ONLY on
# items[].schedule[].status.secured; item lifecycle on items[].status.

def on_customer_me(req):
    err = _require_basic(req)
    if err != None:
        return err
    return respond(200, {
        "id": 1,
        "email": "me@sim.invalid",
        "first_name": "Sandbox",
        "last_name": "Customer",
    })

def on_transaction_create(req):
    err = _require_basic(req)
    if err != None:
        return err
    body, ok = body_of(req)
    if not ok:
        return bad_body()
    parties = body.get("parties", [])
    if parties == None:
        parties = []
    if len(parties) < 2:
        return respond(400, {"errors": {"parties": ["Transaction must have a buyer and a seller"]}})
    if party_by_role(body, "seller") == None:
        return respond(400, {"errors": {"parties": {"0": ["Transaction must have 1 seller"]}}})
    if party_by_role(body, "buyer") == None:
        return respond(400, {"errors": {"parties": {"0": ["Transaction must have 1 buyer"]}}})

    id = next_id("transactions")
    stored_parties = []
    for i in range(len(parties)):
        p = parties[i]
        # The first party in the list is the initiating party; the real API
        # auto-agrees the authenticated creator (documented convention).
        agreed = i == 0
        stored_parties.append({
            "id": next_id("parties"),
            "role": p.get("role"),
            "customer": p.get("customer"),
            "agreed": agreed,
        })

    buyer = party_by_role(body, "buyer")
    stored_items = []
    for item in body.get("items", []):
        schedule = []
        for s in item.get("schedule", []):
            schedule.append({
                "amount": _to_cents(s.get("amount", 0)) or 0,
                "payer_customer": s.get("payer_customer"),
                "beneficiary_customer": s.get("beneficiary_customer"),
                "type": s.get("type", "deposit"),
                "status": {"secured": False},
            })
        amount = 0
        for s in schedule:
            amount = amount + s["amount"]
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
                "amount": amount * ESCROW_FEE_RATE // 10000,
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
        "creation_date": clock.now_rfc3339(),
        "close_date": None,
        "is_cancelled": False,
        "parties": stored_parties,
        "items": stored_items,
    }
    store_collection("transactions").insert(tx)
    _emit("transaction_created", tx)
    return respond(201, present(tx))

def on_transaction_list(req):
    err = _require_basic(req)
    if err != None:
        return err
    docs = [present(t) for t in store_collection("transactions").list()]
    page = _to_int(req.get("query", {}).get("page", "1"))
    if page < 1:
        page = 1
    per = _to_int(req.get("query", {}).get("per_page", "10"))
    if per < 1:
        per = 10
    items, _next = paginate(docs, per, str((page - 1) * per))
    return respond(200, {"transactions": items})

def _find(id_str):
    for tx in store_collection("transactions").list():
        if str(tx.get("ref_id")) == str(id_str):
            return tx
    return None

def on_transaction_get(req):
    err = _require_basic(req)
    if err != None:
        return err
    tx = _find(req.get("params", {}).get("id", ""))
    if tx == None:
        return respond(404, {"errors": {"id": ["Transaction not found"]}})
    return respond(200, present(tx))

def on_transaction_by_reference(req):
    err = _require_basic(req)
    if err != None:
        return err
    ref = req.get("params", {}).get("reference", "")
    for tx in store_collection("transactions").list():
        if tx.get("reference") == ref:
            return respond(200, present(tx))
    return respond(404, {"errors": {"reference": ["Transaction not found"]}})

def on_transaction_action(req):
    err = _require_basic(req)
    if err != None:
        return err
    body, ok = body_of(req)
    if not ok:
        return bad_body()
    action = body.get("action", "")
    id_str = req.get("params", {}).get("id", "")
    txs = store_collection("transactions")
    tx = _find(id_str)
    if tx == None:
        return respond(404, {"errors": {"id": ["Transaction not found"]}})

    # The sim identifies the acting party by the customer given in the body;
    # the real API infers it from the authenticated user.
    actor = body.get("customer")

    if action == "agree":
        if actor == None:
            return respond(400, {"errors": {"customer": ["Customer can't be blank"]}})
        matched = False
        for p in tx.get("parties", []):
            if p.get("customer") == actor:
                p["agreed"] = True
                matched = True
        if not matched:
            return respond(400, {"errors": {"customer": ["Customer is not a party on this transaction"]}})
    elif action == "accept":
        if not is_secured(tx):
            return respond(400, {"errors": {"transaction": ["Transaction is not secured"]}})
        for item in tx.get("items", []):
            item["status"]["accepted"] = True
        tx["close_date"] = clock.now_rfc3339()
    elif action == "ship":
        for item in tx.get("items", []):
            item["status"]["shipped"] = True
    elif action == "receive":
        for item in tx.get("items", []):
            item["status"]["received"] = True
    elif action == "cancel":
        for item in tx.get("items", []):
            item["status"]["canceled"] = True
        tx["is_cancelled"] = True
    else:
        return respond(400, {"errors": {"action": ["Unknown action: " + action]}})

    txs.update(tx.get("id"), tx)
    _emit("transaction_" + action + "ed" if action in ("ship", "receive") else "transaction_" + action + "d", tx)
    return respond(200, present(tx))

def on_sim_fund(req):
    """Stands in for the buyer wiring funds on escrow.com. Not a real endpoint."""
    err = _require_basic(req)
    if err != None:
        return err
    txs = store_collection("transactions")
    tx = _find(req.get("params", {}).get("id", ""))
    if tx == None:
        return respond(404, {"errors": {"id": ["Transaction not found"]}})
    if not all_agreed(tx):
        return respond(400, {"errors": {"parties": ["All parties must agree before funding"]}})
    for item in tx.get("items", []):
        for s in item.get("schedule", []):
            s["status"] = {"secured": True}
    txs.update(tx.get("id"), tx)
    _emit("transaction_secured", tx)
    return respond(200, present(tx))

# _emit delivers a lifecycle event to the registered sink. Escrow.com does
# not sign its deliveries, so these are unsigned-by-design (README).
def _emit(event_type, tx):
    if events_target() == None:
        return
    events_emit(event_type, present(tx))

# _to_int parses a decimal string; 0 on miss.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch < "0" or ch > "9":
            return 0
        n = n * 10 + (ord(ch) - ord("0"))
    return n
