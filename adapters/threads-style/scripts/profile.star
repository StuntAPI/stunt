# Profile handler — GET /v1.0/me (Bearer presence required).
#
# Returns the static mock profile. The token is NOT validated — only its
# presence is checked (token-PRESENCE policy).

# Shared helper (_bearer_present) is preloaded from scripts/lib.star.

# on_profile returns the mock profile.
def on_profile(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "code": 190}})
    return respond(200, {
        "id": "u_me",
        "username": "mock_user_me",
        "threads_profile_picture_path": "https://mock-threads.example/pic/me.jpg",
        "threads_biography": "building in public",
    })
