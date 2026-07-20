# Functions handlers — createSecrets, encryptSecrets, createRequest.
#
# Functions endpoints require auth (Bearer token).
# STATEFUL secrets are stored in the "secrets" collection.
#
# POST /v2/functions/createSecrets   → { secretID, encryptedSecrets, ... }
# POST /v2/functions/encryptSecrets  → { encryptedSecrets, ... }
# POST /v2/functions/createRequest   → { requestID, ... }

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

# on_create_request creates a Functions request.
def on_create_request(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    request_id = _request_id()

    return respond(200, {
        "requestID": request_id,
        "donId": body.get("donId", "fun-ethereum-mainnet-1"),
        "subscriptionId": body.get("subscriptionId", 0),
        "status": "queued",
        "encryptedSecrets": body.get("encryptedSecrets", ""),
    })
