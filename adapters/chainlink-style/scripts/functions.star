# Functions handlers — createSecrets, encryptSecrets, createRequest.
#
# Functions endpoints require auth (Bearer token).
# STATEFUL secrets are stored in the "secrets" collection.
# STATEFUL requests are stored in the "requests" collection.
#
# POST /v2/functions/createSecrets   -> { secretID, encryptedSecrets, versions, ... }
# POST /v2/functions/encryptSecrets  -> { encryptedSecrets, ... }
# POST /v2/functions/createRequest   -> { requestID, status: "queued", ... }
# GET  /v2/functions/request/{id}    -> derived status (see _derive_request)
#
# SECRETS ENCRYPTION — HONEST DEVIATION (see README): the real service
# encrypts secrets to the DON's public key with ECIES (randomized). stunt's
# crypto module has NO encryption primitives (MAC/hash/sign only), so this
# adapter computes a DETERMINISTIC version-tagged HMAC-SHA256 envelope:
# integrity + versioning semantics (slotID + version), not confidentiality.
# The plaintext is never stored and cannot be recovered from the envelope.
#
# REQUEST LIFECYCLE (derive-on-read): a request is created "queued", flips to
# "running" once the DON picks it up, and lands in "fulfilled" (or "failed").
# Status is derived from the engine clock at GET time —
# queued (0-1s) -> running (1-3s) -> fulfilled | failed (>=3s) — and
# persisted back so repeated polls agree. A fulfilled request carries the
# returned bytes32 result plus the on-chain RequestFulfilled event shape
# (requestId, subscriptionId, data, gasUsed, gasUsedAndChainIdCode).
# SIMULATOR EXTENSION: pass "simulate_fail": true (or one of the failure
# kinds below) in the createRequest body to force the terminal state
# "failed" with the real Functions fulfillment-code vocabulary.

# _FAIL_KINDS maps simulate_fail kinds to (fulfillmentCode, errorMessage).
# The codes and messages are the real Functions fulfillment vocabulary:
#   1 FULFILLMENT_CODE_USER_ERROR           — response violates constraints
#   2 FULFILLMENT_CODE_COMPUTED_FAILED      — source JS failed / exceeded
#   3 FULFILLMENT_CODE_COST_EXCEEDS_COMMITMENT — cost > subscription balance
_FAIL_KINDS = {
    "computation": [2, "code 2: computation exceeded"],
    "js_error": [2, "code 2: Uncaught exception inside Functions source"],
    "timeout": [2, "code 2: computation exceeded (request timed out)"],
    "user_error": [1, "code 1: response exceeds 256 bytes"],
    "response_size": [1, "code 1: response exceeds 256 bytes"],
    "balance": [3, "code 3: cost exceeds commitment (subscription balance too low)"],
    "cost": [3, "code 3: cost exceeds commitment (subscription balance too low)"],
}

_FAIL_CODE_NAMES = {
    0: "FULFILLMENT_CODE_SUCCESS",
    1: "FULFILLMENT_CODE_USER_ERROR",
    2: "FULFILLMENT_CODE_COMPUTED_FAILED",
    3: "FULFILLMENT_CODE_COST_EXCEEDS_COMMITMENT",
}

# on_create_secrets uploads DON secrets: each slot gets a new VERSION (the
# real slot/version semantics) and a deterministic encryptedSecrets envelope.
def on_create_secrets(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}
    secrets = body.get("secrets", None)
    if secrets == None or type(secrets) != type({}) or len(secrets) == 0:
        return _cl_err(400, "BAD_REQUEST", "body must include a non-empty 'secrets' object")

    don_id = body.get("donId", "fun-ethereum-mainnet-1")
    network = body.get("network", "ethereum")

    slot_ids = body.get("slotIDs", [0])
    if slot_ids == None or type(slot_ids) != type([]) or len(slot_ids) == 0:
        slot_ids = [0]

    versions = []
    envelopes = []
    for slot in slot_ids:
        version = store_kv_incr("chainlink", "slotver:" + don_id + ":" + str(slot))
        versions.append(version)
        envelopes.append(_secrets_envelope(don_id, slot, version, secrets))

    secret_id = _secret_id()
    doc = {
        "secretID": secret_id,
        "encryptedSecrets": envelopes[0],
        "slotIDs": slot_ids,
        "versions": versions,
        "donId": don_id,
        "network": network,
    }
    store_collection("secrets").insert(doc)

    return respond(200, _secrets_view(doc))

# on_encrypt_secrets encrypts a secrets payload WITHOUT storing it (version 0
# — no slot upload). Deterministic: same payload -> same envelope.
def on_encrypt_secrets(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}
    secrets = body.get("secrets", None)
    if secrets == None or type(secrets) != type({}) or len(secrets) == 0:
        return _cl_err(400, "BAD_REQUEST", "body must include a non-empty 'secrets' object")

    don_id = body.get("donId", "fun-ethereum-mainnet-1")
    slot_ids = body.get("slotIDs", [0])
    if slot_ids == None or type(slot_ids) != type([]) or len(slot_ids) == 0:
        slot_ids = [0]

    return respond(200, {
        "encryptedSecrets": _secrets_envelope(don_id, slot_ids[0], 0, secrets),
        "slotIDs": slot_ids,
        "donId": don_id,
        "network": body.get("network", "ethereum"),
    })

# on_get_secrets returns a stored secrets upload (envelope + slot versions;
# the plaintext is never stored).
def on_get_secrets(req):
    err = _require_auth(req)
    if err != None:
        return err

    sid = req.get("params", {}).get("secretID", "")
    sc = store_collection("secrets")
    for doc in sc.list():
        if doc.get("secretID", "") == sid:
            return respond(200, _secrets_view(doc))
    return _cl_err(404, "NOT_FOUND", "Secrets upload not found: " + sid)

# _secrets_view is the public shape of a secrets upload.
def _secrets_view(doc):
    return {
        "secretID": doc.get("secretID", ""),
        "encryptedSecrets": doc.get("encryptedSecrets", ""),
        "slotIDs": doc.get("slotIDs", [0]),
        "versions": doc.get("versions", [1]),
        "donId": doc.get("donId", ""),
        "network": doc.get("network", "ethereum"),
    }

# on_create_request creates a Functions request and stores it so its
# lifecycle can be derived on read (queued -> running -> fulfilled/failed).
def on_create_request(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    request_id = _request_id()
    now = clock.now_unix()

    # Failure injection kind: true -> the default (computation), or one of
    # the vocabulary strings above; anything unrecognized -> computation.
    fail_kind = ""
    fail = body.get("simulate_fail", False)
    if fail == None:
        fail = False
    if type(fail) == type("") and fail != "":
        fail_kind = fail
        if fail_kind not in _FAIL_KINDS:
            fail_kind = "computation"
    elif fail != False and fail != "" and fail != 0:
        fail_kind = "computation"

    doc = {
        "requestID": request_id,
        "donId": body.get("donId", "fun-ethereum-mainnet-1"),
        "subscriptionId": body.get("subscriptionId", 0),
        "network": body.get("network", "ethereum"),
        "encryptedSecrets": body.get("encryptedSecrets", ""),
        "gasLimit": body.get("gasLimit", 500 * 1000),
        "status": "queued",
        "_running_at": now + 1,
        "_done_at": now + 3,
        "_fail_kind": fail_kind,
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
#   now <  _running_at -> "queued"   (request accepted, waiting on the DON)
#   now <  _done_at    -> "running"  (DON is executing the JavaScript)
#   now >= _done_at    -> "fulfilled" (or "failed" via simulate_fail)
# The derived status is persisted so repeated polls agree and each terminal
# field is filled in exactly once (on first arrival at the terminal state).
def _derive_request(rc, doc):
    now = clock.now_unix()
    status = "queued"
    if now >= doc.get("_done_at", 0):
        if doc.get("_fail_kind", "") != "":
            status = "failed"
        else:
            status = "fulfilled"
    elif now >= doc.get("_running_at", 0):
        status = "running"

    if status == doc.get("status", ""):
        return

    doc["status"] = status
    if status == "fulfilled":
        doc["result"] = _result_bytes32(doc)
        doc["fulfillEvent"] = _fulfill_event(doc, doc["result"])
        doc["completedAt"] = now
    elif status == "failed":
        pair = _FAIL_KINDS.get(doc.get("_fail_kind", ""), [2, "code 2: computation exceeded"])
        doc["fulfillmentCode"] = pair[0]
        doc["fulfillmentCodeName"] = _FAIL_CODE_NAMES.get(pair[0], "FULFILLMENT_CODE_COMPUTED_FAILED")
        doc["errorMessage"] = pair[1]
        doc["completedAt"] = now
    rc.update(doc["id"], doc)

# _result_bytes32 derives the request's returned bytes32 result
# deterministically from the request id.
def _result_bytes32(doc):
    h = crypto.hmac_sha256("functions-result", doc.get("requestID", ""), "hex")
    return "0x" + h[0:64]

# _fulfill_event returns the on-chain RequestFulfilled event shape the DON
# emits when fulfilling (FunctionsCoordinator):
#   event RequestFulfilled(bytes32 indexed requestId, uint256 indexed
#     subscriptionId, bytes32 data, uint256 gasUsedAndChainIdCode,
#     uint256 gasUsed)
# gasUsed is derived deterministically; gasUsedAndChainIdCode packs
# chainId << 64 | gasUsed (the coordinator's packed encoding). Both large
# uints serialize as strings.
def _fulfill_event(doc, result):
    rid = _to_int(doc.get("requestID", "0"))
    gas_used = 60 * 1000 + (rid % (40 * 1000))
    chain = _chain_id(doc.get("network", "ethereum"))
    packed = chain
    for i in range(64):
        packed = packed * 2
    packed = packed + gas_used
    return {
        "name": "RequestFulfilled",
        "requestId": "0x" + _hex_pad(rid, 64),
        "subscriptionId": doc.get("subscriptionId", 0),
        "data": result,
        "gasUsed": gas_used,
        "gasUsedAndChainIdCode": str(packed),
    }

# _request_view returns the public request shape (internal _-prefixed
# timing/failure fields are stripped).
def _request_view(doc):
    status = doc.get("status", "queued")
    out = {
        "requestID": doc.get("requestID", ""),
        "donId": doc.get("donId", ""),
        "subscriptionId": doc.get("subscriptionId", 0),
        "status": status,
        "encryptedSecrets": doc.get("encryptedSecrets", ""),
        "gasLimit": doc.get("gasLimit", 500 * 1000),
    }
    if status == "fulfilled":
        out["result"] = doc.get("result", "")
        out["fulfillEvent"] = doc.get("fulfillEvent", {})
        out["completedAt"] = doc.get("completedAt", 0)
    elif status == "failed":
        out["fulfillmentCode"] = doc.get("fulfillmentCode", 2)
        out["fulfillmentCodeName"] = doc.get("fulfillmentCodeName", "FULFILLMENT_CODE_COMPUTED_FAILED")
        out["errorMessage"] = doc.get("errorMessage", "")
        out["completedAt"] = doc.get("completedAt", 0)
    return out
