# Uploads handler — raw octet-stream → uploadToken.
#
# POST /v1/uploads (Bearer; raw binary body) -> plain-text uploadToken
#
# The Google Photos Library API uses a two-step upload:
#   1. POST /v1/uploads with raw binary → returns an uploadToken (plain text)
#   2. POST /v1/mediaItems:batchCreate with the uploadToken → creates a mediaItem
#
# This handler mints and stores an uploadToken so that batchCreate can
# reference it. The raw body is not JSON (octet-stream), so req["body"] is
# None — that's expected; we just mint the token.

# Shared helpers (_bearer, _user_for_token) are preloaded from scripts/lib.star.

# on_uploads mints an uploadToken for the uploaded bytes.
def on_uploads(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    seq = store_kv_incr("photos", "upload_seq")
    token = "CAISI" + str(seq) + "mockUploadToken"

    utc = store_collection("upload_tokens")
    utc.insert({
        "id": token,
        "user": user["sub"],
    })

    # uploadToken is returned as plain text (not JSON).
    return respond(200, token)
