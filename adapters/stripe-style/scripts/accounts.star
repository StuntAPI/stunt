# Connected accounts handlers — Stripe Connect.
#
# Manages Custom/Express/Standard connected accounts stored in the
# connect_accounts collection. Emits account.updated on create and update.
# Shared helpers (_require_auth, _next_id, _not_found) are in lib.star.

# POST /v1/accounts — create a connected account.
def on_create_account(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    acct_id = _next_id("acct")
    acct_type = body.get("type", "express")
    country = body.get("country", "US")
    email = body.get("email", None)
    business_type = body.get("business_type", None)
    capabilities = body.get("capabilities", {})

    doc = {
        "id": acct_id,
        "object": "account",
        "type": acct_type,
        "country": country,
        "email": email,
        "business_type": business_type,
        "capabilities": capabilities,
        "details_submitted": False,
        "charges_enabled": False,
        "payouts_enabled": False,
        "requirements": {"currently_due": [], "eventually_due": [], "past_due": [], "disabled_reason": None},
        "created": 1700000000,
    }

    c = store_collection("connect_accounts")
    c.insert(doc)

    # Emit webhook event (fire-and-forget: errors do not break account creation).
    events_emit("account.updated", doc)

    return respond(201, doc)

# GET /v1/accounts/{id} — retrieve a single connected account.
def on_retrieve_account(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("connect_accounts")
    doc = c.get(id)
    if doc == None:
        return _not_found("account", id)
    return respond(200, doc)

# POST /v1/accounts/{id} — update a connected account (e.g. capabilities).
def on_update_account(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("connect_accounts")
    doc = c.get(id)
    if doc == None:
        return _not_found("account", id)

    body = req["body"]
    if body != None:
        for k in body:
            doc[k] = body[k]

        # If capabilities were updated, derive enablement flags.
        caps = body.get("capabilities")
        if caps != None:
            if caps.get("card_payments") == "active":
                doc["charges_enabled"] = True
            if caps.get("transfers") == "active":
                doc["payouts_enabled"] = True

    c.update(id, doc)

    # Emit webhook event (fire-and-forget).
    events_emit("account.updated", doc)

    return respond(200, doc)

# GET /v1/accounts — list all connected accounts.
def on_list_accounts(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("connect_accounts")
    docs = c.list()
    return respond(200, {"object": "list", "data": docs, "has_more": False, "url": "/v1/accounts"})
