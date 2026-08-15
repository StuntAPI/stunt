# Transactions sync handler — cursor-based add/modify/remove sync.
#
# POST /transactions/sync
#   { access_token, cursor, count }
#   -> { added: [...], modified: [...], removed: [...], next_cursor, request_id }
#
# STATEFUL: every transaction carries a per-item monotonic `seq` and a
# `state` of "new" (never delivered), "modified" (delivered then mutated —
# see /sandbox/item/fire_webhook), or "removed" (tombstone). The cursor
# ("cursor-N") is the watermark: the client has consumed every update with
# seq <= N. Each sync returns the item's transactions with seq > N, capped
# at `count` (combined added+modified+removed, real Plaid range 1-500,
# default 100), and advances next_cursor to the last seq served.

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

    # Real /transactions/sync `count` param: 1-500, default 100. Caps the
    # number of updates (added + modified + removed) returned per sync.
    count = body.get("count", 100)
    if count == None or count <= 0:
        count = 100
    if count > 500:
        count = 500
    # JSON numbers may arrive as floats; the cap below needs an int.
    count = int(count)

    item_id = _resolve_item_id(access_token)
    if item_id == "":
        return respond(400, {
            "display_message": None,
            "error_type": "INVALID_INPUT",
            "error_code": "INVALID_ACCESS_TOKEN",
            "error_message": "access_token does not exist",
            "request_id": _request_id(),
        })

    # Watermark: everything with seq <= watermark has been consumed.
    if cursor == "" or cursor == None:
        watermark = 0
    else:
        watermark = _parse_int(cursor.replace("cursor-", ""))

    # Candidate updates for this item above the watermark, oldest first.
    tc = store_collection("transactions")
    candidates = []
    for t in tc.list():
        if t.get("seq", 0) <= watermark:
            continue
        if not _account_belongs_to_item(t.get("account_id", ""), item_id):
            continue
        candidates.append(t)
    candidates = query_select(candidates, None, "seq", "asc", None, None, None)

    # Serve at most `count` updates in seq order.
    served = []
    for t in candidates:
        if len(served) >= count:
            break
        served.append(t)

    added = []
    modified = []
    removed = []
    last_seq = watermark
    for t in served:
        # JSON numbers may arrive as floats ("3"); coerce so the cursor is
        # always "cursor-3", never "cursor-3.0" (which would not parse back).
        last_seq = int(t.get("seq", watermark))
        state = t.get("state", "new")
        if state == "removed":
            removed.append({
                "transaction_id": t["id"],
                "account_id": t.get("account_id", ""),
            })
        elif state == "modified":
            modified.append(_tx_public(t))
        else:
            added.append(_tx_public(t))

    # Cursor math: advance to the last seq actually served; when the count
    # cap truncated the batch, the un-served updates come back on the next
    # sync with the returned cursor.
    if len(served) > 0:
        next_cursor = "cursor-" + str(last_seq)
    elif watermark > 0:
        next_cursor = "cursor-" + str(watermark)
    else:
        next_cursor = ""

    # Emit a webhook event for the update (if webhooks are registered).
    if len(added) + len(modified) + len(removed) > 0:
        events_emit("SYNC_UPDATES_AVAILABLE", {
            "webhook_type": "TRANSACTIONS",
            "webhook_code": "SYNC_UPDATES_AVAILABLE",
            "item_id": item_id,
            "initial_update_complete": True,
            "historical_update_complete": True,
        })

    return respond(200, {
        "added": added,
        "modified": modified,
        "removed": removed,
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
