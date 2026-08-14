# Profile handler — GET /v1.0/me (valid Bearer required).
#
# Returns the static mock profile. The token must be known and unexpired
# (minted by the OAuth flow); otherwise 401 code 190.

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
