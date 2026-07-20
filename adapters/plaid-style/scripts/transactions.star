# Transactions sync handler — cursor-based pagination.
#
# POST /transactions/sync
#   { access_token, cursor }
#   -> { added: [...], modified: [...], removed: [...], next_cursor, request_id }
#
# STATEFUL: transactions are batched by cursor_batch. Each sync returns the
# next batch and advances the cursor. When all batches are consumed, an empty
# result is returned with the final cursor.

# Shared helpers (_check_auth, _request_id, _resolve_item_id, _tx_public)
# from lib.star.

def on_sync(req):
    err = _check_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    access_token = body.get("access_token", "")
    cursor = body.get("cursor", "")

    item_id = _resolve_item_id(access_token)
    if item_id == "":
        return respond(400, {
            "display_message": None,
            "error_type": "INVALID_INPUT",
            "error_code": "INVALID_ACCESS_TOKEN",
            "error_message": "access_token does not exist",
            "request_id": _request_id(),
        })

    # Determine which batch to serve based on the cursor.
    if cursor == "" or cursor == None:
        batch = 0
    else:
        # cursor-N -> batch N+1 (cursor-0 means "next is batch 1")
        num_str = cursor.replace("cursor-", "")
        batch = _parse_int(num_str) + 1

    # Get transactions for this item at the requested batch.
    tc = store_collection("transactions")
    all_tx = tc.list()
    added = []
    for t in all_tx:
        # Only return transactions for accounts belonging to this item.
        acct_id = t.get("account_id", "")
        if not _account_belongs_to_item(acct_id, item_id):
            continue
        tx_batch = _parse_int(t.get("cursor_batch_str", ""))
        # Use the stored cursor_batch if cursor_batch_str is absent.
        if tx_batch == 0:
            tx_batch = t.get("cursor_batch", 0)
        if tx_batch == batch:
            added.append(_tx_public(t))

    next_cursor = _new_cursor(batch)

    # Emit a webhook event for the initial update (if webhooks are set).
    if len(added) > 0:
        events_emit("SYNC_UPDATES_AVAILABLE", {
            "webhook_type": "TRANSACTIONS",
            "webhook_code": "SYNC_UPDATES_AVAILABLE",
            "item_id": item_id,
            "initial_update_complete": True,
            "historical_update_complete": True,
        })

    return respond(200, {
        "added": added,
        "modified": [],
        "removed": [],
        "next_cursor": next_cursor,
        "request_id": _request_id(),
    })

# _account_belongs_to_item checks whether an account belongs to an item.
def _account_belongs_to_item(acct_id, item_id):
    ac = store_collection("accounts")
    a = ac.get(acct_id)
    if a == None:
        return False
    return a.get("item_id", "") == item_id

# _parse_int converts a string to int, returns 0 on failure.
def _parse_int(s):
    if s == None:
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n
