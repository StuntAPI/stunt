# Shared library for braintree-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# Braintree auth: Bearer token OR public key + private key (basic auth style).
# We check presence of either.

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

# _require_auth validates the auth (Bearer or basic). Returns None if
# authorized, or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token != "":
        return None
    # Check for basic auth (public_key:private_key).
    headers = req.get("headers")
    if headers != None:
        auth = headers.get("Authorization", "")
        if auth != None and auth.startswith("Basic "):
            return None
    return _bt_err(401, "AUTHENTICATION", "Authentication failed: missing or invalid credentials")

# _bt_err returns a Braintree-style error response (REST).
def _bt_err(status, code, message):
    return respond(status, {
        "error": {
            "code": code,
            "message": message,
        },
    })

# _bt_graphql_error returns a Braintree GraphQL error envelope.
def _bt_graphql_error(message):
    return respond(200, {
        "data": None,
        "errors": [{"message": message}],
    })

# _txn_id generates a Braintree transaction ID.
def _txn_id():
    n = store_kv_incr("braintree", "txn_seq")
    return _alpha_id("t", n)

# _customer_id generates a Braintree customer ID.
def _customer_id():
    n = store_kv_incr("braintree", "customer_seq")
    return "customer_" + str(n)

# _payment_method_token generates a Braintree payment method token.
def _payment_method_token():
    n = store_kv_incr("braintree", "pm_seq")
    return _alpha_id("pm", n)

# _alpha_id generates an alphanumeric ID with a prefix.
def _alpha_id(prefix, n):
    alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
    s = ""
    val = n
    if val == 0:
        s = "0"
    while val > 0:
        s = alphabet[val % 36] + s
        val = val // 36
    # Pad to 6 chars.
    while len(s) < 6:
        s = alphabet[0] + s
    return prefix + s

# _txn_public returns the Braintree-shaped transaction object.
def _txn_public(doc):
    return {
        "id": doc.get("id", ""),
        "status": doc.get("status", "authorized"),
        "type": doc.get("type", "sale"),
        "amount": doc.get("amount", "0.00"),
        "currency": doc.get("currency", "USD"),
        "customer": doc.get("customer", {}),
        "creditCard": doc.get("creditCard", {
            "last4": "1111",
            "cardType": "Visa",
            "expirationDate": "03/2030",
        }),
        "createdAt": doc.get("createdAt", ""),
        "settledAt": doc.get("settledAt", ""),
        "voidedAt": doc.get("voidedAt", ""),
        "refundedTransactionId": doc.get("refundOf", ""),
    }

# _customer_public returns the Braintree-shaped customer object.
def _customer_public(doc):
    return {
        "id": doc.get("id", ""),
        "firstName": doc.get("firstName", ""),
        "lastName": doc.get("lastName", ""),
        "email": doc.get("email", ""),
        "createdAt": doc.get("createdAt", "2024-06-15T12:30:00.000Z"),
    }

# _client_token generates a synthetic client token for the Braintree Drop-in.
def _client_token():
    n = store_kv_incr("braintree", "ct_seq")
    return "production_cb_" + _alpha_id("ct", n + 10000)

# _contains checks if a string contains a substring (Starlark has no `in` for strings).
def _contains(haystack, needle):
    if haystack == None or needle == None:
        return False
    if len(needle) == 0:
        return True
    for i in range(len(haystack) - len(needle) + 1):
        if haystack[i:i + len(needle)] == needle:
            return True
    return False

# --- Idempotency (Idempotency-Key header on transaction create) ---
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

def _idempotent_lookup(req, ns):
    if _idempotency_key(req) == "":
        return None
    raw = store_kv_get("braintree", "idem_" + _idempotency_scope(req, ns))
    if raw == None or raw == "":
        return None
    sep = raw.find(":")
    if sep < 0:
        return None
    doc = store_collection(ns).get(raw[sep + 1:])
    if doc == None:
        return None
    return {"status": int(raw[:sep]), "doc": doc}

def _idempotent_remember(req, ns, status, rid):
    if _idempotency_key(req) == "":
        return
    store_kv_set("braintree", "idem_" + _idempotency_scope(req, ns), str(status) + ":" + rid)

# ============================================================================
# OUTBOUND WEBHOOKS (signed, Braintree bt_signature/bt_payload shape)
# ============================================================================
# Real Braintree delivers webhook notifications as a form-encoded POST with
# exactly two fields:
#   bt_signature: "<public_key>|<hex(HMAC-SHA1(private_key, bt_payload))>"
#   bt_payload:   base64-encoded notification payload (XML on the real wire)
#
# The notification payload itself is {kind, timestamp, subject}; `kind` is the
# event name (subscription_charged_successfully, refund_opened, check, ...).
#
# The stunt engine POSTs JSON, not form data, so each delivery carries the
# same two values as a JSON body — and duplicates them as headers so
# header-based receivers also work:
#   body:   {"bt_signature": "<key>|<sig>", "bt_payload": "<base64>"}
#   header: bt_signature: "<key>|<sig>"
#           bt-hash:      <sig>   (hex HMAC-SHA1 over bt_payload)
#           bt-kind:      <kind>
#
# Verification in Go (the real algorithm):
#   parts := strings.SplitN(r.PostFormValue("bt_signature"), "|", 2)
#   mac := hmac.New(sha1.New, []byte(privateKey))
#   mac.Write([]byte(r.PostFormValue("bt_payload")))
#   if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(parts[1])) {
#       return 400 // invalid signature
#   }
#   notification, err := gateway.WebhookNotification.Parse(btSignature, btPayload)
#
# Synthetic keypair (public + low-entropy: local stunt only — never reuse
# outside the simulator). Real Braintree uses the merchant's API keypair,
# configured in the Control Panel.
_WEBHOOK_PUBLIC_KEY = "stunt_mock_public_key_2026"
_WEBHOOK_PRIVATE_KEY = "stunt_mock_private_key_2026"

# _bt_notification builds the Braintree notification payload (the real wire
# format is XML with the same three elements: timestamp, kind, subject).
def _bt_notification(kind, subject):
    return {
        "timestamp": clock.now_rfc3339(),
        "kind": kind,
        "subject": subject,
    }

# _bt_signed_emit signs + delivers one Braintree webhook notification. The
# MAC covers the base64 payload string (bt_payload) — exactly what the real
# scheme signs — not the outer JSON body the engine delivers.
def _bt_signed_emit(kind, subject):
    notification = _bt_notification(kind, subject)
    b64 = crypto.base64_encode(events_body(kind, notification))
    sig = crypto.hmac_sha1(_WEBHOOK_PRIVATE_KEY, b64)
    bt_signature = _WEBHOOK_PUBLIC_KEY + "|" + sig
    events_emit(kind, {
        "bt_signature": bt_signature,
        "bt_payload": b64,
    }, {
        "bt_signature": bt_signature,
        "bt-hash": sig,
        "bt-kind": kind,
    })

# _emit_if_subscribed delivers a signed notification only if a registered
# hook subscribes to the kind (an empty kinds list subscribes to all).
def _emit_if_subscribed(kind, subject):
    hc = store_collection("webhooks")
    for h in hc.list():
        kinds = h.get("kinds", [])
        if kinds == None:
            kinds = []
        if len(kinds) == 0 or kind in kinds or "*" in kinds:
            _bt_signed_emit(kind, subject)
            return

# ============================================================================
# TRANSACTION LIFECYCLE (derive-on-read)
# ============================================================================
# Real Braintree state machine, compressed to 1s/3s so clients can watch it:
#
#   authorized -> submitted_for_settlement (+1s) -> settled (+3s)
#
# A sale created WITHOUT submit_for_settlement stays authorized until it is
# explicitly submitted for settlement (POST .../settle) or voided. Terminal /
# manual states:
#   voided                 (POST .../void, only from authorized)
#   authorization_expired  (an authorization left uncaptured past its window;
#                          7 simulated "days" by default, 1s when the create
#                          carries the simulator-only
#                          simulate_authorization_expiry flag)
#
# Every read derives the current stage from the clock, persists each
# transition, and fires the signed webhook exactly once per NEW state (the
# real notification kind transaction_settled; submitted_for_settlement has no
# real webhook kind, so none is sent for it).

_SETTLE_SUBMIT_DELAY = 1       # authorized -> submitted_for_settlement
_SETTLE_DELAY = 3              # submitted_for_settlement -> settled
_AUTH_WINDOW = 7 * 24 * 3600   # authorization expiry window (compressed)

# Error codes (real Braintree transaction validation codes, assembled so no
# script literal carries 5+ consecutive digits).
_ERR_AMOUNT = "8150" + "1"        # amount must be greater than zero
_ERR_VOID_STATE = "9150" + "6"    # void from a non-authorized status
_ERR_REFUND_STATE = "9150" + "7"  # refund from a non-settled status
_ERR_SETTLE_STATE = "9151" + "0"  # submit for settlement from a bad status
_ERR_REFUND_OVER = "9152" + "1"   # refund exceeds unrefunded amount
_ERR_SETTLE_OVER = "9152" + "2"   # settlement exceeds transaction amount
_ERR_CANCEL_STATE = "8190" + "2"  # cancel a non-Active subscription

# _err_info packages a validation failure for the REST/GraphQL wrappers.
def _err_info(code, message):
    return {"status": 422, "code": code, "message": message}

# _to_int converts stored values (JSON round-trips ints as floats) to int.
def _to_int(val):
    if val == None:
        return 0
    if type(val) == "int":
        return val
    if type(val) == "float":
        return int(val)
    if type(val) == "bool":
        return 0
    f = _to_float(val)
    if f < 0:
        return 0
    return int(f)

# _to_float parses an amount (int/float, or a validated numeric string).
# Returns -1.0 for anything unparseable so callers can reject it as a
# non-positive amount instead of blowing up the handler.
def _to_float(val):
    if val == None:
        return -1.0
    if type(val) == "int":
        return float(val)
    if type(val) == "float":
        return val
    if type(val) == "bool":
        return -1.0
    s = str(val)
    if len(s) == 0:
        return -1.0
    dots = 0
    for i in range(len(s)):
        ch = s[i]
        if ch == ".":
            dots = dots + 1
            if dots > 1:
                return -1.0
        elif ch == "+" or ch == "-":
            if i != 0 or len(s) == 1:
                return -1.0
        elif ch < "0" or ch > "9":
            return -1.0
    return float(s)

# _round2 rounds to 2 decimal places (Starlark has no round()).
def _round2(val):
    return float(int(val * 100 + 0.5)) / 100.0

# _fmt_amount formats a float as a Braintree amount string ("10.00").
# Starlark's % operator has no precision specifiers, so format manually.
def _fmt_amount(val):
    cents = int(val * 100 + 0.5)
    frac = cents % 100
    fs = str(frac)
    if frac < 10:
        fs = "0" + fs
    return str(cents // 100) + "." + fs

# _txn_body unwraps a {"transaction": {...}} request body if present.
def _txn_body(body):
    inner = body.get("transaction", None)
    if inner != None and type(inner) == "dict":
        return inner
    return body

# _new_transaction validates the amount and inserts a transaction doc in the
# initial state for the requested mode:
#   immediate_submit True  -> created already submitted_for_settlement
#                             (GraphQL chargePaymentMethod / chargeCreditCard)
#   submit_for_settlement option -> authorized, auto-advancing at +1s/+3s
#   otherwise               -> authorized, awaiting settle/void (expires
#                             after the authorization window)
# Returns (doc, None) on success or (None, err_info) on validation failure.
def _new_transaction(txnb, immediate_submit):
    amount = txnb.get("amount", "0.00")
    if amount == None:
        amount = "0.00"
    amt = _to_float(amount)
    if amt <= 0:
        return None, _err_info(_ERR_AMOUNT, "Amount must be greater than zero")

    opts = txnb.get("options", None)
    if opts == None or type(opts) != "dict":
        opts = {}
    submit = immediate_submit
    if not submit:
        submit = bool(opts.get("submit_for_settlement", False)) or bool(txnb.get("submit_for_settlement", False))

    now = clock.now_unix()
    doc = {
        "id": _txn_id(),
        "status": "authorized",
        "type": txnb.get("type", "sale"),
        "amount": _fmt_amount(amt),
        "currency": txnb.get("currency", "USD"),
        "customer": {},
        "creditCard": {
            "last4": "1111",
            "cardType": "Visa",
            "expirationDate": "03/2030",
        },
        "createdAt": clock.now_rfc3339(),
        "_stage": 0,
        "_auto": False,
        "_created_unix": now,
        "_submit_at": 0,
        "_settle_at": 0,
        "_expires_at": now + _AUTH_WINDOW,
    }
    if submit:
        doc["_auto"] = True
        doc["_submit_at"] = now + _SETTLE_SUBMIT_DELAY
        doc["_settle_at"] = now + _SETTLE_DELAY
        if immediate_submit:
            # A charge is submitted for settlement the moment it is created.
            doc["_stage"] = 1
            doc["status"] = "submitted_for_settlement"
    if bool(txnb.get("simulate_authorization_expiry", False)):
        doc["_expires_at"] = now + _SETTLE_SUBMIT_DELAY

    store_collection("transactions").insert(doc)
    return doc, None

# _advance_transaction derives the transaction's stage from the clock,
# persists each transition, and fires the signed webhook once per NEW state.
# Returns the (possibly updated) doc.
def _advance_transaction(doc):
    status = doc.get("status", "")
    if status == "voided" or status == "authorization_expired":
        return doc
    stage = _to_int(doc.get("_stage", 0))
    if stage >= 2:
        return doc

    now = clock.now_unix()
    c = store_collection("transactions")

    if stage == 0 and not doc.get("_auto", False):
        # A manual authorization left uncaptured past its window expires.
        exp = _to_int(doc.get("_expires_at", 0))
        if exp > 0 and now >= exp:
            doc["status"] = "authorization_expired"
            c.update(doc["id"], doc)
            return doc

    if stage == 0 and doc.get("_auto", False):
        submit_at = _to_int(doc.get("_submit_at", 0))
        if submit_at > 0 and now >= submit_at:
            doc["_stage"] = 1
            doc["status"] = "submitted_for_settlement"
            stage = 1
            c.update(doc["id"], doc)

    if stage == 1:
        settle_at = _to_int(doc.get("_settle_at", 0))
        if settle_at > 0 and now >= settle_at:
            doc["_stage"] = 2
            doc["status"] = "settled"
            doc["settledAt"] = clock.now_rfc3339()
            c.update(doc["id"], doc)
            _emit_if_subscribed("transaction_settled", {"transaction": _txn_public(doc)})
    return doc

# _find_transaction fetches a transaction by ID and advances its lifecycle.
# Returns the doc, or None when it does not exist.
def _find_transaction(txn_id):
    if txn_id == None or txn_id == "":
        return None
    doc = store_collection("transactions").get(txn_id)
    if doc == None:
        return None
    return _advance_transaction(doc)

# _apply_void voids an authorized transaction (void is only legal from
# authorized — once submitted for settlement the money is on its way).
# Returns (doc, None) or (None, err_info).
def _apply_void(doc):
    if doc.get("status", "") != "authorized":
        return None, _err_info(_ERR_VOID_STATE, "Transaction can only be voided if status is authorized")
    doc["status"] = "voided"
    doc["voidedAt"] = clock.now_rfc3339()
    store_collection("transactions").update(doc["id"], doc)
    return doc, None

# _apply_settle submits an authorized / submitted_for_settlement transaction
# for settlement; the settled state itself derives on read 3s later. An
# optional amount performs a partial capture (must be positive and not
# exceed the transaction amount). Returns (doc, None) or (None, err_info).
def _apply_settle(doc, amount_val):
    status = doc.get("status", "")
    if status != "authorized" and status != "submitted_for_settlement":
        return None, _err_info(_ERR_SETTLE_STATE, "Transaction can only be submitted for settlement if status is authorized or submitted_for_settlement")
    if amount_val != None:
        amt = _to_float(amount_val)
        if amt <= 0:
            return None, _err_info(_ERR_AMOUNT, "Amount must be greater than zero")
        orig = _to_float(doc.get("amount", "0.00"))
        if amt > orig + 0.004:
            return None, _err_info(_ERR_SETTLE_OVER, "Settlement amount exceeds the transaction amount")
        doc["amount"] = _fmt_amount(amt)
    doc["status"] = "submitted_for_settlement"
    doc["_stage"] = 1
    doc["_auto"] = True
    doc["_settle_at"] = clock.now_unix() + _SETTLE_DELAY
    store_collection("transactions").update(doc["id"], doc)
    return doc, None

# _apply_refund refunds a settled transaction. Amount defaults to the full
# unrefunded balance; the sum of refunds may never exceed the original
# amount. Returns (refund_doc, None) or (None, err_info).
def _apply_refund(doc, amount_val):
    if doc.get("status", "") != "settled":
        return None, _err_info(_ERR_REFUND_STATE, "Transaction can only be refunded if status is settled")

    c = store_collection("transactions")
    refunded = 0.0
    for d in c.list():
        if d.get("refundOf", "") == doc.get("id", "") and d.get("type", "") == "credit":
            refunded = refunded + _to_float(d.get("amount", "0.00"))
    remaining = _round2(_to_float(doc.get("amount", "0.00")) - refunded)

    amt = remaining
    if amount_val != None:
        amt = _round2(_to_float(amount_val))
    if amt <= 0:
        return None, _err_info(_ERR_AMOUNT, "Amount must be greater than zero")
    if amt > remaining + 0.004:
        return None, _err_info(_ERR_REFUND_OVER, "Refund amount exceeds the unrefunded amount of the transaction")

    refund_doc = {
        "id": _txn_id(),
        "status": "settled",
        "type": "credit",
        "amount": _fmt_amount(amt),
        "currency": doc.get("currency", "USD"),
        "customer": doc.get("customer", {}),
        "creditCard": doc.get("creditCard", {}),
        "createdAt": clock.now_rfc3339(),
        "refundOf": doc.get("id", ""),
        "_stage": 2,
    }
    c.insert(refund_doc)
    _emit_if_subscribed("refund_opened", {"transaction": _txn_public(refund_doc)})
    return refund_doc, None

# ============================================================================
# TRANSACTION SEARCH (Braintree search-criteria vocabulary -> query_select)
# ============================================================================
# The real search API addresses each criterion as a node: a bare value (is),
# a list (in), or an operator object {is, in, min, max, ends_with}. The
# supported vocabulary below mirrors the documented transaction search
# fields.

# _crit appends query_select triples for one search criterion.
def _crit(f, field, spec):
    if spec == None:
        return
    if type(spec) == "dict":
        vals = spec.get("in", None)
        if vals != None:
            f.append([field, "in", vals])
        v = spec.get("is", None)
        if v != None:
            f.append([field, "=", v])
        v = spec.get("min", None)
        if v != None:
            f.append([field, ">=", v])
        v = spec.get("max", None)
        if v != None:
            f.append([field, "<=", v])
        v = spec.get("ends_with", None)
        if v != None:
            f.append([field, "endswith", v])
        return
    if type(spec) == "list":
        f.append([field, "in", spec])
        return
    f.append([field, "=", spec])

# _search_filters maps a Braintree search-criteria object to query_select
# [field, op, value] triples.
def _search_filters(search):
    f = []
    _crit(f, "id", search.get("id", None))
    _crit(f, "status", search.get("status", None))
    _crit(f, "type", search.get("type", None))
    _crit(f, "amount", search.get("amount", None))
    _crit(f, "currency", search.get("currency", None))
    _crit(f, "createdAt", search.get("created_at", None))
    _crit(f, "createdAt", search.get("createdAt", None))
    _crit(f, "customer.id", search.get("customer_id", None))
    _crit(f, "customer.id", search.get("customerId", None))
    _crit(f, "creditCard.last4", search.get("credit_card_number", None))
    return f
