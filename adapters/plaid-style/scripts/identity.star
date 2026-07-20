# Identity handler — returns owner info for accounts.
#
# POST /identity/get  { access_token }
#   -> { accounts: [{ account_id, owners: [{ names, emails, phone_numbers, addresses }] }], request_id }

# Shared helpers (_check_auth, _request_id, _resolve_item_id) from lib.star.

def on_get_identity(req):
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

    ac = store_collection("accounts")
    all_accts = ac.list()
    accounts = []
    for a in all_accts:
        if a.get("item_id", "") != item_id:
            continue
        accounts.append({
            "account_id": a["id"],
            "owners": [{
                "names": ["Alberta Bobbeth Charles"],
                "phone_numbers": ["+1 (123) 456-7890"],
                "emails": ["accountholder0@example.com"],
                "addresses": [{
                    "data": {
                        "city": "Malakoff",
                        "state": "TX",
                        "street": "2992 Cameron Road",
                        "postal_code": "75148",
                        "country": "US",
                    },
                    "primary": True,
                }],
            }],
        })

    return respond(200, {
        "accounts": accounts,
        "request_id": _request_id(),
    })
