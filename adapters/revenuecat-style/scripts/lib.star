# Shared library for revenuecat-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# One day in seconds, assembled at runtime (no long digit runs in source).
_DAY_SECONDS = 24 * 60 * 60

# Well-known static test API key, seeded once into the KV store on first
# request (see _seed_tokens) so existing clients/tests that use it keep
# working while any other key is rejected with 401. This mirrors
# RevenueCat's public "sk_" SDK keys.
_TEST_API_KEY = "sk_test_revenuecat_style_mock_key"

# _seed_tokens inserts the well-known test key into the KV store exactly once
# per instance (guarded by the "auth_seeded" flag), stored under
# "tok:<key>" with a far-future expiry computed at runtime (never a
# hardcoded epoch).
def _seed_tokens():
    if store_kv_get("revenuecat", "auth_seeded") == "yes":
        return
    store_kv_set("revenuecat", "auth_seeded", "yes")
    exp = str(clock.now_unix() + 3600 * 24 * 365 * 10)
    store_kv_set("revenuecat", "tok:" + _TEST_API_KEY, exp)

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

# _require_auth validates the Authorization: Bearer <key> header against the
# KV token store. Returns None if the key is known and unexpired, or a 401
# error-response dict if missing, malformed, unknown, or expired.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return respond(401, {
            "code": 401,
            "message": "Missing API key in Authorization header.",
        })
    _seed_tokens()
    exp = store_kv_get("revenuecat", "tok:" + token)
    if exp == None or clock.now_unix() > _to_int(exp):
        return respond(401, {
            "code": 401,
            "message": "Invalid API key.",
        })
    return None

# ============================================================================
# PRODUCT CATALOG (synthetic, real-shaped)
# ============================================================================
# Subscription products carry REAL expiry math: a period in days plus an
# optional intro/trial period in days granted on the very first purchase of
# that product. Non-subscription products (consumables) land in
# subscriber.non_subscriptions instead of entitlements/subscriptions.

_PRODUCTS = {
    "premium": {
        "kind": "subscription",
        "entitlement": "pro",
        "period_days": 30,
        "trial_days": 7,
        "price": 9.99,
        "currency": "USD",
    },
    "pro": {
        "kind": "subscription",
        "entitlement": "pro",
        "period_days": 365,
        "trial_days": 0,
        "price": 99.99,
        "currency": "USD",
    },
    "gold_coins": {
        "kind": "non_subscription",
        "entitlement": "gold_coins",
        "period_days": 0,
        "trial_days": 0,
        "price": 4.99,
        "currency": "USD",
    },
}

# _product returns the catalog entry for a product id, falling back to a
# generic monthly subscription whose entitlement id equals the product id.
def _product(product_id):
    p = _PRODUCTS.get(product_id)
    if p != None:
        return p
    return {
        "kind": "subscription",
        "entitlement": product_id,
        "period_days": 30,
        "trial_days": 0,
        "price": 9.99,
        "currency": "USD",
    }

# _store_for_platform maps a receipt platform to RevenueCat's store codes.
def _store_for_platform(platform):
    if platform == "ios":
        return "app_store"
    return "play_store"

# ============================================================================
# SUBSCRIBER DOCUMENTS
# ============================================================================

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
# entitlements, subscriptions, non_subscriptions, and attributes maps.
def _default_subscriber(app_user_id):
    return {
        "id": app_user_id,
        "app_user_id": app_user_id,
        "original_app_user_id": app_user_id,
        "first_seen": clock.now_rfc3339(),
        "entitlements": {},
        "subscriptions": {},
        "non_subscriptions": {},
        "attributes": {},
    }

# _subscriber_view projects a stored subscriber doc onto the public
# RevenueCat schema:
#   entitlements.{id}: {expires_date, product_identifier, purchase_date}
#   subscriptions.{id}: {product_identifier, purchase_date, expires_date,
#                        period_type, store, original_purchase_date, ...}
#   non_subscriptions.{id}: [{id, purchase_date, product_id}, ...]
def _subscriber_view(doc):
    ents = {}
    for k in doc.get("entitlements", {}):
        ents[k] = _strip_map_scalars(doc["entitlements"][k])
    subs = {}
    for k in doc.get("subscriptions", {}):
        subs[k] = _strip_map_scalars(doc["subscriptions"][k])
    nons = {}
    for k in doc.get("non_subscriptions", {}):
        items = []
        for it in doc["non_subscriptions"][k]:
            items.append(_strip_map_scalars(it))
        nons[k] = items
    return {
        "original_app_user_id": doc.get("original_app_user_id", doc.get("app_user_id", "")),
        "first_seen": doc.get("first_seen", ""),
        "entitlements": ents,
        "subscriptions": subs,
        "non_subscriptions": nons,
        "attributes": _strip_map_scalars(doc.get("attributes", {})),
    }

# _strip_map_scalars strips internal keys from a flat map (one level deep —
# entitlement/subscription entries and non-subscription purchase records are
# flat by schema).
def _strip_map_scalars(m):
    out = {}
    for k in m:
        if not k.startswith("_"):
            out[k] = m[k]
    return out

# _subscriber_response wraps the public view in RevenueCat's response shape:
# {subscriber: {...}}.
def _subscriber_response(doc):
    return respond(200, {"subscriber": _subscriber_view(doc)})

# ============================================================================
# EXPIRATION (derive-on-read state machine)
# ============================================================================
# Real RevenueCat removes an entitlement from the subscriber once its period
# lapses and fires an EXPIRATION webhook. stunt derives that on read: any
# handler that touches a subscriber doc first calls _refresh_subscriber,
# which checks each subscription's internal _expires_at against the clock,
# drops the entitlements it granted, marks the subscription expired, persists
# the transition, and emits the EXPIRATION event exactly once per lapsed
# subscription (guarded by the internal _exp_emitted flag).

def _refresh_subscriber(doc):
    subs = doc.get("subscriptions", {})
    now = clock.now_unix()
    changed = False
    dead_products = []
    for pid in subs:
        sub = subs[pid]
        exp = _num(sub.get("_expires_at", 0))
        if exp > 0 and exp <= now and sub.get("_exp_emitted", False) == False:
            sub["expires_date"] = clock.unix_to_rfc3339(exp)
            sub["is_active"] = False
            sub["_exp_emitted"] = True
            dead_products.append(pid)
            changed = True
    if len(dead_products) == 0:
        return doc
    ents = doc.get("entitlements", {})
    kept = {}
    for eid in ents:
        ent = ents[eid]
        if ent.get("product_identifier", "") in dead_products:
            continue
        kept[eid] = ent
    doc["entitlements"] = kept
    # Persist BEFORE emitting: a crash mid-loop must not re-emit.
    c = store_collection("subscribers")
    c.update(doc.get("app_user_id", ""), doc)
    for pid in dead_products:
        sub = subs[pid]
        _emit_event({
            "type": "EXPIRATION",
            "app_user_id": doc.get("app_user_id", ""),
            "original_app_user_id": doc.get("original_app_user_id", ""),
            "product_id": pid,
            "entitlement_id": sub.get("_entitlement_id", ""),
            "period_type": sub.get("period_type", "NORMAL"),
            "store": sub.get("store", "app_store"),
            "expired_at": clock.now_rfc3339(),
            "expiration_at_ms": _num(sub.get("_expires_at", 0)) * 1000,
            "environment": "SANDBOX",
        })
    return doc

# ============================================================================
# PURCHASES (receipts -> entitlements/subscriptions/non_subscriptions)
# ============================================================================

# _apply_purchase processes one validated receipt against a subscriber doc:
#
#   - subscription product, no active sub  -> INITIAL_PURCHASE (period_type
#     TRIAL when the product has an intro/trial period and the user has
#     never purchased it before, else NORMAL); expiry = now + trial|period.
#   - subscription product, active sub     -> RENEWAL; expiry = current
#     expiry + period (real RC stacks renewal time onto unexpired time).
#   - non-subscription product             -> NON_RENEWING_PURCHASE,
#     appended to non_subscriptions.{product_id}.
#
# Returns the emitted event dict (already emitted).
def _apply_purchase(doc, product_id, platform):
    prod = _product(product_id)
    store = _store_for_platform(platform)
    now = clock.now_unix()
    now_iso = clock.now_rfc3339()
    ent_id = prod["entitlement"]

    if prod["kind"] == "non_subscription":
        nons = doc.get("non_subscriptions", {})
        lst = nons.get(product_id)
        if lst == None:
            lst = []
        purchase = {
            "id": "rc_" + str(store_kv_incr("revenuecat", "purchase_seq")),
            "purchase_date": now_iso,
            "product_id": product_id,
        }
        lst.append(purchase)
        nons[product_id] = lst
        doc["non_subscriptions"] = nons
        return _emit_event({
            "type": "NON_RENEWING_PURCHASE",
            "app_user_id": doc.get("app_user_id", ""),
            "original_app_user_id": doc.get("original_app_user_id", ""),
            "product_id": product_id,
            "entitlement_id": None,
            "store": store,
            "purchased_at_ms": now * 1000,
            "environment": "SANDBOX",
            "price": prod["price"],
            "currency": prod["currency"],
        })

    subs = doc.get("subscriptions", {})
    sub = subs.get(product_id)
    active = sub != None and _num(sub.get("_expires_at", 0)) > now
    purchased_at = now
    if active:
        # RENEWAL: extend from the unexpired remainder.
        expires = _num(sub.get("_expires_at", 0)) + prod["period_days"] * _DAY_SECONDS
        period_type = "NORMAL"
        event_type = "RENEWAL"
        original_purchase_date = sub.get("original_purchase_date", now_iso)
    else:
        trial = prod["trial_days"] > 0 and sub == None
        if trial:
            expires = now + prod["trial_days"] * _DAY_SECONDS
            period_type = "TRIAL"
        else:
            expires = now + prod["period_days"] * _DAY_SECONDS
            period_type = "NORMAL"
        event_type = "INITIAL_PURCHASE"
        if sub != None:
            original_purchase_date = sub.get("original_purchase_date", now_iso)
        else:
            original_purchase_date = now_iso
    expires_iso = clock.unix_to_rfc3339(expires)
    subs[product_id] = {
        "product_identifier": product_id,
        "purchase_date": now_iso,
        "original_purchase_date": original_purchase_date,
        "expires_date": expires_iso,
        "period_type": period_type,
        "store": store,
        "is_active": True,
        "auto_renewal_status": 1,
        "_expires_at": expires,
        "_entitlement_id": ent_id,
    }
    doc["subscriptions"] = subs
    ents = doc.get("entitlements", {})
    ents[ent_id] = {
        "expires_date": expires_iso,
        "product_identifier": product_id,
        "purchase_date": original_purchase_date,
        "_expires_at": expires,
    }
    doc["entitlements"] = ents
    return _emit_event({
        "type": event_type,
        "app_user_id": doc.get("app_user_id", ""),
        "original_app_user_id": doc.get("original_app_user_id", ""),
        "product_id": product_id,
        "entitlement_id": ent_id,
        "period_type": period_type,
        "store": store,
        "purchased_at_ms": purchased_at * 1000,
        "expiration_at_ms": expires * 1000,
        "environment": "SANDBOX",
        "price": prod["price"],
        "currency": prod["currency"],
    })

# _revoke_subscription models RevenueCat's revoke (store refund): the
# subscription lapses immediately, the entitlement it granted is removed, and
# a CANCELLATION event fires with the mapped cancel_reason.
_CANCEL_REASONS = {
    "refund": "REFUND",
    "cancel_subscription": "VOLUNTARY",
    "billing_error": "BILLING_ERROR",
    "price_increase": "PRICE_INCREASE",
}
def _revoke_subscription(doc, product_id, reason):
    sub = doc.get("subscriptions", {}).get(product_id)
    if sub == None:
        return None
    now = clock.now_unix()
    now_iso = clock.now_rfc3339()
    sub["expires_date"] = now_iso
    sub["is_active"] = False
    sub["auto_renewal_status"] = 0
    sub["_expires_at"] = now
    sub["_exp_emitted"] = True  # already terminal: never also emit EXPIRATION
    cancel_reason = _CANCEL_REASONS.get(reason, "UNKNOW")
    ents = doc.get("entitlements", {})
    kept = {}
    for eid in ents:
        ent = ents[eid]
        if ent.get("product_identifier", "") == product_id:
            continue
        kept[eid] = ent
    doc["entitlements"] = kept
    return _emit_event({
        "type": "CANCELLATION",
        "app_user_id": doc.get("app_user_id", ""),
        "original_app_user_id": doc.get("original_app_user_id", ""),
        "product_id": product_id,
        "entitlement_id": sub.get("_entitlement_id", ""),
        "period_type": sub.get("period_type", "NORMAL"),
        "store": sub.get("store", "app_store"),
        "cancel_reason": cancel_reason,
        "canceled_at_ms": now * 1000,
        "expiration_at_ms": now * 1000,
        "environment": "SANDBOX",
    })

# ============================================================================
# OUTBOUND WEBHOOKS (RevenueCat v1 event envelope)
# ============================================================================
# Real RevenueCat v1 webhooks POST:
#   { "api_version": "1.0", "event": { "type": "...", "app_user_id": "...",
#     "product_id": "...", "purchased_at_ms": ..., "expiration_at_ms": ..., ... } }
# to the URL configured in the dashboard (there is no REST registration
# endpoint and no v1 signing scheme — deliveries are UNSIGNED BY DESIGN;
# RevenueCat's documented guidance is to validate the app_user_id afterwards
# via GET /v1/subscribers/{app_user_id}. RevenueCat's newer v2 webhooks are
# Ed25519-signed, but this adapter simulates the v1 surface).
#
# stunt therefore emits unsigned, with the real v1 envelope carried inside the
# engine's {"type", "payload"} delivery envelope. Timestamps are epoch
# milliseconds minted from the engine clock.

# _emit_event delivers one unsigned v1 webhook event.
def _emit_event(event):
    events_emit(event.get("type", ""), {
        "api_version": "1.0",
        "event": event,
    })

# ============================================================================
# CONVERSION HELPERS
# ============================================================================

# _num coerces a JSON-round-tripped number (int or float) to int.
def _num(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(v)

# _to_int parses a decimal string to int. Returns 0 for None or empty.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n
