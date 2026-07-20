# Data handlers — pinList, testAuthentication, pinByHash.
#
# GET /data/testAuthentication → { message: "Congratulations! ..." }
# GET /data/pinList            → { count, rows: [...] }
# GET /data/pinByHash          → { count, rows: [...] } (filter by hash query)

# on_test_auth returns the Pinata authentication-success message.
def on_test_auth(req):
    err = _require_auth(req)
    if err != None:
        return err
    return respond(200, {
        "message": "Congratulations! You are communicating with the Pinata API!",
    })

# on_pin_list lists all pins with Pinata's { count, rows } envelope.
def on_pin_list(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("pins")
    docs = c.list()

    # Build rows using the Pinata pin-list shape.
    rows = []
    for doc in docs:
        rows.append(_pin_row(doc))

    return respond(200, {
        "count": len(rows),
        "rows": rows,
    })

# on_pin_by_hash looks up a pin by its hash (cid query parameter).
def on_pin_by_hash(req):
    err = _require_auth(req)
    if err != None:
        return err

    cid = req["query"].get("hash", "")
    if cid == None:
        cid = ""

    c = store_collection("pins")
    docs = c.list()

    rows = []
    for doc in docs:
        if cid == "" or doc.get("ipfs_pin_hash", "") == cid:
            rows.append(_pin_row(doc))

    return respond(200, {
        "count": len(rows),
        "rows": rows,
    })
