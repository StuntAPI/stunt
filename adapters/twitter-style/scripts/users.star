# User handlers — synthetic user data backed by the "users" collection.
#
# The current user (usr_me) is seeded in the users collection and also
# hardcoded here as a fallback for the /me endpoint.

# _now is preloaded from scripts/lib.star.

# _CURRENT_USER is the synthetic "current user" for this local session.
# The same user is seeded in fixtures/users.jsonl so that /2/users/{id}
# and /2/users/by/username/{username} also return it consistently.
_CURRENT_USER = {
    "id": "usr_me",
    "name": "Local Test User",
    "username": "local_test_user",
    "public_metrics": {
        "followers_count": 42,
        "following_count": 17,
        "tweet_count": 3,
    },
    "created_at": _now(),
}

# GET /2/users/me — return the current synthetic user.
def on_me(req):
    # Try the collection first (seeded data), fall back to hardcoded.
    c = store_collection("users")
    doc = c.get("usr_me")
    if doc != None:
        return respond(200, {"data": doc})
    return respond(200, {"data": _CURRENT_USER})

# GET /2/users/{id} — show a user by ID.
def on_show(req):
    id = req["params"]["id"]
    c = store_collection("users")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"detail": "User not found: " + id, "title": "Not Found", "type": "about:blank"}})
    return respond(200, {"data": doc})

# GET /2/users/by/username/{username} — lookup by username.
def on_lookup(req):
    username = req["params"]["username"]
    c = store_collection("users")
    docs = c.list()
    for doc in docs:
        if doc.get("username", None) == username:
            return respond(200, {"data": doc})
    return respond(404, {"error": {"detail": "User not found: " + username, "title": "Not Found", "type": "about:blank"}})
