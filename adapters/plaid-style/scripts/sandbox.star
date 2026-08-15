# Sandbox handlers — Plaid's local-testing surface.
#
# POST /sandbox/public_token/create
#   { institution_id, initial_products, link_token? }
#   -> { public_token, expiration, request_id }
# POST /sandbox/item/reset_login
#   { access_token }
#   -> { reset_login: true, request_id }
# POST /sandbox/item/fire_webhook
#   { access_token, webhook_code }
#   -> { webhook_fired: true, request_id }
#
# /sandbox/public_token/create mints a fresh item for the chosen institution
# (with accounts and transactions) plus a public_token bound to it — and, when
# a link_token is passed, to that Link session. This is how one Link session
# yields multiple items.
#
# /sandbox/item/fire_webhook with webhook_code SYNC_UPDATES_AVAILABLE is the
# simulate trigger: it mutates the item's configured transactions (one flips
# to modified — pending posted — one is removed) so the next
# /transactions/sync returns them in `modified`/`removed`.

# Shared helpers (_check_auth, _request_id, _resolve_item_id) from lib.star.

def on_create_public_token(req):
    err = _check_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    institution_id = body.get("institution_id", "")
    ic = store_collection("institutions")
    inst = ic.get(institution_id)
    if institution_id == "" or inst == None:
        return respond(400, {
            "display_message": None,
            "error_type": "INVALID_INPUT",
            "error_code": "INVALID_INSTITUTION",
            "error_message": "invalid institution_id",
            "request_id": _request_id(),
        })

    products = body.get("initial_products", ["transactions"])
    if products == None:
        products = ["transactions"]

    # When a link_token is supplied it must be a live Link session; the minted
    # public_token is then bound to that session for exchange verification.
    link_token = body.get("link_token", "")
    if link_token != "":
        lc = store_collection("link_tokens")
        if lc.get(link_token) == None:
            return respond(400, {
                "display_message": None,
                "error_type": "INVALID_INPUT",
                "error_code": "INVALID_FIELD",
                "error_message": "link_token does not exist",
                "request_id": _request_id(),
            })

    # Materialize a new item for the institution: 2 accounts, 3 transactions.
    n = store_kv_incr("plaid", "item_seq")
    item_id = "item-sb-" + str(n)

    items = store_collection("items")
    items.insert({
        "id": item_id,
        "item_id": item_id,
        "institution_id": institution_id,
        "products": products,
        "cursor_initial": "",
        "status": "good",
    })

    acct_a = "acc-sb-" + str(n) + "-a"
    acct_b = "acc-sb-" + str(n) + "-b"
    ac = store_collection("accounts")
    ac.insert({
        "id": acct_a,
        "item_id": item_id,
        "balances": {"available": 210.0, "current": 240.0, "iso_currency_code": "USD"},
        "name": inst.get("name", "Sandbox Bank") + " Checking",
        "mask": "2222",
        "subtype": "checking",
        "type": "depository",
    })
    ac.insert({
        "id": acct_b,
        "item_id": item_id,
        "balances": {"available": 800.0, "current": 800.0, "iso_currency_code": "USD"},
        "name": inst.get("name", "Sandbox Bank") + " Savings",
        "mask": "3333",
        "subtype": "savings",
        "type": "depository",
    })

    tc = store_collection("transactions")
    tc.insert({
        "id": "tx-sb-" + str(n) + "-a",
        "account_id": acct_a,
        "amount": -6.25,
        "date": "2026-01-02",
        "name": "Sandbox Gyro Stand",
        "category": ["Food and Drink", "Restaurants"],
        "pending": False,
        "seq": 1,
        "state": "new",
    })
    tc.insert({
        "id": "tx-sb-" + str(n) + "-b",
        "account_id": acct_a,
        "amount": 90.0,
        "date": "2026-01-03",
        "name": "Sandbox Payroll",
        "category": ["Transfer", "Deposit"],
        "pending": False,
        "seq": 2,
        "state": "new",
    })
    tc.insert({
        "id": "tx-sb-" + str(n) + "-c",
        "account_id": acct_b,
        "amount": -120.0,
        "date": "2026-01-04",
        "name": "Sandbox Transfer Out",
        "category": ["Transfer", "Withdrawal"],
        "pending": True,
        "seq": 3,
        "state": "new",
    })

    public = _new_public_token(link_token, item_id)
    return respond(200, {
        "public_token": public,
        "expiration": clock.now_rfc3339(),
        "request_id": _request_id(),
    })

def on_reset_login(req):
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

    # Real behavior: forces the item into ITEM_LOGIN_REQUIRED so the next
    # Link flow re-authenticates it.
    ic = store_collection("items")
    doc = ic.get(item_id)
    if doc != None:
        d = _copy_doc(doc)
        d["status"] = "item_login_required"
        ic.update(item_id, d)
        events_emit("ERROR", {
            "webhook_type": "ITEM",
            "webhook_code": "ERROR",
            "item_id": item_id,
            "error": {
                "error_type": "ITEM_ERROR",
                "error_code": "ITEM_LOGIN_REQUIRED",
            },
        })

    return respond(200, {
        "reset_login": True,
        "request_id": _request_id(),
    })

def on_fire_webhook(req):
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

    code = body.get("webhook_code", "DEFAULT_WEBHOOK")
    if code == None or code == "":
        code = "DEFAULT_WEBHOOK"

    if code == "SYNC_UPDATES_AVAILABLE":
        _simulate_tx_mutations(item_id)
        events_emit(code, {
            "webhook_type": "TRANSACTIONS",
            "webhook_code": code,
            "item_id": item_id,
            "initial_update_complete": True,
            "historical_update_complete": True,
        })
    else:
        events_emit(code, {
            "webhook_type": "ITEM",
            "webhook_code": code,
            "item_id": item_id,
        })

    return respond(200, {
        "webhook_fired": True,
        "request_id": _request_id(),
    })

# _simulate_tx_mutations applies the post-initial-pull mutations: the oldest
# live transaction is posted (pending -> cleared, surfaces in sync
# `modified`) and the newest live transaction is tombstoned (surfaces in
# sync `removed`). Both get fresh seq values above the item's watermark.
def _simulate_tx_mutations(item_id):
    tc = store_collection("transactions")
    live = []
    for t in tc.list():
        if t.get("state", "new") == "removed":
            continue
        if not _tx_belongs_to_item(t, item_id):
            continue
        live.append(t)
    live = query_select(live, None, "seq", "asc", None, None, None)
    if len(live) == 0:
        return

    max_seq = _max_tx_seq(item_id)

    mod = _copy_doc(live[0])
    mod["state"] = "modified"
    mod["pending"] = False
    mod["seq"] = max_seq + 1
    tc.update(mod["id"], mod)

    if len(live) > 1:
        rem = _copy_doc(live[len(live) - 1])
        rem["state"] = "removed"
        rem["seq"] = max_seq + 2
        tc.update(rem["id"], rem)

# _new_public_token mints and stores a public_token bound to an item (and,
# when given, the Link session that owns it).
def _new_public_token(link_token, item_id):
    n = store_kv_incr("plaid", "link_seq")
    public = "public-sandbox-" + str(n)
    pc = store_collection("public_tokens")
    pc.insert({
        "id": public,
        "item_id": item_id,
        "link_token": link_token,
    })
    return public

# _max_tx_seq returns the highest seq among the item's transactions.
def _max_tx_seq(item_id):
    tc = store_collection("transactions")
    mx = 0
    for t in tc.list():
        if _tx_belongs_to_item(t, item_id):
            if t.get("seq", 0) > mx:
                # Coerce: JSON numbers may arrive as floats.
                mx = int(t.get("seq", 0))
    return mx

# _tx_belongs_to_item: a transaction belongs to the item its account belongs
# to.
def _tx_belongs_to_item(t, item_id):
    ac = store_collection("accounts")
    a = ac.get(t.get("account_id", ""))
    if a == None:
        return False
    return a.get("item_id", "") == item_id

# _copy_doc duplicates a stored document (dict.update-style helpers are not
# used so update() fully replaces the doc with every original field kept).
def _copy_doc(doc):
    d = {}
    for k in doc.keys():
        d[k] = doc[k]
    return d
