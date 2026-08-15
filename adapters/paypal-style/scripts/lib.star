# Shared library for paypal-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# _bearer extracts the Bearer token from the Authorization header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _require_auth validates the bearer token: it must have been minted via
# POST /v1/oauth2/token (stored in the access_tokens collection) and be
# unexpired. Returns None if authorized, or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return _pp_err_simple(401, "AUTHENTICATION_FAILURE", "Authentication failed due to invalid authentication credentials.")
    c = store_collection("access_tokens")
    doc = c.get(token)
    if doc == None:
        return _pp_err_simple(401, "AUTHENTICATION_FAILURE", "Access token does not exist.")
    expires_at = doc.get("expires_at", 0)
    if expires_at == None:
        expires_at = 0
    if type(expires_at) == "float":
        expires_at = int(expires_at)
    if expires_at > 0 and clock.now_unix() > expires_at:
        return _pp_err_simple(401, "AUTHENTICATION_FAILURE", "Access token expired.")
    return None

# _pp_err returns a PayPal-style error response.
def _pp_err(status, name, message, debug_id):
    return respond(status, {
        "name": name,
        "details": [{"issue": "ERROR", "description": message}],
        "message": message,
        "debug_id": debug_id,
    })

# _pp_err_simple returns an error with a synthetic debug_id.
def _pp_err_simple(status, name, message):
    n = store_kv_incr("paypal", "debug_seq")
    return respond(status, {
        "name": name,
        "details": [{"issue": "ERROR", "description": message}],
        "message": message,
        "debug_id": "debug-" + str(n),
    })

# _pp_err_details returns a PayPal error carrying a real details[].issue
# code (ORDER_NOT_APPROVED, PAYER_ACTION_REQUIRED, ...), the shape PayPal
# returns for 4xx business-rule failures.
def _pp_err_details(status, name, issue, message):
    n = store_kv_incr("paypal", "debug_seq")
    return respond(status, {
        "name": name,
        "details": [{"issue": issue, "description": message}],
        "message": message,
        "debug_id": "debug-" + str(n),
    })

# ============================================================================
# MONEY (string decimal amounts, e.g. "10.00" — PayPal's wire format)
# ============================================================================

# _all_digits reports whether s consists only of digits. Starlark strings
# are not iterable (no `for ch in s`), so this uses str.isdigit — which also
# rejects the empty string, mapping inputs like "5." to a 400.
def _all_digits(s):
    if s == "":
        return False
    return s.isdigit()

# _cents parses a PayPal amount string ("10", "10.5", "10.00") into integer
# cents. Returns None for malformed input (callers map that to a 400).
def _cents(v):
    s = "0"
    if v != None:
        s = str(v)
    if s == "":
        s = "0"
    neg = False
    if s.startswith("-"):
        neg = True
        s = s[1:]
    pieces = s.split(".")
    whole = pieces[0]
    frac = "0"
    if len(pieces) > 1:
        frac = pieces[1]
    if whole == "":
        whole = "0"
    if not _all_digits(whole) or not _all_digits(frac):
        return None
    if len(pieces) > 2:
        return None
    if len(frac) == 1:
        frac = frac + "0"
    elif len(frac) > 2:
        frac = frac[:2]
    n = int(whole) * 100 + int(frac)
    if neg:
        n = -n
    return n

# _fmt_cents renders integer cents as a PayPal amount string ("10.00").
def _fmt_cents(n):
    neg = ""
    if n < 0:
        neg = "-"
        n = -n
    d = n // 100
    c = n % 100
    cs = str(c)
    if c < 10:
        cs = "0" + cs
    return neg + str(d) + "." + cs

# _links returns a standard PayPal links array for an order.
def _order_links(order_id, status):
    links = [
        {"href": "https://api.stunt.test/v2/checkout/orders/" + order_id, "rel": "self", "method": "GET"},
    ]
    if status == "CREATED":
        links.append({"href": "https://www.stunt.test/checkoutnow?token=" + order_id, "rel": "approve", "method": "GET"})
        links.append({"href": "https://api.stunt.test/v2/checkout/orders/" + order_id + "/capture", "rel": "capture", "method": "POST"})
    if status == "APPROVED":
        links.append({"href": "https://api.stunt.test/v2/checkout/orders/" + order_id + "/capture", "rel": "capture", "method": "POST"})
        links.append({"href": "https://api.stunt.test/v2/checkout/orders/" + order_id + "/authorize", "rel": "authorize", "method": "POST"})
    return links

# _order_public returns the PayPal-shaped order object.
def _order_public(doc):
    status = doc.get("status", "CREATED")
    result = {
        "id": doc["id"],
        "status": status,
        "intent": doc.get("intent", "CAPTURE"),
        "purchase_units": doc.get("purchase_units", []),
        "create_time": doc.get("create_time", ""),
        "links": _order_links(doc["id"], status),
    }
    return result

# _order_id generates an order ID from the sequence counter.
def _order_id():
    n = store_kv_incr("paypal", "order_seq")
    return "ORDERID-" + str(n)

# _capture_id generates a capture ID from the sequence counter.
def _capture_id():
    n = store_kv_incr("paypal", "capture_seq")
    return "CAPTUREID-" + str(n)

# _refund_id generates a refund ID from the sequence counter.
def _refund_id():
    n = store_kv_incr("paypal", "refund_seq")
    return "REFUNDID-" + str(n)

# _request_id returns or stores the PayPal-Request-Id header for idempotency.
def _get_request_id(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    rid = headers.get("PayPal-Request-Id", "")
    if rid == None:
        rid = ""
    return rid

# _check_idempotency returns a cached response for a PayPal-Request-Id, or None.
def _check_idempotency(req, path):
    rid = _get_request_id(req)
    if rid == "":
        return None
    cached = store_kv_get("paypal", "idem_" + path + "_" + rid)
    if cached != None and cached != "":
        # Return the cached order by ID.
        oc = store_collection("orders")
        return oc.get(cached)
    return None

# _store_idempotency caches an order ID for a PayPal-Request-Id.
def _store_idempotency(req, path, order_id):
    rid = _get_request_id(req)
    if rid == "":
        return
    store_kv_set("paypal", "idem_" + path + "_" + rid, order_id)

# ============================================================================
# OUTBOUND WEBHOOKS (PayPal webhook event envelope, UNSIGNED BY DESIGN)
# ============================================================================
# Real PayPal signs webhook deliveries with a certificate-based scheme
# (PayPal-Transmission-Sig / PayPal-Cert-Url / PayPal-Transmission-Id...),
# verified by calling POST /v1/notifications/verify-webhook-signature against
# the PayPal API with the transmitted headers + webhook ID — not by a shared
# HMAC the receiver can compute itself. The local simulator therefore emits
# UNSIGNED deliveries with the real event envelope, and exposes the verify
# endpoint (always SUCCESS) so client verification flows can be exercised.
#
# Envelope (delivered inside the engine's {"type", "payload"} wrapper):
#   { "id": "WH-N", "event_version": "1.0", "create_time": "...",
#     "resource_type": "capture", "event_type": "PAYMENT.CAPTURE.COMPLETED",
#     "summary": "...", "resource": {...}, "links": [...] }

# _webhook_event_id generates a PayPal-style webhook event id (WH-...).
def _webhook_event_id():
    n = store_kv_incr("paypal", "webhook_event_seq")
    return "WH-" + str(n) + "-STUNT-SIM"

# _emit_event delivers one PayPal webhook notification with the full real
# envelope (unsigned — see the scheme note above).
def _emit_event(event_type, resource_type, summary, resource):
    event = {
        "id": _webhook_event_id(),
        "event_version": "1.0",
        "create_time": clock.now_rfc3339(),
        "resource_type": resource_type,
        "event_type": event_type,
        "summary": summary,
        "resource": resource,
        "links": [{
            "href": "https://api.stunt.test/v1/notifications/webhooks-events/" + event_type,
            "rel": "self",
            "method": "GET",
        }],
    }
    events_emit(event_type, event)

# ============================================================================
# RESOURCE VIEWS (strip internal _ keys)
# ============================================================================

# _capture_public renders a capture from the captures collection.
def _capture_public(doc):
    links = [
        {"href": "https://api.stunt.test/v2/payments/captures/" + doc["id"], "rel": "self", "method": "GET"},
    ]
    if doc.get("authorization_id", "") != "":
        links.append({"href": "https://api.stunt.test/v2/payments/authorizations/" + doc.get("authorization_id", ""), "rel": "up", "method": "GET"})
    elif doc.get("order_id", "") != "":
        links.append({"href": "https://api.stunt.test/v2/checkout/orders/" + doc.get("order_id", ""), "rel": "up", "method": "GET"})
    return {
        "id": doc["id"],
        "status": doc.get("status", "COMPLETED"),
        "amount": doc.get("amount", {}),
        "final_capture": doc.get("final_capture", False),
        "create_time": doc.get("create_time", ""),
        "seller_protection": {"status": "ELIGIBLE", "dispute_categories": ["ITEM_NOT_RECEIVED", "UNAUTHORIZED_TRANSACTION"]},
        "links": links,
    }

# _auth_links returns the action links for an authorization by status.
def _auth_links(auth_id, status):
    base = "https://api.stunt.test/v2/payments/authorizations/" + auth_id
    links = [{"href": base, "rel": "self", "method": "GET"}]
    if status == "CREATED" or status == "AUTHORIZED":
        links.append({"href": base + "/reauthorize", "rel": "reauthorize", "method": "POST"})
        links.append({"href": base + "/void", "rel": "void", "method": "POST"})
        links.append({"href": base + "/capture", "rel": "capture", "method": "POST"})
    return links

# _auth_public renders an authorization from the authorizations collection.
def _auth_public(doc):
    status = doc.get("status", "CREATED")
    out = {
        "id": doc["id"],
        "status": status,
        "amount": doc.get("amount", {}),
        "create_time": doc.get("create_time", ""),
        "seller_protection": {"status": "ELIGIBLE", "dispute_categories": ["ITEM_NOT_RECEIVED", "UNAUTHORIZED_TRANSACTION"]},
        "links": _auth_links(doc["id"], status),
    }
    if doc.get("update_time", "") != "":
        out["update_time"] = doc.get("update_time", "")
    exp = doc.get("_expiration_unix", 0)
    if type(exp) == "float":
        exp = int(exp)
    if exp > 0:
        out["expiration_time"] = clock.unix_to_rfc3339(exp)
    return out

# _refund_public renders a refund from the refunds collection.
def _refund_public(doc):
    out = {
        "id": doc["id"],
        "status": doc.get("status", "PENDING"),
        "amount": doc.get("amount", {}),
        "create_time": doc.get("create_time", ""),
        "links": [{
            "href": "https://api.stunt.test/v2/payments/captures/" + doc.get("capture_id", ""),
            "rel": "up",
            "method": "GET",
        }],
    }
    if doc.get("update_time", "") != "":
        out["update_time"] = doc.get("update_time", "")
    return out

# ============================================================================
# ORDER / AUTHORIZATION / CAPTURE BOOKKEEPING
# ============================================================================

# _sync_order_payment patches an embedded payments entry ("captures" or
# "authorizations") inside the order's purchase_units so GET order reflects
# state changes made through the payments API.
def _sync_order_payment(order_id, kind, res_id, patch):
    if order_id == "":
        return
    oc = store_collection("orders")
    doc = oc.get(order_id)
    if doc == None:
        return
    changed = False
    for pu in doc.get("purchase_units", []):
        payments = pu.get("payments", {})
        arr = payments.get(kind, [])
        for item in arr:
            if item.get("id", "") == res_id:
                for k in patch:
                    item[k] = patch[k]
                changed = True
    if changed:
        oc.update(order_id, doc)

# _append_order_payment appends an embedded payments entry to the first
# purchase unit of an order (used when capturing an authorization).
def _append_order_payment(order_id, kind, entry):
    if order_id == "":
        return
    oc = store_collection("orders")
    doc = oc.get(order_id)
    if doc == None:
        return
    pus = doc.get("purchase_units", [])
    if len(pus) == 0:
        return
    pu = pus[0]
    payments = pu.get("payments", {})
    arr = payments.get(kind, [])
    arr.append(entry)
    payments[kind] = arr
    pu["payments"] = payments
    oc.update(order_id, doc)

# ============================================================================
# REFUND LIFECYCLE (derive-on-read state machine + over-refund guard)
# ============================================================================
# Real PayPal refunds are created PENDING and settle asynchronously. stunt
# derives the terminal state from the clock on every read (PENDING ->
# COMPLETED after 3s, or -> FAILED when created with the simulator-only
# simulate_fail flag), persists the transition, and emits
# PAYMENT.CAPTURE.REFUNDED exactly once on the COMPLETED transition. The
# over-refund guard counts every non-FAILED refund (PENDING included) of the
# capture.

# _refunds_for returns all refunds targeting one capture.
def _refunds_for(capture_id):
    docs = store_collection("refunds").list()
    return query_select(docs, [["capture_id", "=", capture_id]])

# _refunded_cents sums every non-FAILED refund of a capture (PENDING counts —
# the balance is reserved as soon as the refund is accepted).
def _refunded_cents(docs):
    total = 0
    for r in docs:
        if r.get("status", "COMPLETED") == "FAILED":
            continue
        amt = r.get("amount", {})
        c = _cents(amt.get("value", "0"))
        if c == None:
            c = 0
        total = total + c
    return total

# _advance_refund derives a refund's terminal state from the clock, persists
# it, and emits the webhook event exactly once on the transition.
def _advance_refund(doc):
    stage = doc.get("_stage", 0)
    if type(stage) == "float":
        stage = int(stage)
    if stage >= 2:
        return doc
    done_at = doc.get("_done_at", 0)
    if type(done_at) == "float":
        done_at = int(done_at)
    if clock.now_unix() < done_at:
        return doc
    if doc.get("_fail_mode", "") == "FAILED":
        doc["status"] = "FAILED"
    else:
        doc["status"] = "COMPLETED"
    doc["_stage"] = 2
    doc["update_time"] = clock.now_rfc3339()
    store_collection("refunds").update(doc["id"], doc)
    if doc["status"] == "COMPLETED":
        amt = doc.get("amount", {})
        _emit_event(
            "PAYMENT.CAPTURE.REFUNDED",
            "refund",
            "Payment refunded for " + amt.get("value", "0.00") + " " + amt.get("currency_code", "USD"),
            _refund_public(doc),
        )
    return doc
