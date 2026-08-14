# Functions handlers — createSecrets, encryptSecrets, createRequest.
#
# Functions endpoints require auth (Bearer token).
# STATEFUL secrets are stored in the "secrets" collection.
# STATEFUL requests are stored in the "requests" collection.
#
# POST /v2/functions/createSecrets   → { secretID, encryptedSecrets, ... }
# POST /v2/functions/encryptSecrets  → { encryptedSecrets, ... }
# POST /v2/functions/createRequest   → { requestID, status: "pending", ... }
# GET  /v2/functions/request/{id}    → derived status (see _derive_request)
#
# REQUEST LIFECYCLE (derive-on-read): a request is created "pending", flips
# to "in_flight" once the DON picks it up, and lands in "fulfilled" (or
# "failed"). Status is derived from the engine clock at GET time —
# pending (0-1s) -> in_flight (1-3s) -> fulfilled | failed (>=3s) — and
# persisted back so repeated polls agree. SIMULATOR EXTENSION: pass
# "simulate_fail": true in the createRequest body to force the terminal
# state "failed" (real Functions requests fail on DON execution errors /
# timeouts; there is no sandbox failure trigger in the off-chain API).

# on_create_secrets creates and stores encrypted secrets for Functions.
def on_create_secrets(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    secret_id = _secret_id()
    enc = _encrypted_secrets()

    doc = {
        "secretID": secret_id,
        "encryptedSecrets": enc,
        "slotIDs": body.get("slotIDs", [0]),
        "network": body.get("network", "ethereum"),
    }

    c = store_collection("secrets")
    c.insert(doc)

    return respond(200, {
        "secretID": secret_id,
        "encryptedSecrets": enc,
        "slotIDs": doc["slotIDs"],
        "network": doc["network"],
    })

# on_encrypt_secrets encrypts a secrets payload without storing it.
def on_encrypt_secrets(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    enc = _encrypted_secrets()

    return respond(200, {
        "encryptedSecrets": enc,
        "slotIDs": body.get("slotIDs", [0]),
        "network": body.get("network", "ethereum"),
    })

# on_create_request creates a Functions request and stores it so its
# lifecycle can be derived on read (pending -> in_flight -> fulfilled/failed).
def on_create_request(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    request_id = _request_id()
    now = clock.now_unix()

    fail = body.get("simulate_fail", False)
    if fail == None:
        fail = False

    doc = {
        "requestID": request_id,
        "donId": body.get("donId", "fun-ethereum-mainnet-1"),
        "subscriptionId": body.get("subscriptionId", 0),
        "encryptedSecrets": body.get("encryptedSecrets", ""),
        "status": "pending",
        "_running_at": now + 1,
        "_done_at": now + 3,
        "_fail": fail,
    }
    store_collection("requests").insert(doc)

    return respond(200, _request_view(doc))

# on_get_request returns a Functions request with its CURRENT status derived
# from the clock (derive-on-read) and persisted back to the collection.
def on_get_request(req):
    err = _require_auth(req)
    if err != None:
        return err

    rid = req.get("params", {}).get("requestID", "")
    rc = store_collection("requests")
    for doc in rc.list():
        if doc.get("requestID", "") == rid:
            _derive_request(rc, doc)
            return respond(200, _request_view(doc))
    return _cl_err(404, "NOT_FOUND", "Functions request not found: " + rid)

# _derive_request advances the stored status using the engine clock:
#   now <  _running_at -> "pending"   (request accepted, waiting on the DON)
#   now <  _done_at    -> "in_flight" (DON is executing the JavaScript)
#   now >= _done_at    -> "fulfilled" (or "failed" with simulate_fail)
# The derived status is persisted so webhooks/lists agree with the poll and
# each transition is applied exactly once (terminal fields are filled in on
# first arrival at the terminal state).
def _derive_request(rc, doc):
    now = clock.now_unix()
    status = "pending"
    if now >= doc.get("_done_at", 0):
        if doc.get("_fail", False):
            status = "failed"
        else:
            status = "fulfilled"
    elif now >= doc.get("_running_at", 0):
        status = "in_flight"

    if status == doc.get("status", ""):
        return

    doc["status"] = status
    if status == "fulfilled":
        doc["result"] = "0x" + _hex_pad(_to_int(doc.get("requestID", "0")), 64)
        doc["completedAt"] = now
    elif status == "failed":
        doc["errorMessage"] = "DON request failed: execution reverted"
        doc["completedAt"] = now
    rc.update(doc["id"], doc)

# _request_view returns the public request shape (internal _-prefixed
# timing fields are stripped).
def _request_view(doc):
    out = {
        "requestID": doc.get("requestID", ""),
        "donId": doc.get("donId", ""),
        "subscriptionId": doc.get("subscriptionId", 0),
        "status": doc.get("status", "pending"),
        "encryptedSecrets": doc.get("encryptedSecrets", ""),
    }
    if out["status"] == "fulfilled":
        out["result"] = doc.get("result", "")
        out["completedAt"] = doc.get("completedAt", 0)
    elif out["status"] == "failed":
        out["errorMessage"] = doc.get("errorMessage", "")
        out["completedAt"] = doc.get("completedAt", 0)
    return out
