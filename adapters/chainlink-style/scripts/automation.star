# Automation handlers — the keepers-registry upkeep lifecycle.
#
# Automation endpoints require auth (Bearer token).
# STATEFUL upkeeps are stored in the "upkeeps" collection.
#
# POST /v2/automation/registerUpkeep       -> { upkeepID, status:"registered", ... }
# GET  /v2/automation/upkeeps              -> { data: [{ upkeepID, name, ... }] }
# GET  /v2/automation/{id}                 -> { data: { ... } } (derived state)
# POST /v2/automation/{id}/fund            -> addFunds (LINK, in juels)
# POST /v2/automation/{id}/cancel          -> cancelUpkeep
# POST /v2/automation/{id}/withdraw        -> withdrawFunds (admin, after cancel)
# GET  /v2/automation/{id}/check           -> checkUpkeep (upkeepNeeded + performData)
# POST /v2/automation/{id}/perform         -> performUpkeep (manual perform)
# GET  /v2/automation/{id}/performs        -> performed history (derive-on-read)
#
# REGISTRY SEMANTICS (keepers registry shape): an upkeep is registered with
# a name, gas limit, admin, checkData, trigger type and an initial balance
# (LINK, tracked in juels). Balances are uint96-scale, which a JSON document
# store cannot hold exactly as a number — they are stored and serialized as
# STRINGS and converted to exact ints for arithmetic (_balance/_set_balance).
#
# The keepers network performs eligible upkeeps automatically on the upkeep
# cadence (`interval`, simulator seconds); each perform deducts the LINK
# premium (0.25 LINK here) from the balance. CANCELLING freezes the upkeep
# (no further performs); the remaining balance can then be WITHDRAWN by the
# admin. Performed history is DERIVED ON READ from the clock: reads simulate
# every missed perform tick since registration and persist the entries, so
# history and balance advance with real time.

# _PREMIUM_JUELS is the LINK premium charged per perform (0.25 LINK).
def _premium_juels():
    return _pow10(18) // 4

# _link_to_juels converts a LINK amount (int) to juels.
def _link_to_juels(link):
    return link * _pow10(18)

# _balance reads an upkeep's balance as an exact int (string-stored).
def _balance(doc):
    return _to_int(doc.get("balanceJuels", "0"))

# _set_balance stores a balance (string, floored at zero).
def _set_balance(doc, v):
    if v < 0:
        v = 0
    doc["balanceJuels"] = str(v)

# on_register_upkeep registers a new Automation upkeep (registry shape).
def on_register_upkeep(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    name = body.get("name", "")
    if name == None or name == "":
        return _cl_err(400, "BAD_REQUEST", "name is required")
    gas_limit = _as_whole(body.get("gasLimit", 500 * 1000), -1)
    if gas_limit <= 0 or gas_limit > 5000 * 1000:
        return _cl_err(400, "BAD_REQUEST", "gasLimit must be a positive integer (max 5,000,000)")
    amount = _as_whole(body.get("amount", 0), -1)
    if amount < 0:
        return _cl_err(400, "BAD_REQUEST", "amount must be a non-negative integer (LINK)")
    interval = _as_whole(body.get("interval", 300), 300)
    if interval < 1:
        interval = 300

    upkeep_id = _upkeep_id()
    now = clock.now_unix()

    doc = {
        "upkeepID": upkeep_id,
        "name": name,
        "encryptedEmail": body.get("encryptedEmail", ""),
        "adminAddr": body.get("adminAddr", "0x" + "ab" * 20),
        "triggerType": body.get("triggerType", "condition"),
        "network": body.get("network", "ethereum"),
        "status": "active",
        "gasLimit": gas_limit,
        "checkData": body.get("checkData", "0x"),
        "balanceJuels": str(_link_to_juels(amount)),
        "_registered_at": now,
        "_interval": interval,
        "_performed_ticks": 0,
        "_perform_count": 0,
        "_last_performed_at": 0,
        "_performs": [],
    }

    store_collection("upkeeps").insert(doc)

    return respond(200, {
        "upkeepID": upkeep_id,
        "name": name,
        "status": "registered",
        "adminAddr": doc["adminAddr"],
        "triggerType": doc["triggerType"],
        "network": doc["network"],
        "gasLimit": gas_limit,
        "balance": doc["balanceJuels"],
    })

# on_list_upkeeps lists all registered upkeeps with derived state.
def on_list_upkeeps(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("upkeeps")
    upkeeps = []
    for doc in c.list():
        _derive_performs(c, doc)
        upkeeps.append(_upkeep_view(doc))

    page, next_cursor = _list_page(req, upkeeps)
    if page == None:
        return _cl_err(400, "invalid_cursor", "Invalid cursor token")
    body = {"data": page}
    if next_cursor != None:
        body["nextCursor"] = next_cursor
    return respond(200, body)

# on_get_upkeep returns a single upkeep by ID with derived state.
def on_get_upkeep(req):
    err = _require_auth(req)
    if err != None:
        return err

    upkeep_id = req["params"].get("id", "")
    if upkeep_id == None or upkeep_id == "":
        return _cl_err(400, "BAD_REQUEST", "id path parameter is required")

    c = store_collection("upkeeps")
    for doc in c.list():
        if doc.get("upkeepID", "") == upkeep_id:
            _derive_performs(c, doc)
            return respond(200, {"data": _upkeep_view(doc)})

    return _cl_err(404, "NOT_FOUND", "Upkeep not found: " + upkeep_id)

# on_fund_upkeep adds LINK to an upkeep's balance (registry addFunds).
def on_fund_upkeep(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc, resp = _find_upkeep(req)
    if resp != None:
        return resp
    body = req.get("body")
    if body == None:
        body = {}
    amount = _as_whole(body.get("amount", None), -1)
    if amount <= 0:
        return _cl_err(400, "BAD_REQUEST", "body must include a positive integer 'amount' (LINK)")
    if doc.get("status", "") == "cancelled":
        return _cl_err(400, "UPKEEP_CANCELLED", "cannot add funds to a cancelled upkeep")

    c = store_collection("upkeeps")
    _derive_performs(c, doc)
    _set_balance(doc, _balance(doc) + _link_to_juels(amount))
    c.update(doc["id"], doc)

    return respond(200, {
        "data": {
            "upkeepID": doc["upkeepID"],
            "addedJuels": str(_link_to_juels(amount)),
            "balance": doc["balanceJuels"],
        },
    })

# on_cancel_upkeep cancels an upkeep (registry cancelUpkeep): no further
# performs; the balance stays until withdrawn.
def on_cancel_upkeep(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc, resp = _find_upkeep(req)
    if resp != None:
        return resp
    if doc.get("status", "") == "cancelled":
        return _cl_err(400, "UPKEEP_ALREADY_CANCELLED", "upkeep is already cancelled")

    c = store_collection("upkeeps")
    _derive_performs(c, doc)
    doc["status"] = "cancelled"
    c.update(doc["id"], doc)

    return respond(200, {
        "data": {
            "upkeepID": doc["upkeepID"],
            "status": "cancelled",
            "balance": doc["balanceJuels"],
        },
    })

# on_withdraw_upkeep withdraws the remaining balance (registry withdrawFunds
# — only a cancelled upkeep can be withdrawn from).
def on_withdraw_upkeep(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc, resp = _find_upkeep(req)
    if resp != None:
        return resp
    if doc.get("status", "") != "cancelled":
        return _cl_err(400, "UPKEEP_NOT_CANCELLED", "upkeep must be cancelled before funds can be withdrawn")
    balance = _balance(doc)
    if balance <= 0:
        return _cl_err(400, "NO_FUNDS", "upkeep has no remaining funds to withdraw")
    body = req.get("body")
    if body == None:
        body = {}
    to = body.get("to", doc.get("adminAddr", ""))

    c = store_collection("upkeeps")
    _set_balance(doc, 0)
    c.update(doc["id"], doc)

    return respond(200, {
        "data": {
            "upkeepID": doc["upkeepID"],
            "amount": str(balance),
            "to": to,
        },
    })

# on_check_upkeep simulates the registry's checkUpkeep call: whether the
# upkeep is currently eligible and the performData it would hand to
# performUpkeep. An upkeep is eligible once its cadence interval has elapsed
# since the last perform (and it is still active with balance).
def on_check_upkeep(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc, resp = _find_upkeep(req)
    if resp != None:
        return resp

    c = store_collection("upkeeps")
    _derive_performs(c, doc)
    needed, perform_data = _check_upkeep(doc)

    return respond(200, {
        "data": {
            "upkeepID": doc["upkeepID"],
            "upkeepNeeded": needed,
            "performData": perform_data,
        },
    })

# on_perform_upkeep performs the upkeep manually (registry performUpkeep):
# deducts the LINK premium, records the perform, and returns the receipt.
def on_perform_upkeep(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc, resp = _find_upkeep(req)
    if resp != None:
        return resp
    if doc.get("status", "") == "cancelled":
        return _cl_err(400, "UPKEEP_CANCELLED", "cannot perform a cancelled upkeep")

    c = store_collection("upkeeps")
    _derive_performs(c, doc)
    needed, perform_data = _check_upkeep(doc)
    if not needed:
        return _cl_err(400, "UPKEEP_NOT_NEEDED", "upkeep is not currently eligible (interval not elapsed)")

    now = clock.now_unix()
    entry = {
        "id": str(doc.get("_perform_count", 0) + 1),
        "performedAt": now,
        "gasUsed": _perform_gas(doc),
        "transactionHash": _perform_tx(doc, now),
        "trigger": "manual",
        "premiumJuels": str(_premium_juels()),
    }
    doc["_performs"].append(entry)
    doc["_perform_count"] = doc.get("_perform_count", 0) + 1
    doc["_last_performed_at"] = now
    _set_balance(doc, _balance(doc) - _premium_juels())
    c.update(doc["id"], doc)

    return respond(200, {
        "data": {
            "upkeepID": doc["upkeepID"],
            "performed": True,
            "performData": perform_data,
            "transactionHash": entry["transactionHash"],
            "gasUsed": entry["gasUsed"],
            "premiumJuels": entry["premiumJuels"],
            "balance": doc["balanceJuels"],
        },
    })

# on_list_performs returns the upkeep's performed history (newest first),
# derived on read — the keepers network performs eligible upkeeps on the
# cadence without being asked.
def on_list_performs(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc, resp = _find_upkeep(req)
    if resp != None:
        return resp

    c = store_collection("upkeeps")
    _derive_performs(c, doc)

    entries = []
    for e in doc.get("_performs", []):
        entries.append(e)
    # newest first
    rev = []
    for i in range(len(entries) - 1, -1, -1):
        rev.append(entries[i])

    page, next_cursor = _list_page(req, rev)
    if page == None:
        return _cl_err(400, "invalid_cursor", "Invalid cursor token")
    body = {"data": page, "count": len(rev)}
    if next_cursor != None:
        body["nextCursor"] = next_cursor
    return respond(200, body)

# _find_upkeep locates the upkeep from the {id} path param.
# Returns (doc, None) or (None, error_response).
def _find_upkeep(req):
    upkeep_id = req["params"].get("id", "")
    if upkeep_id == None or upkeep_id == "":
        return None, _cl_err(400, "BAD_REQUEST", "id path parameter is required")
    for doc in store_collection("upkeeps").list():
        if doc.get("upkeepID", "") == upkeep_id:
            return doc, None
    return None, _cl_err(404, "NOT_FOUND", "Upkeep not found: " + upkeep_id)

# _check_upkeep computes (upkeepNeeded, performData). An ACTIVE upkeep with
# balance is eligible once `interval` seconds have elapsed since the last
# perform (never-performed upkeeps are immediately eligible).
def _check_upkeep(doc):
    if doc.get("status", "") != "active" or _balance(doc) < _premium_juels():
        return False, "0x"
    now = clock.now_unix()
    last = _as_int(doc.get("_last_performed_at", 0))
    interval = _as_int(doc.get("_interval", 300))
    if now - last < interval:
        return False, "0x"
    return True, "0x" + crypto.hmac_sha256("upkeep-perform", doc.get("upkeepID", "") + ":" + str(now // interval), "hex")[0:16]

# _perform_gas derives a deterministic gas usage for a perform.
def _perform_gas(doc):
    rid = _to_int(doc.get("upkeepID", "0"))
    return _as_int(doc.get("gasLimit", 500 * 1000)) // 4 + (rid % (50 * 1000))

# _perform_tx derives a deterministic transaction hash for a perform.
def _perform_tx(doc, at):
    return "0x" + crypto.hmac_sha256("upkeep-tx", doc.get("upkeepID", "") + ":" + str(at), "hex")

# _derive_performs simulates the keepers network: every elapsed cadence tick
# since registration counts as one automatic perform (each deducting the
# premium), but only while the upkeep is ACTIVE and can cover the premium —
# exactly the registry's behavior of stopping upkeeps that run out of LINK.
# The entries are persisted so history and balance advance with real time.
def _derive_performs(c, doc):
    if doc.get("status", "") != "active":
        return
    now = clock.now_unix()
    interval = _as_int(doc.get("_interval", 300))
    registered = _as_int(doc.get("_registered_at", now))
    ticks = (now - registered) // interval
    if ticks < 0:
        ticks = 0
    changed = False
    while doc.get("_performed_ticks", 0) < ticks:
        tick = doc.get("_performed_ticks", 0) + 1
        if _balance(doc) < _premium_juels():
            break
        performed_at = registered + tick * interval
        if performed_at > now:
            break
        entry = {
            "id": str(doc.get("_perform_count", 0) + 1),
            "performedAt": performed_at,
            "gasUsed": _perform_gas(doc),
            "transactionHash": _perform_tx(doc, performed_at),
            "trigger": "auto",
            "premiumJuels": str(_premium_juels()),
        }
        doc["_performs"].append(entry)
        doc["_perform_count"] = doc.get("_perform_count", 0) + 1
        doc["_performed_ticks"] = tick
        doc["_last_performed_at"] = performed_at
        _set_balance(doc, _balance(doc) - _premium_juels())
        changed = True
        # Keep the persisted history bounded (latest 100).
        if len(doc["_performs"]) > 100:
            doc["_performs"] = doc["_performs"][len(doc["_performs"]) - 100:]
    if changed:
        c.update(doc["id"], doc)

# _upkeep_view returns the public upkeep shape (internal _ keys stripped;
# uint96-scale balances serialized as strings).
def _upkeep_view(doc):
    return {
        "upkeepID": doc.get("upkeepID", ""),
        "name": doc.get("name", ""),
        "triggerType": doc.get("triggerType", "condition"),
        "network": doc.get("network", "ethereum"),
        "status": doc.get("status", "active"),
        "gasLimit": _as_int(doc.get("gasLimit", 500 * 1000)),
        "balance": doc.get("balanceJuels", "0"),
        "performedCount": _as_int(doc.get("_perform_count", 0)),
        "lastPerformedAt": _as_int(doc.get("_last_performed_at", 0)),
    }
