# Mail handlers — stateful send and list.
#
# POST /v3/mail/send   (Bearer; JSON {personalizations, from, content, ...})
#   -> 202 Accepted (empty body — exactly like real SendGrid)
# GET  /v3/messages?limit=N
#   -> {messages: [...]}  (debug/retrieval endpoint for asserting sent mail)
#
# Mail records are STATEFUL: a message sent via POST appears in the GET
# messages endpoint, enabling round-trip testing locally.
#
# ASYNC DELIVERY LIFECYCLE (derive-on-read): a send is accepted with status
# "processed" (and a signed "processed" event per recipient, like the real
# Event Webhook); every GET /v3/messages then derives each message's status
# from the clock — "delivered" at +3s (or "dropped" with failure injection)
# — persisting the transition and emitting the terminal event exactly once.
# See scripts/lib.star.

# Shared helpers (_bearer, _require_auth, _next_msg_id, _extract_emails,
# _num, _lifecycle_stamp, _lifecycle_stage, _emit_event) are preloaded from
# scripts/lib.star.

# on_send_mail creates a mail record and returns 202 Accepted (empty body).
def on_send_mail(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    # Extract personalizations (recipients)
    personalizations = body.get("personalizations", [])
    if personalizations == None:
        personalizations = []

    # Extract "from" address
    from_obj = body.get("from", {})
    if from_obj == None:
        from_obj = {}

    # Extract content
    content_list = body.get("content", [])
    if content_list == None:
        content_list = []

    # Extract subject (may be in personalizations or top-level)
    subject = ""
    for p in personalizations:
        p_subject = p.get("subject", None)
        if p_subject != None and p_subject != "":
            subject = p_subject
            break
    if subject == "":
        subject = body.get("subject", "")
        if subject == None:
            subject = ""

    # Extract recipients from personalizations
    recipients = _extract_emails(personalizations)

    msg_id = _next_msg_id()

    # Failure injection: simulator-only body flag selecting the "dropped"
    # terminal (see README). Real SendGrid emits processed then dropped.
    fail_mode = ""
    sf = body.get("simulate_fail", False)
    if sf != None and sf:
        fail_mode = "dropped"

    doc = {
        "id": msg_id,
        "from": from_obj,
        "to": recipients,
        "subject": subject,
        "content": content_list,
        "template_id": body.get("template_id", None),
        "categories": body.get("categories", []),
        "custom_args": body.get("custom_args", {}),
        "headers": body.get("headers", {}),
        "created_at": _now_iso(),
        "status": "processed",
    }
    doc["_fail_mode"] = fail_mode
    _lifecycle_stamp(doc)

    c = store_collection("mail")
    c.insert(doc)

    # Emit the ECDSA-signed "processed" Event Webhook event immediately
    # (fire-and-forget), one per recipient, matching the real per-recipient
    # event stream. See scripts/lib.star for the signature scheme;
    # deliveries only fire when an Event Webhook is enabled via
    # POST /v3/user/webhooks/event/settings. The terminal "delivered" /
    # "dropped" event fires later, when a read first derives it.
    for r in recipients:
        email = r.get("email", "")
        _emit_event("processed", msg_id, email)

    # Real SendGrid returns 202 Accepted with empty body.
    return respond(202, "", {
        "X-Message-Id": msg_id,
        "Access-Control-Allow-Origin": "https://sendgrid.com",
    })

# on_list_messages returns sent mail for assertion/testing.
#
# GET /v3/messages?limit=N&offset=<token>
#   -> {messages: [...], next_offset: "<token>"}  (next_offset only when more)
#
# SendGrid-style offset pagination: limit (page size, default 50) and offset
# (the opaque cursor token returned by a prior call). Paging is applied after
# the full result list is assembled.
#
# Each message's async delivery status is derived from the clock first
# (persisted + terminal event emitted on transition), so lists agree with the
# Event Webhook stream.
def on_list_messages(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("mail")
    all_mail = []
    for m in c.list():
        all_mail.append(_advance_mail(m))
    all_mail = _apply_message_filters(req, all_mail)

    result = []
    for m in all_mail:
        result.append({
            "id": m.get("id", ""),
            "from": m.get("from", {}),
            "to": m.get("to", []),
            "subject": m.get("subject", ""),
            "content": m.get("content", []),
            "created_at": m.get("created_at", ""),
            "status": m.get("status", ""),
        })

    page, next_cursor = _list_page(req, result)
    body = {"messages": page}
    if next_cursor != None and next_cursor != "":
        body["next_offset"] = next_cursor
    return respond(200, body)

# --- helpers ---

# _advance_mail derives a message's stage from the clock, persists the
# transition, and emits the terminal Event Webhook event ("delivered" or
# "dropped", one per recipient) exactly once, the first time a read observes
# it. Stage 1 keeps status "processed" — SendGrid's event vocabulary has no
# separate in-transit state between processed and delivered. Returns the doc.
def _advance_mail(m):
    stage = _num(m.get("_stage", 0))
    target = _lifecycle_stage(m)
    if target <= stage:
        return m
    c = store_collection("mail")
    while stage < target:
        stage = stage + 1
        if stage == 1:
            m["status"] = "processed"
        elif m.get("_fail_mode", "") == "dropped":
            m["status"] = "dropped"
        else:
            m["status"] = "delivered"
        m["_stage"] = stage
        c.update(m["id"], m)
        if stage == 2:
            for r in m.get("to", []):
                _emit_event(m["status"], m["id"], r.get("email", ""))
    return m

# _apply_message_filters implements a subset of the real Email Activity
# query language for GET /v3/messages?query=...: `field="value"` and
# `field CONTAINS "value"` terms AND'ed together. Recognized fields:
# msg_id, from_email, to_email, subject, status, template_id. Applied
# before paging (like the real API). Terms using other fields or syntax
# are ignored.
def _apply_message_filters(req, docs):
    q = _get_query(req, "query")
    if q == "":
        return docs

    f = []
    for term in q.split(" AND "):
        term = term.strip()
        if term == "":
            continue
        clause = _parse_query_term(term)
        if clause == None:
            continue
        if clause[0] == "to_email":
            docs = _filter_to_email(docs, clause[1], clause[2])
        else:
            f.append(clause)
    if len(f) > 0:
        docs = query_select(docs, f)
    return docs

# _parse_query_term parses one `field="value"` / `field CONTAINS "value"`
# / `field!="value"` term into a query_select [field, op, value] triple,
# mapping Email Activity field names onto stored doc paths. Returns None
# for unrecognized fields or malformed terms.
def _parse_query_term(term):
    ci = term.find(" CONTAINS ")
    if ci >= 0:
        field = _map_query_field(term[:ci].strip())
        if field == "":
            return None
        return [field, "contains", _unquote(term[ci + 10:].strip())]

    eq = term.find("=")
    if eq < 1:
        return None
    op = "="
    fname = term[:eq].strip()
    if len(fname) > 0 and fname[len(fname) - 1] == "!":
        op = "!="
        fname = fname[:len(fname) - 1].strip()
    field = _map_query_field(fname)
    if field == "":
        return None
    return [field, op, _unquote(term[eq + 1:].strip())]

# _map_query_field maps a real Email Activity field name (case-insensitive)
# onto the stored mail doc path. Returns "" for unsupported fields.
def _map_query_field(name):
    n = name.lower()
    if n == "msg_id":
        return "id"
    if n == "from_email":
        return "from.email"
    if n == "to_email":
        return "to_email"
    if n == "subject":
        return "subject"
    if n == "status":
        return "status"
    if n == "template_id":
        return "template_id"
    return ""

# _unquote strips one pair of surrounding double or single quotes.
def _unquote(s):
    if len(s) >= 2:
        if (s[0] == '"' and s[len(s) - 1] == '"') or (s[0] == "'" and s[len(s) - 1] == "'"):
            return s[1:len(s) - 1]
    return s

# _filter_to_email filters docs on any recipient address (the "to" list
# stores {email} dicts; query_select cannot express any-of over a list).
def _filter_to_email(docs, op, val):
    kept = []
    for d in docs:
        hit = False
        for addr in d.get("to", []):
            email = addr.get("email", "")
            if op == "contains":
                if email.find(val) >= 0:
                    hit = True
            elif email == val:
                hit = True
        if op == "!=":
            hit = not hit
        if hit:
            kept.append(d)
    return kept
