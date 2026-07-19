# Profile handler — getProfile.
#
# GET /xrpc/app.bsky.actor.getProfile?actor=<did>
#   -> 200 { did, handle, displayName, description, avatar, ... }
#
# Returns the profile for a DID (from the sessions store) or a minimal
# synthetic profile if the DID is not known.

# Shared helpers (_to_int) are preloaded from scripts/lib.star.

# on_get_profile returns the actor profile for a DID.
def on_get_profile(req):
    actor = req["query"].get("actor", "")
    if actor == "":
        return respond(400, {
            "error": "InvalidRequest",
            "message": "actor query parameter is required",
        })

    sc = store_collection("sessions")
    for doc in sc.list():
        if doc.get("did", "") == actor:
            return respond(200, _profile_for(doc))

    return respond(200, _profile_for({
        "did": actor,
        "handle": "unknown.test",
    }))

def _profile_for(doc):
    did = doc.get("did", "")
    handle = doc.get("handle", "")
    seq = _seq_from_did(did)
    return {
        "did": did,
        "handle": handle,
        "displayName": "Mock User " + str(seq),
        "description": "Synthetic Bluesky profile for local testing.",
        "avatar": "https://mock-bsky.example/avatar/" + did + ".jpg",
        "followersCount": seq * 3,
        "followsCount": seq * 2,
        "postsCount": seq,
        "indexedAt": "2024-01-15T00:00:00.000Z",
    }

# _seq_from_did extracts the trailing numeric part from a did:plc:<padded> DID.
def _seq_from_did(did):
    # did:plc:000000000001 -> 1
    prefix = "did:plc:"
    if len(did) > len(prefix) and did[:len(prefix)] == prefix:
        return _to_int(did[len(prefix):])
    return 0
