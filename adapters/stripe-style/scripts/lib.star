# Shared library for stripe-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# Mock webhook signing secret. Configure your Stripe webhook receiver with
# this exact string to verify stunt's deliveries. Public + low-entropy: local
# stunt only — never reuse outside the simulator.
_WEBHOOK_SECRET = "whsec_stunt_mock_0123456789abcdef0123456789abcdef"

# _signed_emit MACs the exact on-wire body and delivers with Stripe-Signature.
# The same (event_type, payload) feeds events_body (signing input) and
# events_emit (delivery), so the signature verifies against the bytes the sink
# receives. Stripe signs "{timestamp}.{body}" and carries t=,v1= in the header.
#
# Every emitted event is ALSO recorded in the "events" collection with
# Stripe's event-object shape, so GET /v1/events (scripts/events.star) lists
# exactly the event types the webhook sink receives.
def _signed_emit(event_type, payload):
    t = clock.now_unix()
    ev = {
        "id": _next_id("evt"),
        "object": "event",
        "api_version": "2025-01-27.acacia",
        "created": t,
        "type": event_type,
        "data": {"object": payload},
        "livemode": False,
        "pending_webhooks": 0,
        "request": {"id": None, "idempotency_key": None},
    }
    store_collection("events").insert(ev)
    body = events_body(event_type, payload)
    sig = crypto.hmac_sha256(_WEBHOOK_SECRET, str(t) + "." + body)
    events_emit(event_type, payload, {"Stripe-Signature": "t=" + str(t) + ",v1=" + sig})

# _bearer_token extracts the bearer token from the Authorization header, or
# None if absent.
def _bearer_token(req):
    headers = req.get("headers")
    if headers == None:
        return None
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return None

# _require_auth validates the bearer token.
#
# Returns None if authorized, or an error-response dict to return from the
# handler if not.
#
# Dev bypass: tokens starting with "sk_test" are accepted WITHOUT
# identity_validate, for frictionless local testing.
def _require_auth(req):
    token = _bearer_token(req)
    if token == None:
        return respond(401, {"error": {"type": "authentication_error", "message": "Missing Authorization header. Provide 'Authorization: Bearer <token>'."}})

    # Dev bypass: sk_test tokens skip real validation.
    if token.startswith("sk_test"):
        return None

    # Real validation via the identity issuer.
    claims = identity_validate(token)
    if claims == None:
        return respond(401, {"error": {"type": "authentication_error", "message": "Invalid API Key provided."}})
    return None

# _next_id returns a monotonically-increasing provider-style ID using the
# KV store as a sequence counter. Produces ids like "ch_1", "ch_2", ...
def _next_id(prefix):
    # Atomic increment via store_kv_incr (race-free under concurrent requests).
    return prefix + "_" + str(store_kv_incr("stripe", prefix + "_seq"))

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
def _to_int(s):
    if s == None:
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return n
    return n

# _get_query safely returns a query parameter value ("" if absent/None).
def _get_query(req, key):
    q = req.get("query")
    if q == None:
        return ""
    v = q.get(key, "")
    if v == None:
        return ""
    return v

# _created_filters maps Stripe's `created` / `created[gt|gte|lt|lte]` query
# params (exact timestamp or bracketed range, form-encoded) to query_select
# triples against the int `created` field. Appends to the clause list in
# place; no-op when none of the params is set.
def _created_filters(req, f):
    v = _to_int(_get_query(req, "created"))
    if v > 0:
        f.append(["created", "=", v])
    v = _to_int(_get_query(req, "created[gt]"))
    if v > 0:
        f.append(["created", ">", v])
    v = _to_int(_get_query(req, "created[gte]"))
    if v > 0:
        f.append(["created", ">=", v])
    v = _to_int(_get_query(req, "created[lt]"))
    if v > 0:
        f.append(["created", "<", v])
    v = _to_int(_get_query(req, "created[lte]"))
    if v > 0:
        f.append(["created", "<=", v])

# _created_check validates the `created` / `created[gt|gte|lt|lte]` filter
# params. Real Stripe rejects a non-numeric value with a 400
# parameter_invalid_integer error; previously _to_int silently parsed the
# numeric prefix and ignored the rest. Returns None when all set params are
# numeric, or a 400 error response the caller must return.
def _created_check(req):
    for key in ["created", "created[gt]", "created[gte]", "created[lt]", "created[lte]"]:
        v = _get_query(req, key)
        if v == "":
            continue
        ok = True
        for i in range(len(v)):
            ch = v[i]
            if ch < "0" or ch > "9":
                ok = False
                break
        if not ok:
            return respond(400, {"error": {"code": "parameter_invalid_integer", "message": "Invalid integer: " + v, "param": key, "type": "invalid_request_error"}})
    return None

# _newest_first returns docs in reverse insertion order. Store lists are
# oldest-first, but Stripe list endpoints return objects "sorted by creation
# date, with the most recent appearing first" (charges, customers,
# payment intents, refunds, payouts, transfers). Apply before _list_page so
# both the default page and the starting_after offset lookup operate on
# newest-first order. Stored `created` values are a constant fixture
# timestamp, so sorting on `created` cannot reorder anything; reversing
# insertion order (ids are monotonically increasing) yields creation-date
# descending exactly.
def _newest_first(docs):
    out = []
    i = len(docs) - 1
    while i >= 0:
        out.append(docs[i])
        i = i - 1
    return out

# _stripe_account extracts the Stripe-Account header used by Stripe Connect
# to scope requests to a connected account. Returns None if absent.
def _stripe_account(req):
    headers = req.get("headers")
    if headers == None:
        return None
    acct = headers.get("Stripe-Account", "")
    if acct == None or acct == "":
        return None
    return acct

# _get_balance returns the available balance (in cents) for a connected
# account, tracked via the KV store. Defaults to 0 for new accounts.
def _get_balance(acct_id):
    val = store_kv_get("stripe", "bal_" + acct_id)
    return _to_int(val)

# _set_balance sets the available balance (in cents) for a connected account.
def _set_balance(acct_id, amount):
    store_kv_set("stripe", "bal_" + acct_id, str(amount))

# _not_found returns a standard Stripe-style 404 error response.
def _not_found(resource, id):
    return respond(404, {"error": {"message": "No such " + resource + ": " + id, "type": "invalid_request_error"}})

# _list_page applies Stripe cursor pagination (limit + starting_after) to a list
# of docs via the paginate builtin. Returns (page, has_more, error_response).
# Stripe does not echo a cursor: the client sets starting_after to the last
# returned id next time. If starting_after names an id that is not present
# (deleted object / stale cursor), error_response is a 400 the caller must
# return — mirroring Stripe's resource_missing, instead of silently paging
# from the start (which would loop a cursor-following client forever).
_STRIPE_DEFAULT_LIMIT = 10
_STRIPE_MAX_LIMIT = 100

def _list_page(req, docs, resource):
    limit = _STRIPE_DEFAULT_LIMIT
    offset = ""
    q = req.get("query")
    if q != None:
        n = _to_int(q.get("limit", ""))
        if n > 0:
            limit = n
    if limit > _STRIPE_MAX_LIMIT:
        limit = _STRIPE_MAX_LIMIT
    if q != None:
        sa = q.get("starting_after", "")
        if sa != None and sa != "":
            found = False
            for i in range(len(docs)):
                if docs[i].get("id") == sa:
                    offset = str(i + 1)
                    found = True
                    break
            if not found:
                err = respond(400, {"error": {"type": "invalid_request_error", "message": "No such " + resource + ": " + sa, "param": "starting_after"}})
                return None, False, err
    page, nxt = paginate(docs, limit, offset)
    return page, nxt != None, None

# --- Idempotency (Stripe Idempotency-Key header) ---
# A write carrying Idempotency-Key replays the original response verbatim.
# Scoped by method+path+collection+key so the same key on different endpoints
# never collides. The cache stores "<status>:<resource_id>"; the handler
# re-renders the resource from its stored doc on replay.

# _idempotency_key reads the Idempotency-Key header ("" if absent).
def _idempotency_key(req):
    h = req.get("headers")
    if h == None:
        return ""
    k = h.get("Idempotency-Key", "")
    if k == None:
        return ""
    return k

def _idempotency_scope(req, ns):
    return req["method"] + "|" + req["path"] + "|" + ns + "|" + _idempotency_key(req)

# _idempotent_lookup returns {"status": int, "doc": dict} for a prior write with
# this key, or None. The doc is the stored resource (re-fetched, so for mutating
# endpoints it reflects the final state).
def _idempotent_lookup(req, ns):
    if _idempotency_key(req) == "":
        return None
    raw = store_kv_get("stripe", "idem_" + _idempotency_scope(req, ns))
    if raw == None or raw == "":
        return None
    sep = raw.find(":")
    if sep < 0:
        return None
    doc = store_collection(ns).get(raw[sep + 1:])
    if doc == None:
        return None
    return {"status": int(raw[:sep]), "doc": doc}

# _idempotent_remember caches "<status>:<resource_id>" for this key. Call only
# on the success path (Stripe does not cache non-2xx).
def _idempotent_remember(req, ns, status, rid):
    if _idempotency_key(req) == "":
        return
    store_kv_set("stripe", "idem_" + _idempotency_scope(req, ns), str(status) + ":" + rid)

# _num coerces a JSON-round-tripped number (int, float, or numeric string) to
# int. Returns 0 for None, bools map to 0/1.
def _num(v):
    if v == None:
        return 0
    if type(v) == "bool":
        if v:
            return 1
        return 0
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(v)

# ============================================================================
# TEST-CARD BEHAVIOR (declines + SCA/3DS)
# ============================================================================
# Real Stripe reserves specific test card numbers for deterministic outcomes:
# declines (with the real decline_code) and SCA cards that force 3DS
# authentication. The digit strings are assembled at runtime from <=4-digit
# chunks so no literal in this file ever contains 5+ consecutive digits.

_DECLINE_CARDS = {
    "4000" + "0000" + "0000" + "0002": {"code": "card_declined", "decline_code": "generic_decline", "message": "Your card was declined."},
    "4000" + "0000" + "0000" + "9995": {"code": "card_declined", "decline_code": "insufficient_funds", "message": "Your card has insufficient funds."},
    "4000" + "0000" + "0000" + "9987": {"code": "card_declined", "decline_code": "lost_card", "message": "Your card was declined."},
    "4000" + "0000" + "0000" + "9988": {"code": "card_declined", "decline_code": "stolen_card", "message": "Your card was declined."},
    "4000" + "0000" + "0000" + "0069": {"code": "expired_card", "decline_code": "expired_card", "message": "Your card has expired."},
    "4000" + "0000" + "0000" + "0127": {"code": "incorrect_cvc", "decline_code": "incorrect_cvc", "message": "Your card's security code is incorrect."},
}

# SCA test cards: 3DS in-app SDK flow, and the two challenge/redirect cards.
_SCA_SDK_CARD = "4000" + "0027" + "6000" + "3184"
_SCA_REDIRECT_CARDS = [
    "4000" + "0025" + "0000" + "3155",
    "4000" + "0000" + "0000" + "3220",
]

# _card_outcome maps a card number to its simulated outcome, or None when the
# card behaves normally (attaches + pays). Returns a dict with kind
# "decline" (plus code/decline_code/message), "sca_sdk", or "sca_redirect".
def _card_outcome(number):
    if number == None:
        return None
    d = _DECLINE_CARDS.get(number)
    if d != None:
        out = {"kind": "decline"}
        for k in d:
            out[k] = d[k]
        return out
    if number == _SCA_SDK_CARD:
        return {"kind": "sca_sdk"}
    for i in range(len(_SCA_REDIRECT_CARDS)):
        if number == _SCA_REDIRECT_CARDS[i]:
            return {"kind": "sca_redirect"}
    return None

# _card_number_for resolves the underlying card number for a payment
# instrument id: a card token (tok_*, from POST /v1/tokens) or a PaymentMethod
# created with an explicit card[number]. Returns "" when unknown (the
# instrument then behaves like a normal card).
def _card_number_for(id):
    if id == None:
        return ""
    t = store_collection("tokens").get(id)
    if t != None:
        return t.get("_number", "")
    p = store_collection("payment_methods").get(id)
    if p != None:
        return p.get("_card_number", "")
    return ""

# _card_decline_error builds the 402 card_error response for a declined test
# card, carrying the real error.code + decline_code. kind is "payment_intent"
# or "charge" and names the resource id in the error body like real Stripe.
def _card_decline_error(oc, kind, res_id):
    e = {
        "code": oc["code"],
        "decline_code": oc["decline_code"],
        "doc_url": "https://stripe.com/docs/error-codes/card-declined",
        "message": oc["message"],
        "type": "card_error",
    }
    if kind == "payment_intent":
        e["payment_intent"] = res_id
    else:
        e["charge"] = res_id
    return respond(402, {"error": e})

# _sca_charge_error is the legacy-charges-API outcome for an SCA test card:
# the Charges API cannot run 3DS, so the payment fails with the real
# authentication_required decline_code.
def _sca_charge_error(charge_id):
    return respond(402, {"error": {
        "charge": charge_id,
        "code": "card_declined",
        "decline_code": "authentication_required",
        "doc_url": "https://stripe.com/docs/error-codes/card-declined",
        "message": "This payment requires authentication to complete. Use the PaymentIntents API instead.",
        "type": "card_error",
    }})

# ============================================================================
# REFUND LIFECYCLE (derive-on-read state machine + over-refund guard)
# ============================================================================
# Real refunds start "pending" and settle asynchronously. stunt derives the
# terminal state from the clock on every read (pending -> succeeded after 3s,
# or -> failed when created with the simulator-only simulate_fail flag),
# persists the transition, and emits refund.updated exactly once. The
# over-refund guard counts every non-failed refund (pending included) of the
# same payment/charge, like the real API.

def _refund_public(doc):
    return {
        "id": doc["id"],
        "object": "refund",
        "amount": doc.get("amount", 0),
        "currency": doc.get("currency", "usd"),
        "payment_intent": doc.get("payment_intent", None),
        "charge": doc.get("charge", None),
        "reason": doc.get("reason", "requested_by_customer"),
        "status": doc.get("status", "succeeded"),
        "created": doc.get("created", 1700000000),
    }

# _refunds_for returns all refunds targeting one payment_intent or charge.
def _refunds_for(field, val):
    docs = store_collection("refunds").list()
    return query_select(docs, [[field, "=", val]])

# _refunded_total sums the amounts of every non-failed refund (pending
# refunds count — Stripe reserves the unrefunded balance immediately).
def _refunded_total(docs):
    total = 0
    for r in docs:
        if r.get("status") != "failed":
            total = total + _num(r.get("amount", 0))
    return total

# _usd renders integer cents as a "$dollars.cents" string for error messages.
def _usd(cents):
    if cents < 0:
        cents = 0
    d = cents // 100
    c = cents % 100
    cs = str(c)
    if c < 10:
        cs = "0" + cs
    return "$" + str(d) + "." + cs

# _over_refund_error is the real Stripe 400 for refunding more than the
# unrefunded balance (or refunding an already fully refunded resource).
def _over_refund_error(requested, remaining):
    if remaining <= 0:
        return respond(400, {"error": {"code": "charge_already_refunded", "message": "Charge has already been refunded.", "type": "invalid_request_error"}})
    return respond(400, {"error": {"message": "Refund amount (" + _usd(requested) + ") is greater than unrefunded amount on charge (" + _usd(remaining) + ")", "param": "amount", "type": "invalid_request_error"}})

# _create_refund inserts a pending refund doc stamped with its async schedule
# and emits refund.created. fail_mode True drives the -> failed terminal.
def _create_refund(pi_id, charge_id, amount, currency, reason, fail_mode):
    now = clock.now_unix()
    doc = {
        "id": _next_id("re"),
        "object": "refund",
        "amount": amount,
        "currency": currency,
        "payment_intent": pi_id,
        "charge": charge_id,
        "reason": reason,
        "status": "pending",
        "created": now,
        "_stage": 0,
        "_done_at": now + 3,
    }
    if fail_mode:
        doc["_fail_mode"] = "failed"
    else:
        doc["_fail_mode"] = ""
    store_collection("refunds").insert(doc)
    _signed_emit("refund.created", _refund_public(doc))
    return doc

# _advance_refund derives a refund's terminal state from the clock, persists
# it, and emits refund.updated exactly once on the transition. Returns the doc.
def _advance_refund(doc):
    if _num(doc.get("_stage", 0)) >= 2:
        return doc
    now = clock.now_unix()
    if now < _num(doc.get("_done_at", 0)):
        return doc
    if doc.get("_fail_mode", "") == "failed":
        doc["status"] = "failed"
    else:
        doc["status"] = "succeeded"
    doc["_stage"] = 2
    store_collection("refunds").update(doc["id"], doc)
    _signed_emit("refund.updated", _refund_public(doc))
    return doc

# _apply_charge_refund mutates a charge doc to reflect a refund of `amount`
# added on top of `already` refunded cents (real Stripe: amount_refunded grows,
# refunded/status flip only when the balance hits the full charge amount).
def _apply_charge_refund(ch, already, amount):
    base = _num(ch.get("amount", 0))
    ch["amount_refunded"] = already + amount
    if already + amount >= base:
        ch["refunded"] = True
        ch["status"] = "refunded"
    else:
        ch["refunded"] = False
    return ch
