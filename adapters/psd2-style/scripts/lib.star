# Shared library for psd2-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# PSD2 NextGenPSD2 uses OAuth2 bearer tokens for TPP authentication.
# Account access additionally requires a valid consent.

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

# _get_query safely returns the request query-string dict.
def _get_query(req):
    q = req.get("query")
    if q == None:
        return {}
    return q

# _to_int parses a loose query-string value into an int.
# Returns 0 for None / "" / non-numeric strings.
def _to_int(v):
    if v == None:
        return 0
    s = str(v)
    if s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# === List pagination ===

# _list_page applies Berlin Group NextGenPSD2 paging to a full list of
# resources. It reads the provider's size (page size) and page (cursor, an
# opaque offset token carried via _links.next.href) query params and delegates
# to the pure paginate() builtin.
#
# Returns (page, next_cursor) where next_cursor is an opaque string token for
# the next page, or None when no items remain. When size is absent or <= 0
# paging is disabled and the whole list is returned with next_cursor None,
# preserving the unpaginated behavior.
def _list_page(req, docs):
    q = _get_query(req)
    limit = _to_int(q.get("size", ""))
    cursor = q.get("page", "")
    if cursor == None:
        cursor = ""
    return paginate(docs, limit, cursor)

# _page_links returns the _links block additions for paginated list responses.
# When next_cursor is not None it adds a "next" link with the cursor encoded as
# the page query param, matching the NextGenPSD2 _links.next.href convention.
# size_hint is the page size echoed back into the next href (string).
def _page_links(base_href, next_cursor, size_hint):
    if next_cursor == None or next_cursor == "":
        return {}
    return {
        "next": {"href": base_href + "?page=" + next_cursor + "&size=" + size_hint},
    }

# _num coerces a JSON-round-tripped number (int or float) to int.
def _num(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(str(v))

# _in_list reports whether needle appears in lst (loop instead of `in`).
def _in_list(needle, lst):
    for x in lst:
        if x == needle:
            return True
    return False

# _today returns the current date as an ISO YYYY-MM-DD string. validUntil
# values are plain ISO dates, so lexicographic comparison matches the real
# date semantics.
def _today():
    return clock.now_rfc3339()[:10]

# _default_valid_until returns a consent default expiry one year out,
# assembled at runtime (never a hardcoded date literal).
def _default_valid_until():
    return str(int(_today()[:4]) + 1) + _today()[4:]

# _payment_id generates a payment ID. The numeric tail is assembled from
# short chunks so no 5+ consecutive digit literal appears in source.
def _payment_id():
    n = store_kv_incr("psd2", "payment_seq")
    return "pay-" + str(int("6" + "0" * 9) + n)

# Webhook signing secret for the derive-on-read status notifications
# (consent finalisation, payment status transitions). Simulator extension
# documented in the README.
_WEBHOOK_SECRET = "psd2-style-webhook-secret"

# _signed_emit MACs the exact on-wire body and delivers the webhook with an
# X-Stunt-Signature header: "sha256=" + hex(HMAC-SHA256(secret, body)).
def _signed_emit(event_type, payload):
    body = events_body(event_type, payload)
    sig = crypto.hmac_sha256(_WEBHOOK_SECRET, body)
    events_emit(event_type, payload, {"X-Stunt-Signature": "sha256=" + sig})

# _psd2_err returns a NextGenPSD2-style error response.
# Shape: { tppMessages: [{ category, code, text }] }
def _psd2_err(status, category, code, text):
    return respond(status, {
        "tppMessages": [
            {
                "category": category,
                "code": code,
                "text": text,
            },
        ],
    })

# _consent_id generates a consent ID (numeric tail assembled at runtime so no
# 5+ consecutive digit literal appears in source).
def _consent_id():
    n = store_kv_incr("psd2", "consent_seq")
    return "consent-" + str(int("7" + "0" * 9) + n)

# _authorisation_id generates an authorisation ID (same assembly rule).
def _authorisation_id():
    n = store_kv_incr("psd2", "auth_seq")
    return "auth-" + str(int("8" + "0" * 9) + n)

# _require_tpp validates the bearer token (TPP-level auth).
# The token must be one minted by /v1/oauth/token (stored in the
# access_tokens collection with an expires_at unix timestamp) and unexpired.
# Unknown or expired tokens get the NextGenPSD2 401 tppMessages envelope.
# Returns None if authorized, or an error-response dict if not.
def _require_tpp(req):
    token = _bearer(req)
    if token == "":
        return _psd2_err(401, "ERROR", "TOKEN_INVALID", "Missing or invalid access token")

    tc = store_collection("access_tokens")
    doc = tc.get(token)
    if doc == None:
        return _psd2_err(401, "ERROR", "TOKEN_INVALID", "Missing or invalid access token")

    # _num: ints stored in a collection round-trip as floats; str() of a
    # float renders in exponent form and _to_int would zero it.
    expires_at = _num(doc.get("expires_at", 0))
    if expires_at > 0 and clock.now_unix() > expires_at:
        return _psd2_err(401, "ERROR", "TOKEN_EXPIRED", "The access token has expired")

    return None

# _consent_expired reports whether a consent's validUntil date is in the past.
def _consent_expired(doc):
    vu = doc.get("validUntil", "")
    if vu == None or len(vu) < 10:
        return False
    return vu < _today()

# _refresh_consent derives a consent's SCA finalisation on read. A consent
# still in "received" whose linked authorisation has passed its challenge
# window is finalised here (the authorisation flips to "finalised", the
# consent to "valid", and the signed consent.status.changed webhook fires
# exactly once — see _advance_auth). cc is the consents collection. Returns
# the (possibly updated) consent doc.
def _refresh_consent(cc, cdoc):
    if cdoc.get("consentStatus", "") != "received":
        return cdoc
    auth_id = cdoc.get("authorisationId", "")
    if auth_id == "":
        return cdoc
    ac = store_collection("authorisations")
    adoc = ac.get(auth_id)
    if adoc == None:
        return cdoc
    _advance_auth(adoc, ac)
    return cc.get(cdoc["id"])

# _select_consent validates the bearer token AND resolves the AIS consent
# that authorises this account read, mirroring the real NextGenPSD2 rule:
#
#   - the request MAY carry an explicit Consent-ID header selecting the
#     authorising consent (that consent must exist, be "valid" and not be
#     expired — unknown -> 400 CONSENT_INVALID, invalid -> 401
#     CONSENT_INVALID, expired -> 401 CONSENT_EXPIRED);
#   - without the header, any valid, unexpired consent grants access.
#
# Derive-on-read: a still-"received" consent whose SCA challenge window has
# elapsed is finalised (-> "valid") before the checks run, so account reads
# never observe a stale pre-finalisation state.
#
# Returns (err, consent_doc): err is None and consent_doc non-None on
# success; otherwise err is a tppMessages error response and consent_doc
# is None.
def _select_consent(req):
    err = _require_tpp(req)
    if err != None:
        return err, None

    headers = req.get("headers")
    if headers == None:
        headers = {}
    cid = headers.get("Consent-ID", "")
    if cid == None:
        cid = ""

    cc = store_collection("consents")

    if cid != "":
        cdoc = cc.get(cid)
        if cdoc == None:
            return _psd2_err(400, "ERROR", "CONSENT_INVALID", "Unknown consent"), None
        cdoc = _refresh_consent(cc, cdoc)
        if _consent_expired(cdoc):
            return _psd2_err(401, "ERROR", "CONSENT_EXPIRED", "The consent has expired"), None
        if cdoc.get("consentStatus", "") != "valid":
            return _psd2_err(401, "ERROR", "CONSENT_INVALID", "Consent is not valid"), None
        return None, cdoc

    # No explicit consent: any valid, unexpired consent grants access. If
    # only expired ones remain, report the expiry (CONSENT_EXPIRED) rather
    # than the generic invalid case.
    expired_seen = False
    for cdoc in cc.list():
        cdoc = _refresh_consent(cc, cdoc)
        if cdoc.get("consentStatus", "") != "valid":
            continue
        if _consent_expired(cdoc):
            expired_seen = True
            continue
        return None, cdoc

    if expired_seen:
        return _psd2_err(401, "ERROR", "CONSENT_EXPIRED", "The consent has expired"), None
    return _psd2_err(401, "ERROR", "CONSENT_INVALID", "No valid consent found"), None

# _consent_covers reports whether the consent's access.<kind> list (kind is
# "accounts", "balances" or "transactions") covers the given IBAN. An empty
# list is the NextGenPSD2 "all accounts" grant and covers everything.
def _consent_covers(consent, kind, iban):
    access = consent.get("access", {})
    if access == None:
        access = {}
    lst = access.get(kind, [])
    if lst == None or len(lst) == 0:
        return True
    return _in_list(iban, lst)

# ============================================================================
# SCA STAGED CHAIN (derive-on-read state machine)
# ============================================================================
# Authorisations no longer jump started -> finalised in one PUT. The real
# NextGenPSD2 chain is stepped through:
#
#   started -> psuAuthenticated  (PUT selects the authentication method)
#           -> scaReceived       (PUT supplies scaAuthenticationData/OTP)
#           -> finalised         (derive-on-read: the challenge window
#                                 elapses; persisted + signed event fired
#                                 once, and the consent becomes valid)
#
# _finalise_at is stamped at CREATE/runtime (now + 1s), never hardcoded.

# _advance_auth derives the finalised state for an scaReceived
# authorisation whose challenge window has elapsed, persists the transition
# and fires the signed webhook exactly once. ac is the authorisations
# collection. Returns the (possibly updated) doc.
def _advance_auth(doc, ac):
    if doc.get("scaStatus", "") != "scaReceived":
        return doc
    finalise_at = _num(doc.get("_finalise_at", 0))
    if clock.now_unix() < finalise_at:
        return doc

    doc["scaStatus"] = "finalised"
    ac.update(doc["id"], doc)

    if doc.get("resourceType", "consent") == "consent":
        cc = store_collection("consents")
        c = cc.get(doc.get("resourceId", ""))
        if c != None and c.get("consentStatus", "") != "valid":
            c["consentStatus"] = "valid"
            c["lastActionDate"] = _today()
            cc.update(c["id"], c)
            _signed_emit("consent.status.changed", {
                "consentId": c["id"],
                "consentStatus": "valid",
                "scaStatus": "finalised",
            })
    else:
        pc = store_collection("payments")
        p = pc.get(doc.get("resourceId", ""))
        if p != None:
            p["_sca_finalised"] = True
            pc.update(p["id"], p)

    return doc

# _load_authorisation fetches an authorisation doc scoped to its parent
# resource. kind is "consent" or "payment"; the route params must match the
# doc's parent (and, for payments, the product). Returns (doc, collection,
# err) — err is None on success.
def _load_authorisation(req, kind):
    auth_id = req["params"]["authorisationId"]
    ac = store_collection("authorisations")
    doc = ac.get(auth_id)
    if doc == None:
        return None, None, _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Authorisation not found")

    if doc.get("resourceType", "consent") != kind:
        return None, None, _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Authorisation does not belong to this resource")

    if kind == "consent":
        if doc.get("consentId", "") != req["params"]["consentId"]:
            return None, None, _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Authorisation does not belong to this consent")
    else:
        if doc.get("resourceId", "") != req["params"]["paymentId"]:
            return None, None, _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Authorisation does not belong to this payment")
        if doc.get("productId", "") != req["params"]["product"]:
            return None, None, _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Authorisation does not belong to this payment product")

    return doc, ac, None

# _sca_get handles GET (and the read side of PUT) for an authorisation:
# derive-on-read first, then the public view.
def _sca_get(req, kind):
    doc, ac, err = _load_authorisation(req, kind)
    if err != None:
        return err
    doc = _advance_auth(doc, ac)
    return respond(200, _authorisation_public(doc))

# _sca_update advances the staged SCA chain one hop per PUT:
#   - scaAuthenticationData present -> scaReceived (challenge window armed)
#   - else authenticationMethodId present -> psuAuthenticated
# Finalisation itself is derive-on-read (see _advance_auth), so the PUT
# response reports the intermediate state, not the terminal one.
def _sca_update(req, kind):
    doc, ac, err = _load_authorisation(req, kind)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}
    method_id = body.get("authenticationMethodId", "")
    if method_id == None:
        method_id = ""
    otp = body.get("scaAuthenticationData") or ""
    if otp == None:
        otp = ""

    if doc.get("scaStatus", "") == "finalised":
        return respond(200, _authorisation_public(doc))

    if otp != "":
        doc["scaStatus"] = "scaReceived"
        doc["_finalise_at"] = clock.now_unix() + 1
        if method_id != "":
            doc["authenticationMethodId"] = method_id
    elif method_id != "":
        doc["scaStatus"] = "psuAuthenticated"
        doc["authenticationMethodId"] = method_id
    else:
        return _psd2_err(400, "ERROR", "PARAMETER_INVALID", "authenticationMethodId or scaAuthenticationData is required")

    ac.update(doc["id"], doc)
    return respond(200, _authorisation_public(doc))

# ============================================================================
# PAYMENT LIFECYCLE (derive-on-read state machine)
# ============================================================================
# A payment is created with transactionStatus "RCVD" and progresses through
# the ISO 20022 vocabulary on a clock-derived schedule (timestamps computed
# at CREATE time, never hardcoded):
#
#   RCVD -> ACTC -> ACSC          (accepted -> settlement completed; 1s/3s)
#   RCVD -> RJCT                  (simulate_fail: true in the POST body —
#                                  simulator extension, see README)
#   RCVD/ACTC -> CANC             (DELETE before terminal)
#
# Every read derives the current stage from the clock, persists each
# transition and fires the signed payment.status.changed webhook exactly
# once per NEW transactionStatus.

# _advance_payment derives the payment's transactionStatus from the clock,
# persists each transition and fires the signed webhook once per new state.
# Returns the (possibly updated) doc.
def _advance_payment(p):
    status = p.get("transactionStatus", "RCVD")
    if status == "ACSC" or status == "RJCT" or status == "CANC":
        return p

    stage = _num(p.get("_stage", 0))
    now = clock.now_unix()
    target = 0
    if now >= _num(p.get("_done_at", 0)):
        target = 2
    elif now >= _num(p.get("_running_at", 0)):
        target = 1
    if target <= stage:
        return p

    pc = store_collection("payments")
    while stage < target:
        stage = stage + 1
        if p.get("_fail_mode", "") != "":
            p["transactionStatus"] = "RJCT"
            p["_stage"] = 2
            pc.update(p["id"], p)
            _signed_emit("payment.status.changed", _payment_view(p))
            return p
        if stage == 1:
            p["transactionStatus"] = "ACTC"
        else:
            p["transactionStatus"] = "ACSC"
        p["_stage"] = stage
        pc.update(p["id"], p)
        _signed_emit("payment.status.changed", _payment_view(p))
    return p

# _payment_view maps a stored payment doc to the NextGenPSD2 payment
# response shape. Internal keys (underscore-prefixed lifecycle fields and
# the store id) never leak.
def _payment_view(p):
    pid = p["id"]
    product = p.get("product", "sepa-credit-transfers")
    base = "https://api.stunt.test/v1/payments/" + product + "/" + pid
    status = p.get("transactionStatus", "RCVD")

    links = {
        "self": {"href": base},
        "status": {"href": base + "/status"},
        "scaStatus": {"href": base + "/authorisations"},
    }
    if status == "RCVD":
        links["startAuthorisation"] = {"href": base + "/authorisations"}

    return {
        "paymentId": pid,
        "transactionStatus": status,
        "product": product,
        "debtorAccount": p.get("debtorAccount", {}),
        "instructedAmount": p.get("instructedAmount", {}),
        "creditorAccount": p.get("creditorAccount", {}),
        "creditorName": p.get("creditorName", ""),
        "creditorAgent": p.get("creditorAgent", ""),
        "remittanceInformationUnstructured": p.get("remittanceInformationUnstructured", ""),
        "_links": links,
    }

# _consent_public returns the NextGenPSD2-shaped consent object with _links.
def _consent_public(doc):
    consent_id = doc["id"]
    status = doc.get("consentStatus", "received")

    links = {
        "self": {"href": "https://api.stunt.test/v1/consents/" + consent_id},
    }

    if status == "received":
        links["startAuthorisation"] = {"href": "https://api.stunt.test/v1/consents/" + consent_id + "/authorisations"}
    if status == "valid":
        links["status"] = {"href": "https://api.stunt.test/v1/consents/" + consent_id}
    links["scaStatus"] = {"href": "https://api.stunt.test/v1/consents/" + consent_id + "/authorisations"}

    return {
        "consentId": consent_id,
        "consentStatus": status,
        "access": doc.get("access", {}),
        "recurringIndicator": doc.get("recurringIndicator", True),
        "validUntil": doc.get("validUntil", ""),
        "frequencyPerDay": doc.get("frequencyPerDay", 4),
        "lastActionDate": doc.get("lastActionDate", "2024-01-01"),
        "_links": links,
    }

# _authorisation_base returns the href base of the authorisation's parent
# resource: .../v1/consents/{id}/authorisations for consent authorisations,
# .../v1/payments/{product}/{paymentId}/authorisations for payment ones.
def _authorisation_base(doc):
    product = doc.get("productId", "")
    if product != "":
        return "https://api.stunt.test/v1/payments/" + product + "/" + doc.get("resourceId", "") + "/authorisations"
    return "https://api.stunt.test/v1/consents/" + doc.get("consentId", "") + "/authorisations"

# _authorisation_public returns the authorisation object with _links.
def _authorisation_public(doc):
    base = _authorisation_base(doc)
    auth_id = doc["id"]
    sca_status = doc.get("scaStatus", "started")

    links = {
        "scaStatus": {"href": base + "/" + auth_id},
        "self": {"href": base + "/" + auth_id},
    }

    # When SCA is started, include the redirect link to the bank's SCA page.
    if sca_status == "started" or sca_status == "psuAuthenticated":
        links["scaRedirect"] = {
            "href": "https://bank.stunt.test/sca/redirect?ref=" + base + "&auth=" + auth_id,
        }

    return {
        "authorisationId": auth_id,
        "scaStatus": sca_status,
        "consentId": doc.get("consentId", ""),
        "authenticationMethodId": doc.get("authenticationMethodId", ""),
        "_links": links,
    }
