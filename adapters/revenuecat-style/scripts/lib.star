# Shared library for revenuecat-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins - without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.
#
# Fidelity note: response shapes mirror the REAL RevenueCat REST v1 subscriber
# object so client code tested here also works against the live API:
#   envelope       -> {request_date, request_date_ms, subscriber}
#   entitlement    -> {expires_date, grace_period_expires_date, product_identifier, purchase_date}
#   subscription   -> {expires_date, grace_period_expires_date, product_identifier, purchase_date,
#                      store, period_type, ownership_type, is_sandbox, unsubscribe_detected_at,
#                      billing_issues_detected_at}
#   non_subscription -> keyed by product id -> [ {id, is_sandbox, purchase_date, store} ]
# An entitlement is ACTIVE when expires_date is null (lifetime, e.g. a one-time
# purchase) or a future timestamp; a past expires_date is expired/inactive.

# Deterministic synthetic timestamps (stunt values reproducibility over wall clock).
_REQUEST_DATE = "2024-01-01T00:00:00Z"
_REQUEST_DATE_MS = 1704067200000
_PURCHASE_DATE = "2024-01-01T00:00:00Z"
_FAR_FUTURE = "2099-12-31T23:59:59Z"

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

# _require_auth validates that a non-empty bearer key is present. RevenueCat
# accepts secret (sk_) and public (appl_/goog_/...) keys; the mock accepts any
# non-empty key. Returns None if authorized, or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return respond(401, {
            "code": 7225,
            "message": "Invalid API Key. Please see https://docs.revenuecat.com/docs/authentication",
        })
    return None

# _get_or_create_subscriber fetches the subscriber doc for app_user_id,
# creating a default empty one if it does not exist. Returns the doc dict.
# RevenueCat's GET is get-or-create, so an unknown user is a valid empty
# subscriber, not a 404.
def _get_or_create_subscriber(app_user_id):
    c = store_collection("subscribers")
    doc = c.get(app_user_id)
    if doc != None:
        return doc
    doc = _default_subscriber(app_user_id)
    c.insert(doc)
    return doc

# _default_subscriber builds a default subscriber document. `id` is the internal
# collection key and is stripped from the wire response (the real API has no
# top-level subscriber id; it uses original_app_user_id).
def _default_subscriber(app_user_id):
    return {
        "id": app_user_id,
        "original_app_user_id": app_user_id,
        "first_seen": _REQUEST_DATE,
        "last_seen": _REQUEST_DATE,
        "management_url": None,
        "original_application_version": None,
        "original_purchase_date": None,
        "entitlements": {},
        "subscriptions": {},
        "non_subscriptions": {},
        "other_purchases": {},
    }

# _subscriber_response wraps a subscriber doc in the real RevenueCat envelope
# {request_date, request_date_ms, subscriber:{...}}, projecting only the wire
# fields (drops the internal `id` key).
def _subscriber_response(doc):
    sub = {
        "original_app_user_id": doc.get("original_app_user_id", doc.get("id", "")),
        "first_seen": doc.get("first_seen", _REQUEST_DATE),
        "last_seen": doc.get("last_seen", _REQUEST_DATE),
        "management_url": doc.get("management_url", None),
        "original_application_version": doc.get("original_application_version", None),
        "original_purchase_date": doc.get("original_purchase_date", None),
        "entitlements": doc.get("entitlements", {}),
        "subscriptions": doc.get("subscriptions", {}),
        "non_subscriptions": doc.get("non_subscriptions", {}),
        "other_purchases": doc.get("other_purchases", {}),
    }
    return respond(200, {
        "request_date": _REQUEST_DATE,
        "request_date_ms": _REQUEST_DATE_MS,
        "subscriber": sub,
    })

# _grant_entitlement adds an entitlement to a subscriber and persists it.
# expires_date == None grants a LIFETIME entitlement (one-time purchase): the
# entitlement carries expires_date null and the product is recorded under
# non_subscriptions. A non-None expires_date grants a time-limited SUBSCRIPTION
# recorded under subscriptions. Either way the entitlement points at
# product_identifier, matching the real API.
def _grant_entitlement(app_user_id, entitlement_id, product_identifier, expires_date):
    c = store_collection("subscribers")
    doc = _get_or_create_subscriber(app_user_id)

    ent = doc.get("entitlements", {})
    ent[entitlement_id] = {
        "expires_date": expires_date,
        "grace_period_expires_date": None,
        "product_identifier": product_identifier,
        "purchase_date": _PURCHASE_DATE,
    }
    doc["entitlements"] = ent

    if expires_date == None:
        # Lifetime / consumable: record under non_subscriptions (product id -> list).
        nons = doc.get("non_subscriptions", {})
        purchases = nons.get(product_identifier, [])
        purchases.append({
            "id": "txn_" + str(store_kv_incr("revenuecat", "txn_seq")),
            "is_sandbox": True,
            "purchase_date": _PURCHASE_DATE,
            "store": "app_store",
        })
        nons[product_identifier] = purchases
        doc["non_subscriptions"] = nons
    else:
        subs = doc.get("subscriptions", {})
        subs[product_identifier] = {
            "expires_date": expires_date,
            "grace_period_expires_date": None,
            "product_identifier": product_identifier,
            "purchase_date": _PURCHASE_DATE,
            "original_purchase_date": _PURCHASE_DATE,
            "store": "app_store",
            "period_type": "normal",
            "ownership_type": "PURCHASED",
            "is_sandbox": True,
            "unsubscribe_detected_at": None,
            "billing_issues_detected_at": None,
        }
        doc["subscriptions"] = subs

    c.update(app_user_id, doc)
