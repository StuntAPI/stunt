# Identity handler — resolveHandle.
#
# GET /xrpc/com.atproto.identity.resolveHandle?handle=<handle>
#   -> 200 { did }
#
# Resolves a handle to its DID by looking up sessions created via
# createSession (which mints a DID per identifier).

# Shared helpers are preloaded from scripts/lib.star.

# on_resolve_handle resolves a handle to a DID.
def on_resolve_handle(req):
    handle = req["query"].get("handle", "")
    if handle == "":
        return respond(400, {
            "error": "InvalidRequest",
            "message": "handle query parameter is required",
        })

    sc = store_collection("sessions")
    for doc in sc.list():
        if doc.get("handle", "") == handle:
            return respond(200, {"did": doc.get("did", "")})

    return respond(404, {
        "error": "NotFound",
        "message": "handle not found",
    })
