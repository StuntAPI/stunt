# Automation handlers — registerUpkeep, list upkeeps, get upkeep.
#
# Automation endpoints require auth (Bearer token).
# STATEFUL upkeeps are stored in the "upkeeps" collection.
#
# POST /v2/automation/registerUpkeep  → { upkeepID, status:"registered", ... }
# GET  /v2/automation/upkeeps         → { data: [{ upkeepID, name, ... }] }
# GET  /v2/automation/{id}            → { data: { upkeepID, name, ... } }

# on_register_upkeep registers a new Automation upkeep (cron or condition).
def on_register_upkeep(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    upkeep_id = _upkeep_id()

    doc = {
        "upkeepID": upkeep_id,
        "name": body.get("name", "unnamed-upkeep"),
        "triggerType": body.get("triggerType", "condition"),
        "network": body.get("network", "ethereum"),
        "status": "active",
        "gasLimit": body.get("gasLimit", 500000),
        "checkData": body.get("checkData", "0x"),
    }

    c = store_collection("upkeeps")
    c.insert(doc)

    return respond(200, {
        "upkeepID": upkeep_id,
        "name": doc["name"],
        "status": "registered",
        "network": doc["network"],
        "triggerType": doc["triggerType"],
    })

# on_list_upkeeps lists all registered upkeeps.
def on_list_upkeeps(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("upkeeps")
    docs = c.list()

    upkeeps = []
    for doc in docs:
        upkeeps.append({
            "upkeepID": doc.get("upkeepID", ""),
            "name": doc.get("name", ""),
            "triggerType": doc.get("triggerType", "condition"),
            "network": doc.get("network", "ethereum"),
            "status": doc.get("status", "active"),
        })

    return respond(200, {
        "data": upkeeps,
    })

# on_get_upkeep returns a single upkeep by ID.
def on_get_upkeep(req):
    err = _require_auth(req)
    if err != None:
        return err

    upkeep_id = req["params"].get("id", "")
    if upkeep_id == None or upkeep_id == "":
        return _cl_err(400, "BAD_REQUEST", "id path parameter is required")

    c = store_collection("upkeeps")
    docs = c.list()

    for doc in docs:
        if doc.get("upkeepID", "") == upkeep_id:
            return respond(200, {
                "data": {
                    "upkeepID": doc.get("upkeepID", ""),
                    "name": doc.get("name", ""),
                    "triggerType": doc.get("triggerType", "condition"),
                    "network": doc.get("network", "ethereum"),
                    "status": doc.get("status", "active"),
                    "gasLimit": doc.get("gasLimit", 500000),
                },
            })

    return _cl_err(404, "NOT_FOUND", "Upkeep not found: " + upkeep_id)
