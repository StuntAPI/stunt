# Shared library for revenuecat-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        return ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _require_auth validates that a non-empty bearer key is present.
# Returns None if authorized, or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return respond(401, {
            "code": 401,
            "message": "Missing API key in Authorization header.",
        })
    return None

# _get_or_create_subscriber fetches the subscriber doc for app_user_id,
# creating a default empty one if it does not exist. Returns the doc dict.
def _get_or_create_subscriber(app_user_id):
    c = store_collection("subscribers")
    doc = c.get(app_user_id)
    if doc != None:
        return doc
    doc = _default_subscriber(app_user_id)
    c.insert(doc)
    return doc

# _default_subscriber builds the default subscriber document with empty
# entitlements, subscriptions, and non_subscriptions maps.
def _default_subscriber(app_user_id):
    return {
        "id": app_user_id,
        "app_user_id": app_user_id,
        "entitlements": {},
        "subscriptions": {},
        "non_subscriptions": {},
    }

# _subscriber_response wraps a subscriber doc in the RevenueCat response
# shape: {subscriber: {...}}.
def _subscriber_response(doc):
    sub = {
        "subscriptions": doc.get("subscriptions", {}),
        "non_subscriptions": doc.get("non_subscriptions", {}),
        "entitlements": doc.get("entitlements", {}),
    }
    return respond(200, {"subscriber": sub})

# _grant_entitlement adds an entitlement to a subscriber doc and persists
# the update. entitlement_id maps to product_id with purchase + expiration
# dates.
def _grant_entitlement(app_user_id, entitlement_id, product_id):
    c = store_collection("subscribers")
    doc = _get_or_create_subscriber(app_user_id)
    ent = doc.get("entitlements", {})
    ent[entitlement_id] = {
        "entitlement_id": entitlement_id,
        "product_id": product_id,
        "purchase_date": "2024-01-01T00:00:00Z",
        "expiration_date": "2099-12-31T23:59:59Z",
    }
    doc["entitlements"] = ent
    c.update(app_user_id, doc)

# _now_iso returns a synthetic ISO 8601 timestamp (stable across calls).
def _now_iso():
    return "2024-01-01T00:00:00Z"
