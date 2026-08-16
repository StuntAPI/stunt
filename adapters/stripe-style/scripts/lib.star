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

# ============================================================================
# TEST CLOCK (global time offset)
# ============================================================================
# Real Stripe Test Clocks (docs.stripe.com/api/test_clocks) freeze per-object
# time and advance deterministically. The engine clock is read-only, so this
# mock keeps ONE GLOBAL offset in the KV store:
#     _now() = clock.now_unix() + offset
# POST /v1/test_clocks[/{id}/advance] (scripts/test_clocks.star) moves every
# object's notion of "now" at once — unlike real Stripe, which advances only
# the objects attached to the clock. EVERY timestamp minted by this adapter
# (created stamps, due dates, signature timestamps) must flow through _now(),
# never clock.now_unix() directly.

# _tc_offset reads the global test-clock offset (seconds, may be negative;
# _to_int_signed handles the leading "-").
def _tc_offset():
    raw = store_kv_get("stripe", "tc_offset")
    if raw == None or raw == "":
        return 0
    return _to_int_signed(raw)

def _now():
    return clock.now_unix() + _tc_offset()

# _tc_activate makes clock_id the active global clock at target time.
def _tc_activate(clock_id, target):
    store_kv_set("stripe", "tc_offset", str(target - clock.now_unix()))
    store_kv_set("stripe", "tc_active", clock_id)

# _tc_clear unsets the global offset, but only when the deleted clock is the
# active one (deleting a stale clock must not disturb the active one).
def _tc_clear(clock_id):
    if store_kv_get("stripe", "tc_active") == clock_id:
        store_kv_set("stripe", "tc_offset", "0")
        store_kv_delete("stripe", "tc_active")

# _signed_emit MACs the exact on-wire body and delivers with Stripe-Signature.
# The DELIVERED body is the full Stripe event object (id/object/type/data),
# exactly like a real Stripe webhook POST — so receiver-side SDK event parsers
# (stripe-node webhooks.constructEvent etc.) and signature verification both
# run against the real shape. The same serialized bytes feed the MAC and the
# delivery, so the signature verifies against what the sink receives. Stripe
# signs "{timestamp}.{body}" and carries t=,v1= in the header.
#
# The event is ALSO recorded in the "events" collection (the same object), so
# GET /v1/events (scripts/events.star) lists exactly what the sink receives.
def _signed_emit(event_type, payload):
    t = _now()
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
    # Webhook gating: with no registered webhook endpoints every event is
    # delivered (the adapter's historical always-deliver behavior). Once
    # endpoints exist (webhook_endpoints collection), only their
    # enabled_events (exact match or "*") are DELIVERED — the event object
    # above is still recorded either way, like real Stripe's GET /v1/events.
    if not _events_enabled(event_type):
        return
    body = json.encode(ev)
    sig = crypto.hmac_sha256(_WEBHOOK_SECRET, str(t) + "." + body)
    events_emit_raw(event_type, body, {"Stripe-Signature": "t=" + str(t) + ",v1=" + sig})

# _events_enabled reports whether event_type should be delivered to the
# configured webhook sink. True when no webhook endpoints are registered
# (store_collection auto-creates an empty table for undeclared resources, so
# an absent webhook_endpoints collection lists as empty), else True only if
# some endpoint lists the type (or "*") in enabled_events.
def _events_enabled(event_type):
    eps = store_collection("webhook_endpoints").list()
    if len(eps) == 0:
        return True
    for i in range(len(eps)):
        evs = eps[i].get("enabled_events", None)
        if evs == None:
            continue
        for j in range(len(evs)):
            et = evs[j]
            if et == "*" or et == event_type:
                return True
    return False

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

# _coupon_public renders a stored coupon (internal keys stripped).
def _coupon_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    return out


# _bad_body reports a malformed JSON body authoritatively: undecodable bodies
# arrive as EMPTY DICTS via req.body, so the raw bytes are the only reliable
# signal (json_safe_decode returns None on malformed). Empty body = not bad.
# Form-encoded bodies (real SDK clients POST urlencoded, not JSON) are parsed
# by the engine into req.body — they are never "bad JSON".
def _bad_body(req):
    raw = req.get("raw_body", "")
    if raw == None or raw == "":
        return False
    h = req.get("headers")
    ct = ""
    if h != None:
        ct = h.get("Content-Type", "")
        if ct == None:
            ct = ""
    if ct.find("x-www-form-urlencoded") >= 0:
        return False
    return json_safe_decode(raw) == None

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
# account, tracked via the KV store. Defaults to 0 for new accounts. Signed:
# the balance-transaction ledger can drive an account negative (dispute
# withdrawals, platform-side transfers), which plain _to_int cannot parse.
def _to_int_signed(s):
    if s == None:
        return 0
    t = s
    neg = False
    if t != "" and t[0] == "-":
        neg = True
        t = t[1:]
    n = _to_int(t)
    if neg:
        return -n
    return n

def _get_balance(acct_id):
    val = store_kv_get("stripe", "bal_" + acct_id)
    return _to_int_signed(val)

# _set_balance sets the available balance (in cents) for a connected account.
def _set_balance(acct_id, amount):
    store_kv_set("stripe", "bal_" + acct_id, str(amount))

# _not_found returns a standard Stripe-style 404 error response.
def _not_found(resource, id):
    return respond(404, {"error": {"message": "No such " + resource + ": '" + id + "'", "type": "invalid_request_error"}})

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
                err = respond(400, {"error": {"type": "invalid_request_error", "message": "No such " + resource + ": '" + sa + "'", "param": "starting_after"}})
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
# chunks so no card number ever appears as a contiguous literal.

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
        "balance_transaction": doc.get("balance_transaction", None),
        "receipt_number": doc.get("receipt_number", None),
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

# _refunded_total sums the amounts of every refund that still counts against
# the unrefunded balance: pending (Stripe reserves it immediately) and
# succeeded. failed refunds never counted; canceled ones are rolled back
# (funds returned), so they must not count either — otherwise a canceled
# refund permanently locks the remaining balance out of re-refund.
def _refunded_total(docs):
    total = 0
    for r in docs:
        st = r.get("status", "")
        if st == "failed" or st == "canceled":
            continue
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

# _receipt_number mints a Stripe-style refund receipt number ("1234-5678").
# Digits are runtime data (not source literals), so no length constraint.
def _receipt_number():
    seq = store_kv_incr("stripe", "receipt_seq")
    tail = _now() % (10 * 1000)
    return str(tail) + "-" + str(seq)

# _create_refund inserts a pending refund doc stamped with its async schedule
# and emits refund.created. fail_mode True drives the -> failed terminal.
# Creation also records the (negative) refund balance transaction on the
# platform account, like real Stripe.
def _create_refund(pi_id, charge_id, amount, currency, reason, fail_mode):
    now = _now()
    rid = _next_id("re")
    bt = _bt_record("", "refund", -amount, 0, currency, rid, "Refund")
    doc = {
        "id": rid,
        "object": "refund",
        "amount": amount,
        "balance_transaction": bt["id"],
        "receipt_number": _receipt_number(),
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
    now = _now()
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

# ============================================================================
# BALANCE-TRANSACTION LEDGER
# ============================================================================
# _bt_record appends a Stripe balance_transaction object
# (docs.stripe.com/api/balance_transactions) to the balance_transactions
# collection AND moves the account's KV balance by net (amount - fee),
# preserving the existing _get_balance/_set_balance semantics.
#
#   acct       connected-account id, or None/"" for the platform account
#   btype      charge | refund | payout | transfer | transfer_reversal |
#              application_fee | application_fee_refund | dispute |
#              dispute_reversal
#   amount     signed cents (negative = funds leaving the account)
#   fee        processing fee in cents (positive when assessed; a reversal
#              refunds the fee with a negative fee)
#   source_id  the Stripe object this txn belongs to (charge/transfer/...)
#
# Returns the stored txn doc; _bt_public strips the _account scoping key.

_BT_TYPES = [
    "charge",
    "refund",
    "payout",
    "transfer",
    "transfer_reversal",
    "application_fee",
    "application_fee_refund",
    "dispute",
    "dispute_reversal",
]

def _bt_record(acct, btype, amount, fee, currency, source_id, description):
    if acct == None:
        acct = ""
    net = amount - fee
    fee_details = []
    if fee > 0:
        fee_details = [
            {
                "amount": fee,
                "application": None,
                "currency": currency,
                "description": "Stripe fee",
                "type": "stripe_fee",
            },
        ]
    doc = {
        "id": _next_id("txn"),
        "object": "balance_transaction",
        "amount": amount,
        "available_on": _now(),
        "created": _now(),
        "currency": currency,
        "description": description,
        "exchange_rate": None,
        "fee": fee,
        "fee_details": fee_details,
        "net": net,
        "reporting_category": btype,
        "source": source_id,
        "status": "available",
        "type": btype,
        "_account": acct,
    }
    store_collection("balance_transactions").insert(doc)
    _set_balance(acct, _get_balance(acct) + net)
    return doc

# _bt_public strips the internal _account scoping key from a stored txn.
def _bt_public(doc):
    out = {}
    for k in doc:
        if k == "_account":
            continue
        out[k] = doc[k]
    return out

# ============================================================================
# APPLICATION FEE HOOK (Stripe Connect)
# ============================================================================
# A charge created (or captured) with application_fee_amount records an
# application_fee object (docs.stripe.com/api/application_fees) plus its
# platform-side balance transaction (type application_fee).

def _maybe_record_fee(ch, body):
    if body == None:
        return None
    amt = _num(body.get("application_fee_amount", 0))
    if amt <= 0:
        return None
    fee_id = _next_id("fee")
    bt = _bt_record("", "application_fee", amt, 0, ch.get("currency", "usd"), fee_id, "Application fee")
    doc = {
        "id": fee_id,
        "object": "application_fee",
        "amount": amt,
        "currency": ch.get("currency", "usd"),
        "charge": ch.get("id"),
        "balance_transaction": bt["id"],
        "refunded": False,
        "amount_refunded": 0,
        "created": _now(),
    }
    store_collection("application_fees").insert(doc)
    _signed_emit("application_fee.created", doc)
    return doc

# ============================================================================
# DISPUTE ENGINE (creation + derive-on-read state machine)
# ============================================================================
# The documented dispute test cards (docs.stripe.com/testing): charging with
# these SUCCEEDS and immediately raises a dispute. Real Stripe now mints du_*
# dispute ids; this simulator uses the du_* prefix shared across the billing
# domains' doc contracts.
#   4000 0000 0000 0259 -> reason fraudulent
#   4000 0000 0000 2685 -> reason product_not_received

_DISPUTE_CARDS = {
    "4000" + "0000" + "0000" + "0259": "fraudulent",
    "4000" + "0000" + "0000" + "2685": "product_not_received",
}

_DISPUTE_FEE = 1500  # $15.00 dispute fee (US), one 4-digit chunk.

# _dispute_reason_for maps a card number to its dispute reason, or None.
def _dispute_reason_for(number):
    if number == None:
        return None
    return _DISPUTE_CARDS.get(number)

# _maybe_create_dispute raises the dispute for a dispute test card on a newly
# captured charge: creates the dispute doc (needs_response), records the funds
# withdrawal (type dispute, -amount, $15 fee) on the platform ledger, points
# the charge's `dispute` field at it, persists everything, and emits
# charge.dispute.created + charge.dispute.funds_withdrawn. Returns the dispute
# doc or None when the card behaves normally / the charge is already disputed.
def _maybe_create_dispute(ch, number):
    reason = _dispute_reason_for(number)
    if reason == None:
        return None
    if ch.get("dispute", None) != None:
        return None
    now = _now()
    due_by = now + 7 * 24 * 3600  # evidence window: created + 7 days
    dp = {
        "id": _next_id("du"),
        "object": "dispute",
        "amount": _num(ch.get("amount", 0)),
        "balance_transactions": [],
        "charge": ch.get("id"),
        "created": now,
        "currency": ch.get("currency", "usd"),
        "evidence": {},
        "evidence_details": {
            "due_by": due_by,
            "has_evidence": False,
            "past_due": False,
            "submission_count": 0,
        },
        "is_charge_refundable": True,
        "livemode": False,
        "metadata": {},
        "payment_intent": ch.get("payment_intent", None),
        "reason": reason,
        "status": "needs_response",
        "_due_by": due_by,
        "_submit_at": None,
        "_settle_at": None,
        "_stage": 0,
        "_closed": False,
    }
    bt = _bt_record("", "dispute", -dp["amount"], _DISPUTE_FEE, dp["currency"], dp["id"], "Dispute withdrawal")
    dp["balance_transactions"] = [bt["id"]]
    ch["dispute"] = dp["id"]
    # Persist every state change BEFORE emitting (dispute doc, then charge).
    store_collection("disputes").insert(dp)
    store_collection("charges").update(ch["id"], ch)
    pub = _dispute_public(dp)
    _signed_emit("charge.dispute.created", pub)
    _signed_emit("charge.dispute.funds_withdrawn", pub)
    return dp

# _dispute_public renders the public dispute shape. evidence_details.past_due
# and has_evidence are derived on read (docs.stripe.com/api/disputes/object).
def _dispute_public(doc):
    now = _now()
    ev = doc.get("evidence", {})
    if ev == None:
        ev = {}
    has_ev = len(ev) > 0
    ed = doc.get("evidence_details", {})
    if ed == None:
        ed = {}
    due_by = _num(ed.get("due_by", 0))
    past = False
    if due_by > 0 and now >= due_by:
        past = True
    return {
        "id": doc["id"],
        "object": "dispute",
        "amount": _num(doc.get("amount", 0)),
        "balance_transactions": doc.get("balance_transactions", []),
        "charge": doc.get("charge", None),
        "created": _num(doc.get("created", 0)),
        "currency": doc.get("currency", "usd"),
        "evidence": ev,
        "evidence_details": {
            "due_by": due_by,
            "has_evidence": has_ev,
            "past_due": past,
            "submission_count": _num(ed.get("submission_count", 0)),
        },
        "is_charge_refundable": doc.get("is_charge_refundable", True),
        "livemode": False,
        "metadata": doc.get("metadata", {}),
        "payment_intent": doc.get("payment_intent", None),
        "reason": doc.get("reason", "general"),
        "status": doc.get("status", "needs_response"),
    }

# _dispute_advance derives a dispute's state from the clock, persisting then
# emitting each transition exactly once:
#   needs_response + _submit_at set      -> under_review (+ charge.dispute.updated)
#   under_review + now >= _settle_at     -> won (dispute_reversal ledger row
#                                           restores funds + fee;
#                                           funds_reinstated + closed won)
#   needs_response + now >= due_by       -> lost (closed lost; funds stay
#                                           withdrawn)
# Call from every dispute read endpoint. Returns the (possibly mutated) doc.
def _dispute_advance(doc):
    if doc.get("_closed", False) == True:
        return doc
    now = _now()
    status = doc.get("status", "")
    if status == "needs_response":
        if doc.get("_submit_at", None) != None:
            doc["status"] = "under_review"
            doc["_stage"] = 1
            store_collection("disputes").update(doc["id"], doc)
            _signed_emit("charge.dispute.updated", _dispute_public(doc))
            status = "under_review"
        elif now >= _num(doc.get("_due_by", 0)):
            return _dispute_close(doc)
        else:
            return doc
    if status == "under_review":
        settle = _num(doc.get("_settle_at", 0))
        if settle > 0 and now >= settle:
            bt = _bt_record("", "dispute_reversal", _num(doc.get("amount", 0)), -_DISPUTE_FEE, doc.get("currency", "usd"), doc["id"], "Dispute reinstated")
            bts = doc.get("balance_transactions", [])
            bts.append(bt["id"])
            doc["balance_transactions"] = bts
            doc["status"] = "won"
            doc["_stage"] = 2
            doc["_closed"] = True
            store_collection("disputes").update(doc["id"], doc)
            pub = _dispute_public(doc)
            _signed_emit("charge.dispute.funds_reinstated", pub)
            _signed_emit("charge.dispute.closed", pub)
    return doc

# _dispute_close resolves a dispute as lost immediately (merchant accepts or
# evidence deadline passed): funds stay withdrawn; emits charge.dispute.closed.
def _dispute_close(doc):
    if doc.get("_closed", False) == True:
        return doc
    doc["status"] = "lost"
    doc["_stage"] = 2
    doc["_closed"] = True
    store_collection("disputes").update(doc["id"], doc)
    _signed_emit("charge.dispute.closed", _dispute_public(doc))
    return doc

# _dispute_submit records an evidence submission (the dispute-update endpoint
# calls this). It schedules the ruling for _settle_at = submit time + 1 day;
# the mock always rules in the merchant's favor when evidence is submitted.
# The needs_response -> under_review transition is derived right away so the
# submit response reflects it.
def _dispute_submit(doc, winning, evidence):
    now = _now()
    doc["_submit_at"] = now
    if evidence != None:
        doc["evidence"] = evidence
    ed = doc.get("evidence_details", {})
    if ed == None:
        ed = {}
    ed["submission_count"] = _num(ed.get("submission_count", 0)) + 1
    doc["evidence_details"] = ed
    if winning:
        doc["_settle_at"] = now + 24 * 3600
    store_collection("disputes").update(doc["id"], doc)
    return _dispute_advance(doc)

# ============================================================================
# CHARGE SETTLEMENT HOOK (shared by charges.star + payment_intents.star)
# ============================================================================
# _charge_settle_hooks records the money movement behind a newly captured
# charge: the charge balance transaction (2.9% + 30c processing fee, pure
# integer math), an application_fee record when the request carried
# application_fee_amount, and the immediate dispute raised by the dispute test
# cards. Idempotent: a charge that already has a balance_transaction is left
# alone. The charge doc is persisted before any emission.
def _charge_settle_hooks(doc, body, number):
    if doc.get("balance_transaction", None) != None:
        return
    amount = _num(doc.get("amount", 0))
    fee = (amount * 29 + 500) // 1000 + 30
    bt = _bt_record("", "charge", amount, fee, doc.get("currency", "usd"), doc["id"], doc.get("description", None))
    doc["balance_transaction"] = bt["id"]
    store_collection("charges").update(doc["id"], doc)
    _maybe_record_fee(doc, body)
    _maybe_create_dispute(doc, number)

# ============================================================================
# BILLING PRIMITIVES (calendar math + subscription invoice construction)
# ============================================================================

# _days_in_month returns the day count of month m (1-12) in year y (Gregorian,
# proleptic — matching Go's time package).
def _days_in_month(y, m):
    if m == 2:
        if (y % 4 == 0 and y % 100 != 0) or y % 400 == 0:
            return 29
        return 28
    if m == 4 or m == 6 or m == 9 or m == 11:
        return 30
    return 31

# _civil_to_unix converts a UTC civil date/time to Unix seconds using Howard
# Hinnant's days_from_civil (no datetime library in Starlark). The two long
# constants are assembled from <=4-digit chunks.
_DAYS_PER_ERA = 146 * 1000 + 97       # days in a 400-year Gregorian era
_EPOCH_SHIFT = 719 * 1000 + 468       # days from 0000-03-01 to 1970-01-01

def _civil_to_unix(y, m, d, hh, mm, ss):
    yy = y
    if m <= 2:
        yy = yy - 1
    era = yy // 400
    yoe = yy - era * 400
    mp = m + 9
    if m > 2:
        mp = m - 3
    doy = (153 * mp + 2) // 5 + d - 1
    doe = yoe * 365 + yoe // 4 - yoe // 100 + doy
    days = era * _DAYS_PER_ERA + doe - _EPOCH_SHIFT
    return days * 24 * 3600 + hh * 3600 + mm * 60 + ss

# _unix_to_civil splits Unix seconds into UTC (y, m, d, hh, mm, ss) via the
# engine's RFC3339 formatter (fixed-width "YYYY-MM-DDTHH:MM:SSZ").
def _unix_to_civil(ts):
    s = clock.unix_to_rfc3339(ts)
    return _to_int(s[0:4]), _to_int(s[5:7]), _to_int(s[8:10]), _to_int(s[11:13]), _to_int(s[14:16]), _to_int(s[17:19])

# _add_months shifts ts forward n calendar months with end-of-month clamping
# (Jan 31 + 1 month = Feb 28/29; May 31 + 3 months = Aug 31). The time of day
# is preserved.
def _add_months(ts, n):
    y, m, d, hh, mm, ss = _unix_to_civil(ts)
    total = y * 12 + (m - 1) + n
    ny = total // 12
    nm = total - ny * 12 + 1
    nd = d
    dim = _days_in_month(ny, nm)
    if nd > dim:
        nd = dim
    return _civil_to_unix(ny, nm, nd, hh, mm, ss)

# _subscription_invoice constructs, stores, and announces the invoice for one
# subscription billing cycle (INVOICE DOC CONTRACT — shared with the billing
# domains):
#   sub          the subscription doc (customer, currency, collection_method,
#                discount, _period_no drive the derived fields)
#   line_dicts   [{type, description, amount, quantity, period, price}, ...]
#   discount_amt discount cents (already computed: percent -> int(subtotal*pct/
#                100 + 0.5), amount_off capped at subtotal)
#   tax_cents    tax cents over the discounted subtotal
#   inclusive    True for tax-inclusive rates (tax shown, NOT added to total)
# Line ids (il_*), proration False, tax_rates [], subtotal/total/amount_due
# are filled in here. The invoice is stored in the invoices collection with
# status "open", invoice.created is emitted, and the doc is returned for the
# caller to advance (auto-charge -> paid, past_due, ...).
def _subscription_invoice(sub, line_dicts, discount_amt, tax_cents, inclusive):
    now = _now()
    subtotal = 0
    lines = []
    for i in range(len(line_dicts)):
        ln = line_dicts[i]
        amt = _num(ln.get("amount", 0))
        qty = _num(ln.get("quantity", 1))
        if qty < 1:
            qty = 1
        subtotal = subtotal + amt * qty
        out = {
            "id": _next_id("il"),
            "object": "line_item",
            "type": ln.get("type", "subscription"),
            "description": ln.get("description", ""),
            "amount": amt,
            "quantity": qty,
            "period": ln.get("period", {"start": now, "end": now}),
            "price": ln.get("price", None),
            "proration": False,
            "tax_rates": [],
        }
        lines.append(out)
    discount = _num(discount_amt)
    if discount > subtotal:
        discount = subtotal
    tax = _num(tax_cents)
    if tax < 0:
        tax = 0
    total = subtotal - discount
    if not inclusive:
        total = total + tax
    currency = sub.get("currency", None)
    if currency == None or currency == "":
        currency = "usd"
        for i in range(len(lines)):
            price = lines[i].get("price", None)
            if price != None and price.get("currency", None) != None:
                currency = price["currency"]
                break
    reason = "subscription_cycle"
    if _num(sub.get("_period_no", 1)) <= 1:
        reason = "subscription_create"
    inv = {
        "id": _next_id("in"),
        "object": "invoice",
        "customer": sub.get("customer", None),
        "subscription": sub.get("id", None),
        "status": "open",
        "collection_method": sub.get("collection_method", "charge_automatically"),
        "currency": currency,
        "lines": lines,
        "subtotal": subtotal,
        "discount": sub.get("discount", None),
        "tax": tax,
        "total": total,
        "amount_due": total,
        "amount_paid": 0,
        "amount_remaining": total,
        "starting_balance": 0,
        "charge": None,
        "payment_intent": None,
        "status_transitions": {"finalized_at": now, "paid_at": None, "voided_at": None},
        "billing_reason": reason,
        "due_date": None,
        "created": now,
        "auto_advance": True,
        "attempted": False,
        "metadata": {},
        "paid": None,
        "_advance_scheduled": False,
    }
    store_collection("invoices").insert(inv)
    _signed_emit("invoice.created", _invoice_public(inv))
    return inv

# _invoice_public renders a stored invoice doc, stripping internal "_" keys.
# lines is stored as a bare array but rendered in a list envelope — real
# Stripe's invoice object wraps it, and typed SDKs read invoice.lines.data.
def _invoice_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    out["lines"] = {
        "object": "list",
        "data": doc.get("lines", []),
        "has_more": False,
        "total_count": len(doc.get("lines", [])),
        "url": "/v1/invoices/" + doc.get("id", "") + "/lines",
    }
    return out
