# Accounts handlers — balance/get and accounts/get.
#
# POST /accounts/balance/get  { access_token } -> { accounts: [...], request_id }
# POST /accounts/get           { access_token } -> { accounts: [...], item: {...}, request_id }

# Shared helpers (_check_auth, _request_id, _resolve_item_id, _item_accounts,
# _account_public) from lib.star.

# on_get_balances returns accounts with current balance data.
def on_get_balances(req):
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

    accounts = _item_accounts(item_id)

    return respond(200, {
        "accounts": accounts,
        "item": {
            "item_id": item_id,
            "institution_id": "ins_000000000000",
        },
        "request_id": _request_id(),
    })

# on_get_accounts returns the full account list.
def on_get_accounts(req):
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

    accounts = _item_accounts(item_id)

    return respond(200, {
        "accounts": accounts,
        "item": {
            "item_id": item_id,
            "institution_id": "ins_000000000000",
        },
        "request_id": _request_id(),
    })
