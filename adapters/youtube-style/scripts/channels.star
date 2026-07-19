# Channels handler — returns the authenticated user's channel.
#
# GET /youtube/v3/channels?part=snippet&mine=true (Bearer) -> { items: [{ id, snippet }] }

# Shared helpers (_bearer, _user_for_token) are preloaded from
# scripts/lib.star.

# on_channels returns a synthetic channel for the authenticated user.
def on_channels(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    # Derive a stable channel id from the user sub.
    channel_id = "mock-channel-" + user["sub"]

    return respond(200, {
        "items": [
            {
                "id": channel_id,
                "snippet": {
                    "title": user["name"] + " Channel",
                    "description": "A mock YouTube channel for local testing",
                    "publishedAt": "2024-01-15T12:00:00Z",
                },
            },
        ],
    })
