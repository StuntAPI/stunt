# Item handlers — public_token exchange, item get, item remove.
#
# POST /item/public_token/exchange  { public_token } -> { access_token, item_id, request_id }
# POST /item/get                    { access_token } -> { item: {...}, request_id }
# POST /item/remove                 { access_token } -> { removed: true, request_id }

# Shared helpers (_check_auth, _request_id) from lib.star.

# on_exchange_public_token exchanges a Link public_token for an access_token.
# STATEFUL: creates an item binding if the public_token maps to the seed item.
def on_exchange_public_token(req):
    err = _check_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    public_token = body.get("public_token") or ""
    if public_token == "":
        return respond(400, {
            "display_message": None,
            "error_type": "INVALID_REQUEST",
            "error_code": "MISSING_PUBLIC_TOKEN",
            "error_message": "public_token is required",
            "request_id": _request_id(),
        })

    pc = store_collection("public_tokens")
    pt_doc = pc.get(public_token)
    if pt_doc == None:
        return respond(400, {
            "display_message": None,
            "error_type": "INVALID_INPUT",
            "error_code": "INVALID_PUBLIC_TOKEN",
            "error_message": "public_token does not exist",
            "request_id": _request_id(),
        })

    # Link-session binding: when the client passes the link_token that
    # started the Link flow, the public_token must belong to that session.
    # Mismatched pairs are rejected the way Plaid rejects cross-session
    # artifacts (INVALID_PRODUCT / INVALID_FIELD).
    link_token = body.get("link_token") or ""
    if link_token != "":
        if pt_doc.get("link_token", "") != link_token:
            return respond(400, {
                "display_message": None,
                "error_type": "INVALID_PRODUCT",
                "error_code": "INVALID_FIELD",
                "error_message": "public_token was not issued for this link_token",
                "request_id": _request_id(),
            })

    item_id = pt_doc.get("item_id", "item-seed-a")

    # Mint an access_token bound to this item.
    n = store_kv_incr("plaid", "access_seq")
    access_token = "access-sandbox-" + str(n)

    ac = store_collection("access_tokens")
    ac.insert({
        "id": access_token,
        "item_id": item_id,
    })

    # Sync the items collection if needed.
    ic = store_collection("items")
    item_doc = ic.get(item_id)
    if item_doc == None:
        ic.insert({
            "id": item_id,
            "item_id": item_id,
            "cursor_initial": "",
            "status": "good",
        })

    return respond(200, {
        "access_token": access_token,
        "item_id": item_id,
        "request_id": _request_id(),
    })

# on_get_item returns the item object for a given access_token.
def on_get_item(req):
    err = _check_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    access_token = body.get("access_token", "")
    item_id = _resolve_item_id(access_token)
    if item_id == "":
        return respond(400, {
            "display_message": None,
            "error_type": "INVALID_INPUT",
            "error_code": "INVALID_ACCESS_TOKEN",
            "error_message": "access_token does not exist",
            "request_id": _request_id(),
        })

    # Institution comes from the item record (sandbox-created items carry
    # the institution chosen in /sandbox/public_token/create).
    institution_id = "ins_1"
    ic = store_collection("items")
    item_doc = ic.get(item_id)
    if item_doc != None:
        institution_id = item_doc.get("institution_id", institution_id)

    return respond(200, {
        "item": {
            "item_id": item_id,
            "institution_id": institution_id,
            "webhook": "",
            "products": ["transactions", "identity"],
            "error": None,
            "available_products": ["balance", "assets"],
            "billed_products": ["transactions", "identity"],
            "consent_expiration_time": None,
            "update_type": "background",
        },
        "status": {
            "transactions": {
                "last_successful_update": "2024-01-06T12:00:00Z",
                "last_failed_update": None,
            },
        },
        "request_id": _request_id(),
    })

# on_remove_item removes the item associated with the access_token.
# Matches Plaid's actual behavior: an unknown access_token is rejected with
# INVALID_ACCESS_TOKEN, but a valid token removes (and reports removed:true
# for) the item even if its data was already gone — Plaid returns
# removed:true for any valid access_token.
def on_remove_item(req):
    err = _check_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    access_token = body.get("access_token", "")
    item_id = _resolve_item_id(access_token)
    if item_id == "":
        return respond(400, {
            "display_message": None,
            "error_type": "INVALID_INPUT",
            "error_code": "INVALID_ACCESS_TOKEN",
            "error_message": "access_token does not exist",
            "request_id": _request_id(),
        })

    # Purge the item and everything hanging off it: accounts, transactions,
    # and the access_token binding itself (a removed item's token is dead in
    # real Plaid).
    ic = store_collection("items")
    ic.delete(item_id)

    account_ids = []
    ac = store_collection("accounts")
    for a in ac.list():
        if a.get("item_id", "") == item_id:
            account_ids.append(a["id"])
            ac.delete(a["id"])

    tc = store_collection("transactions")
    for t in tc.list():
        if t.get("account_id", "") in account_ids:
            tc.delete(t["id"])

    atc = store_collection("access_tokens")
    for at in atc.list():
        if at.get("item_id", "") == item_id:
            atc.delete(at["id"])

    return respond(200, {
        "removed": True,
        "request_id": _request_id(),
    })

# (Shared helpers _check_auth, _request_id, _resolve_item_id from lib.star.)
