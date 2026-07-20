# Shared library for plaid-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# Plaid takes credentials EITHER in the request body (client_id + secret) OR
# as "Authorization: Bearer <client_id>_<secret>". We check presence only —
# the values are not validated against real Plaid credentials.

# _check_auth validates that Plaid credentials are present. Returns None if
# authorized, or an error-response dict if not.
def _check_auth(req):
    body = req.get("body")
    if body != None:
        cid = body.get("client_id", "")
        secret = body.get("secret", "")
        if cid != "" and secret != "":
            return None

    # Fall back to bearer "client_id_secret".
    auth = ""
    headers = req.get("headers")
    if headers != None:
        auth = headers.get("Authorization", "")
        if auth == None:
            auth = ""
    if auth.startswith("Bearer "):
        token = auth[7:]
        if "_" in token:
            return None

    return respond(401, {
        "display_message": None,
        "error_type": "INVALID_API_KEYS",
        "error_code": "INVALID_API_KEYS",
        "error_message": "missing or invalid API keys",
        "request_id": _request_id(),
    })

# _request_id returns a unique-looking request ID for every call.
def _request_id():
    n = store_kv_incr("plaid", "request_seq")
    return "req-" + str(n)

# _new_cursor generates a cursor string for transactions/sync pagination.
def _new_cursor(batch):
    return "cursor-" + str(batch)

# _seed_link generates a public_token that, when exchanged, will bind to the
# seed item+accounts. This models the Plaid Link onSuccess flow: the frontend
# receives a public_token from Link and exchanges it for an access_token.
def _seed_link():
    n = store_kv_incr("plaid", "link_seq")
    public = "public-sandbox-" + str(n)
    c = store_collection("public_tokens")
    c.insert({
        "id": public,
        "item_id": "item-seed-a",
    })
    return public

# _resolve_item_id maps an access_token to an item_id.
def _resolve_item_id(access_token):
    ac = store_collection("access_tokens")
    doc = ac.get(access_token)
    if doc == None:
        return ""
    return doc.get("item_id", "")

# _item_accounts returns the list of account documents for an item.
def _item_accounts(item_id):
    ac = store_collection("accounts")
    all_accts = ac.list()
    result = []
    for a in all_accts:
        if a.get("item_id", "") == item_id:
            result.append(_account_public(a))
    return result

# _account_public strips internal fields and returns the Plaid-shaped account.
def _account_public(a):
    return {
        "account_id": a["id"],
        "balances": {
            "available": a.get("balances", {}).get("available", None),
            "current": a.get("balances", {}).get("current", None),
            "iso_currency_code": a.get("balances", {}).get("iso_currency_code", None),
            "limit": None,
            "unofficial_currency_code": None,
        },
        "mask": a.get("mask", "0000"),
        "name": a.get("name", ""),
        "official_name": None,
        "subtype": a.get("subtype", ""),
        "type": a.get("type", "depository"),
    }

# _tx_public returns the Plaid-shaped transaction.
def _tx_public(t):
    return {
        "transaction_id": t["id"],
        "account_id": t["account_id"],
        "amount": t["amount"],
        "date": t["date"],
        "name": t["name"],
        "merchant_name": None,
        "category": t.get("category", []),
        "subcategory": None,
        "iso_currency_code": "USD",
        "unofficial_currency_code": None,
        "pending": t.get("pending", False),
        "transaction_type": "place",
        "payment_channel": "in store",
        "location": {},
        "payment_meta": {},
    }
