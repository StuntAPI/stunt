# Account handlers — Zuora Billing accounts.
#
# GET  /v1/accounts          -> list/query accounts
# POST /v1/accounts          -> create account
# GET  /v1/accounts/{key}    -> get account by accountKey (id or number)
#
# Zuora envelope: {success:true, ...} on success
# Zuora error: {success:false, processId, reasons:[{code, message}]}

# Shared helpers from lib.star.

def _account_shape(doc):
    return {
        "accountId": doc.get("accountId", doc.get("id", "")),
        "accountNumber": doc.get("accountNumber", ""),
        "name": doc.get("name", ""),
        "currency": doc.get("currency", "USD"),
        "status": doc.get("status", "Active"),
        "balance": doc.get("balance", 0),
        "billTo": doc.get("billTo", {}),
        "soldTo": doc.get("soldTo", {}),
        "createdOn": doc.get("createdOn", _now()),
    }

def on_list_accounts(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("accounts")
    docs = col.list()

    accounts = []
    for d in docs:
        accounts.append(_account_shape(d))

    return respond(200, {
        "success": True,
        "accounts": accounts,
    })

def on_get_account(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    account_key = req["params"].get("accountKey", "")
    col = store_collection("accounts")
    docs = col.list()

    for d in docs:
        if d.get("accountId", "") == account_key or d.get("accountNumber", "") == account_key:
            return respond(200, _account_shape(d))

    return _zuora_err(404, "50000000", "Account not found")

def on_create_account(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)

    account_id = _next_id("account")
    account_number = "ACC-SYNTH-" + str(_to_int(account_id) - 90000 + 100).upper()

    doc = {
        "id": account_id,
        "accountId": account_id,
        "accountNumber": account_number,
        "name": body.get("name", "New Account"),
        "currency": body.get("currency", "USD"),
        "status": body.get("status", "Draft"),
        "balance": 0,
        "billTo": body.get("billTo", {}),
        "soldTo": body.get("soldTo", {}),
        "createdOn": _now(),
    }

    col = store_collection("accounts")
    col.insert(doc)

    return respond(200, {
        "success": True,
        "accountId": account_id,
        "accountNumber": account_number,
    })
