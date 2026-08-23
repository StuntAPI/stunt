# Pinning handlers — pinFileToIPFS, pinJSONToIPFS, unpin.
#
# STATEFUL: pins are stored in the "pins" collection keyed by CID.
#
# POST /pinning/pinFileToIPFS  (multipart)  → { IpfsHash, PinSize, Timestamp, isDuplicate }
# POST /pinning/pinJSONToIPFS  ({pinataContent}) → { IpfsHash, PinSize, Timestamp }
# DELETE /pinning/unpin/{cid}              → 200 OK

# on_pin_file handles multipart file upload pinning. The file part's bytes
# become the pin's size and CID; a pinataMetadata form field names it.
def on_pin_file(req):
    err = _require_auth(req)
    if err != None:
        return err

    name = "pin-file"
    data = None
    raw = req.get("raw_body")
    if raw == None:
        raw = ""
    parts, perr = parse_multipart(_header(req, "Content-Type"), raw)
    if perr == None:
        for p in parts:
            if p["filename"] != None:
                data = p["data"]
            elif p["name"] == "pinataMetadata":
                meta = json_safe_decode(p["data"])
                if meta != None and meta.get("name") != None:
                    name = meta.get("name")

    # Real API rejects an upload with no file part.
    if data == None:
        return _p_err(400, "BAD_REQUEST", "No file was detected in the request")

    cid = _cid_for(data)

    # Identical bytes are already pinned: report the duplicate, no new row.
    existing = _pin_for(cid)
    if existing != None:
        return respond(200, _pin_result(existing, True))

    ts = _timestamp()
    doc = {
        "id": _pin_id(),
        "ipfs_pin_hash": cid,
        "size": len(data),
        "date_pinned": ts,
        "timestamp": ts,
        "metadata": {"name": name},
        "is_duplicate": False,
    }
    store_collection("pins").insert(doc)
    return respond(200, _pin_result(doc, False))

# on_pin_json handles JSON pinning. PinSize/CID derive from the serialized
# pinataContent (compact, key-sorted json.encode — deterministic).
def on_pin_json(req):
    err = _require_auth(req)
    if err != None:
        return err

    # The engine hands a missing JSON body over as an empty dict, not None.
    body = req.get("body")
    if body == None or len(body) == 0:
        return _p_err(400, "BAD_REQUEST", "Request body is required")

    # Pinata wraps content in pinataContent; fall back to body itself.
    content = body.get("pinataContent")
    if content == None:
        content = body

    # Extract name from pinataMetadata if present.
    name = "pin-json"
    meta = body.get("pinataMetadata")
    if meta != None:
        mn = meta.get("name")
        if mn != None:
            name = mn

    data = json.encode(content)
    cid = _cid_for(data)

    # Identical content is already pinned: report the duplicate, no new row.
    existing = _pin_for(cid)
    if existing != None:
        return respond(200, _pin_result(existing, True))

    ts = _timestamp()
    doc = {
        "id": _pin_id(),
        "ipfs_pin_hash": cid,
        "size": len(data),
        "date_pinned": ts,
        "timestamp": ts,
        "metadata": {"name": name},
        "is_duplicate": False,
    }
    store_collection("pins").insert(doc)
    return respond(200, _pin_result(doc, False))

# on_unpin removes a pin by CID.
def on_unpin(req):
    err = _require_auth(req)
    if err != None:
        return err

    cid = req["params"].get("cid", "")
    if cid == None or cid == "":
        return _p_err(400, "BAD_REQUEST", "CID path parameter is required")

    doc = _pin_for(cid)
    if doc != None:
        store_collection("pins").delete(doc.get("id", ""))
        return respond(200, {})

    # Pinata returns 403 when unpinning a CID that isn't pinned.
    return _p_err(403, "FORBIDDEN", "CID not pinned to this account")
