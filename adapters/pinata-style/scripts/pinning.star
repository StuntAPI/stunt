# Pinning handlers — pinFileToIPFS, pinJSONToIPFS, unpin.
#
# STATEFUL: pins are stored in the "pins" collection keyed by CID.
#
# POST /pinning/pinFileToIPFS  (multipart)  → { IpfsHash, PinSize, Timestamp, isDuplicate }
# POST /pinning/pinJSONToIPFS  ({pinataContent}) → { IpfsHash, PinSize, Timestamp }
# DELETE /pinning/unpin/{cid}              → 200 OK

# on_pin_file handles multipart file upload pinning.
def on_pin_file(req):
    err = _require_auth(req)
    if err != None:
        return err

    cid = _cid_gen()
    row_id = _pin_id()
    ts = _timestamp()

    # Determine a synthetic pin size. For multipart uploads the body is not
    # parsed as JSON (body will be nil); we use a deterministic default.
    pin_size = 1024

    # Extract a name from pinataMetadata if present in the body.
    name = "pin-file"
    body = req.get("body")
    if body != None:
        meta = body.get("pinataMetadata")
        if meta != None:
            mn = meta.get("name")
            if mn != None:
                name = mn

    doc = {
        "id": row_id,
        "ipfs_pin_hash": cid,
        "size": pin_size,
        "date_pinned": ts,
        "timestamp": ts,
        "metadata": {"name": name},
        "is_duplicate": False,
    }

    c = store_collection("pins")
    c.insert(doc)

    return respond(200, _pin_result(doc))

# on_pin_json handles JSON pinning.
def on_pin_json(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        return _p_err(400, "BAD_REQUEST", "Request body is required")

    # Pinata wraps content in pinataContent; fall back to body itself.
    content = body.get("pinataContent")
    if content == None:
        content = body

    # Synthetic pin size based on content.
    pin_size = 512

    # Extract name from pinataMetadata if present.
    name = "pin-json"
    meta = body.get("pinataMetadata")
    if meta != None:
        mn = meta.get("name")
        if mn != None:
            name = mn

    cid = _cid_gen()
    row_id = _pin_id()
    ts = _timestamp()

    doc = {
        "id": row_id,
        "ipfs_pin_hash": cid,
        "size": pin_size,
        "date_pinned": ts,
        "timestamp": ts,
        "metadata": {"name": name},
        "is_duplicate": False,
    }

    c = store_collection("pins")
    c.insert(doc)

    return respond(200, _pin_result(doc))

# on_unpin removes a pin by CID.
def on_unpin(req):
    err = _require_auth(req)
    if err != None:
        return err

    cid = req["params"].get("cid", "")
    if cid == None or cid == "":
        return _p_err(400, "BAD_REQUEST", "CID path parameter is required")

    c = store_collection("pins")
    docs = c.list()
    for doc in docs:
        if doc.get("ipfs_pin_hash", "") == cid:
            c.delete(doc.get("id", ""))
            return respond(200, {})

    # Pinata returns 403 when unpinning a CID that isn't pinned.
    return _p_err(403, "FORBIDDEN", "CID not pinned to this account")
