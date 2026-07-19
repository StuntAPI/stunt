# Record handlers — createRecord + deleteRecord (AT Protocol repo writes).
#
# POST /xrpc/com.atproto.repo.createRecord (Bearer)
#   body: { repo: <did>, collection: <nsid>, record: { ... } }
#   -> 200 { uri: "at://<did>/<collection>/<rkey>", cid }
#
# POST /xrpc/com.atproto.repo.deleteRecord (Bearer)
#   body: { repo: <did>, collection: <nsid>, rkey: <rkey> }
#   -> 200 {}
#
# Auth: the Bearer token's DID must match the request's repo field. This
# mirrors ***REMOVED***'s bluesky adapter, which creates a session first, then
# sends createRecord with repo = session.did.

# Shared helpers (_did_for_token, _mint_cid, _pad12) are preloaded from
# scripts/lib.star.

# on_create_record creates a record in the authenticated repo.
def on_create_record(req):
    did = _did_for_token(req)
    if did == "":
        return respond(401, {
            "error": "AuthRequired",
            "message": "Bearer access token required",
        })

    body = req["body"]
    if body == None:
        body = {}

    repo = body.get("repo", "")
    if repo != did:
        return respond(401, {
            "error": "InvalidRequest",
            "message": "repo must match the authenticated session",
        })

    collection = body.get("collection", "")
    if collection == "":
        return respond(400, {
            "error": "InvalidRequest",
            "message": "collection is required",
        })

    seq = store_kv_incr("bluesky", "rkey_seq")
    rkey = "3k" + _pad12(seq)
    uri = "at://" + did + "/" + collection + "/" + rkey
    cid = _mint_cid(seq)

    record = body.get("record", {})

    pc = store_collection("posts")
    pc.insert({
        "id": uri,
        "uri": uri,
        "cid": cid,
        "repo": did,
        "collection": collection,
        "rkey": rkey,
        "record": record,
    })

    return respond(200, {"uri": uri, "cid": cid})

# on_delete_record removes a record from the authenticated repo.
def on_delete_record(req):
    did = _did_for_token(req)
    if did == "":
        return respond(401, {
            "error": "AuthRequired",
            "message": "Bearer access token required",
        })

    body = req["body"]
    if body == None:
        body = {}

    repo = body.get("repo", "")
    if repo != did:
        return respond(401, {
            "error": "InvalidRequest",
            "message": "repo must match the authenticated session",
        })

    collection = body.get("collection", "")
    rkey = body.get("rkey", "")
    uri = "at://" + did + "/" + collection + "/" + rkey

    pc = store_collection("posts")
    doc = pc.get(uri)
    if doc == None:
        return respond(200, {})  # idempotent delete

    pc.delete(uri)
    return respond(200, {})
