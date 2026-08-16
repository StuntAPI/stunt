# Disputes handlers — the HTTP surface over lib.star's dispute engine.
#
# lib.star owns dispute creation (the documented dispute test cards raise one
# on capture), the derive-on-read state machine (_dispute_advance), evidence
# submission (_dispute_submit) and the lost close (_dispute_close). This file
# owns the routes (docs.stripe.com/api/disputes):
#   GET  /v1/disputes            list (charge / payment_intent / created)
#   GET  /v1/disputes/{id}       retrieve (state derived before rendering)
#   POST /v1/disputes/{id}       update: evidence + metadata (+ submit)
#   POST /v1/disputes/{id}/close accept the dispute as lost
#
# Evidence fields are exactly Stripe's dispute_evidence_params object
# (docs.stripe.com/api/disputes/update): each field is a string. Evidence is
# stored SPARSE (only the fields actually posted), which is what drives lib's
# evidence_details.has_evidence derivation; a real dispute object renders the
# full field list with nulls, a fidelity trade-off accepted here so the
# has_evidence semantics stay exact.
# Shared helpers (_require_auth, _not_found, _list_page, _newest_first,
# _created_filters, _created_check, _get_query, _signed_emit, _dispute_public,
# _dispute_advance, _dispute_submit, _dispute_close, _idempotent_lookup,
# _idempotent_remember) are in lib.star.

# The evidence string fields accepted by POST /v1/disputes/{id} (Stripe's
# dispute_evidence_params, minus the enhanced_evidence network-program object
# which this simulator does not model).
_DISP_EVIDENCE_FIELDS = [
    "access_activity_log",
    "billing_address",
    "cancellation_policy",
    "cancellation_policy_disclosure",
    "cancellation_rebuttal",
    "customer_communication",
    "customer_email_address",
    "customer_name",
    "customer_purchase_ip",
    "customer_signature",
    "duplicate_charge_documentation",
    "duplicate_charge_explanation",
    "duplicate_charge_id",
    "product_description",
    "receipt",
    "refund_policy",
    "refund_policy_disclosure",
    "refund_refusal_explanation",
    "service_date",
    "service_documentation",
    "shipping_address",
    "shipping_carrier",
    "shipping_date",
    "shipping_documentation",
    "shipping_tracking_number",
    "uncategorized_file",
    "uncategorized_text",
]

# Absent-value sentinel for dict.get (evidence values are strings, so a dict
# can never collide with it).
_DISP_ABSENT = {}

# _disp_bad_body reports a malformed JSON body authoritatively: a body that
# fails to parse arrives as an EMPTY dict via req.body, so req.raw_body is the
# source of truth.
def _disp_bad_body(req):
    raw = req.get("raw_body", "")
    if raw == None or raw == "":
        return False
    return json_safe_decode(raw) == None

# _disp_known_field reports whether key is one of the accepted evidence
# fields. Unknown evidence fields are ignored (Stripe rejects unknown params
# with parameter_unknown; this adapter's convention is to ignore).
def _disp_known_field(key):
    for i in range(len(_DISP_EVIDENCE_FIELDS)):
        if _DISP_EVIDENCE_FIELDS[i] == key:
            return True
    return False

# _disp_evidence_merge merges a posted evidence dict into the dispute's stored
# evidence. Real Stripe semantics: posting a value sets the field, posting an
# empty string unsets it, untouched fields keep their staged value, unknown
# fields are ignored. Returns the merged sparse dict (no delete statement in
# this Starlark, so the result is rebuilt) and whether anything changed.
def _disp_evidence_merge(current, ev):
    if current == None:
        current = {}
    if ev == None:
        ev = {}
    out = {}
    touched = []
    changed = False
    for k in ev:
        if not _disp_known_field(k):
            continue
        touched.append(k)
        v = ev[k]
        if v == None or v == "":
            # Unset: only a change if something was staged.
            if current.get(k, None) != None:
                changed = True
            continue
        if current.get(k, None) != v:
            changed = True
        out[k] = v
    for k in current:
        v = current[k]
        if v == None or v == "":
            continue
        skip = False
        for i in range(len(touched)):
            if touched[i] == k:
                skip = True
                break
        if skip:
            continue
        out[k] = v
    return out, changed

# _disp_metadata_merge merges a posted metadata map into the stored one with
# the same set/unset semantics as evidence (empty value unsets a key).
def _disp_metadata_merge(current, md):
    if current == None:
        current = {}
    out = {}
    for k in md:
        v = md[k]
        if v != None and v != "":
            out[k] = v
    for k in current:
        if md.get(k, _DISP_ABSENT) == _DISP_ABSENT:
            out[k] = current[k]
    return out

# _disp_get loads a dispute and derives its clock-driven state first (like
# every dispute read): a needs_response dispute whose evidence deadline passed
# resolves to lost, a submitted one moves to under_review / won. Returns None
# when the id is unknown.
def _disp_get(id):
    doc = store_collection("disputes").get(id)
    if doc == None:
        return None
    return _dispute_advance(doc)

# _disp_closed reports whether the dispute is in a terminal (won/lost) state.
def _disp_closed(doc):
    if doc.get("_closed", False) == True:
        return True
    st = doc.get("status", "")
    return st == "won" or st == "lost"

# _disp_closed_evidence_error is the real Stripe 400 shape for evidence
# updates on a resolved dispute (once won/lost, evidence can no longer be
# submitted — docs.stripe.com/api/disputes/object status enum).
def _disp_closed_evidence_error():
    return respond(400, {"error": {"type": "invalid_request_error", "message": "This dispute is closed and can no longer accept evidence.", "param": "evidence"}})

# GET /v1/disputes — list disputes, newest first, with the real Stripe list
# filters (charge, payment_intent, created exact/range) and cursor paging.
def on_list_disputes(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("disputes").list()

    # Derive every dispute's state from the clock before filtering, so a
    # deadline-passed dispute lists as lost.
    for i in range(len(docs)):
        docs[i] = _dispute_advance(docs[i])

    f = []
    ch = _get_query(req, "charge")
    if ch != "":
        f.append(["charge", "=", ch])
    pi = _get_query(req, "payment_intent")
    if pi != "":
        f.append(["payment_intent", "=", pi])
    _created_filters(req, f)
    if len(f) > 0:
        docs = query_select(docs, f)

    docs = _newest_first(docs)

    page, has_more, e = _list_page(req, docs, "dispute")
    if e != None:
        return e
    out = []
    for i in range(len(page)):
        out.append(_dispute_public(page[i]))
    return respond(200, {"object": "list", "data": out, "has_more": has_more, "url": "/v1/disputes"})

# GET /v1/disputes/{id} — retrieve a dispute (state derived before rendering).
def on_retrieve_dispute(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = _disp_get(id)
    if doc == None:
        return _not_found("dispute", id)
    return respond(200, _dispute_public(doc))

# POST /v1/disputes/{id} — update evidence / metadata, optionally submitting
# the evidence to the bank.
#
#   evidence[...]  staged on the dispute (has_evidence flips true), status
#                  unchanged — real Stripe: submit defaults to false-ish
#                  staging until another call posts submit=true
#   metadata       merged, empty values unset keys
#   submit=false   stage only
#   submit=true    submit everything staged: needs_response -> under_review
#                  (charge.dispute.updated). Evidence present -> the ruling
#                  lands WON at submit + 1 day (advance the test clock past
#                  it: funds_reinstated + closed won). A closed (won/lost)
#                  dispute rejects evidence with the real 400.
def on_update_dispute(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "disputes")
    if cached != None:
        return respond(cached["status"], _dispute_public(cached["doc"]))

    if _disp_bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})
    body = req["body"]
    if body == None:
        body = {}

    id = req["params"]["id"]
    doc = _disp_get(id)
    if doc == None:
        return _not_found("dispute", id)

    if _disp_closed(doc):
        return _disp_closed_evidence_error()

    evidence = body.get("evidence", None)
    if evidence != None and type(evidence) != "dict":
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request: evidence must be an object.", "param": "evidence"}})

    metadata = body.get("metadata", None)
    if metadata != None and type(metadata) != "dict":
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request: metadata must be an object.", "param": "metadata"}})

    merged_ev, ev_changed = _disp_evidence_merge(doc.get("evidence", {}), evidence)
    submit = body.get("submit", False)
    if submit != None and submit:
        if len(merged_ev) == 0:
            return respond(400, {"error": {"type": "invalid_request_error", "message": "To submit evidence, provide at least one evidence field.", "param": "evidence"}})
        doc["evidence"] = merged_ev
        if metadata != None:
            doc["metadata"] = _disp_metadata_merge(doc.get("metadata", {}), metadata)
        # lib._dispute_submit persists, derives the under_review transition and
        # emits charge.dispute.updated exactly once for it.
        out = _dispute_submit(doc, True, merged_ev)
        _idempotent_remember(req, "disputes", 200, id)
        return respond(200, _dispute_public(out))

    # Staging (submit absent/false): persist, then emit charge.dispute.updated
    # only when something actually changed.
    if ev_changed:
        doc["evidence"] = merged_ev
    if metadata != None:
        doc["metadata"] = _disp_metadata_merge(doc.get("metadata", {}), metadata)
    if not ev_changed and metadata == None:
        return respond(200, _dispute_public(doc))
    store_collection("disputes").update(id, doc)
    if ev_changed:
        _signed_emit("charge.dispute.updated", _dispute_public(doc))
    _idempotent_remember(req, "disputes", 200, id)
    return respond(200, _dispute_public(doc))

# POST /v1/disputes/{id}/close — accept the dispute as lost (no evidence to
# submit). Closing is irreversible; a closed (won/lost) dispute rejects a
# second close with the real 400 shape.
def on_close_dispute(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "disputes")
    if cached != None:
        return respond(cached["status"], _dispute_public(cached["doc"]))

    if _disp_bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})

    id = req["params"]["id"]
    doc = _disp_get(id)
    if doc == None:
        return _not_found("dispute", id)

    if _disp_closed(doc):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "This dispute is already closed."}})

    # lib._dispute_close persists the lost state and emits charge.dispute.closed.
    out = _dispute_close(doc)
    _idempotent_remember(req, "disputes", 200, id)
    return respond(200, _dispute_public(out))
