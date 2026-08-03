# Uploads handler — raw octet-stream → uploadToken.
#
# POST /v1/uploads (Bearer; raw binary body) -> plain-text uploadToken
#
# The Google Photos Library API uses a two-step upload:
#   1. POST /v1/uploads with raw binary → returns an uploadToken (plain text)
#   2. POST /v1/mediaItems:batchCreate with the uploadToken → creates a mediaItem
#
# This handler mints an uploadToken AND stores the raw request bytes in the
# blob store keyed by that token, so batchCreate can link the bytes to the
# created media item and /v1/media-dl/{id} can serve them back byte-exact.
# The request Content-Type is recorded alongside the token.

# Shared helpers (_bearer, _user_for_token) are preloaded from scripts/lib.star.

# on_uploads mints an uploadToken and stores the uploaded bytes.
def on_uploads(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    seq = store_kv_incr("photos", "upload_seq")
    # Generated id only (blob names must be [A-Za-z0-9][A-Za-z0-9._-]*).
    token = "CAISI" + str(seq) + "mockUploadToken"

    content = req["raw_body"]
    if content == None:
        content = ""
    content_type = req["headers"].get("Content-Type", "")
    if content_type == "":
        content_type = "application/octet-stream"

    b = store_blob("photos")
    b.put(token, content, content_type)

    utc = store_collection("upload_tokens")
    utc.insert({
        "id": token,
        "user": user["sub"],
        "content_type": content_type,
    })

    # uploadToken is returned as plain text (not JSON).
    return respond(200, token)
