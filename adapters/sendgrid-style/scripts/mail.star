# Mail handlers — stateful send and list.
#
# POST /v3/mail/send   (Bearer; JSON {personalizations, from, content, ...})
#   -> 202 Accepted (empty body — exactly like real SendGrid)
# GET  /v3/messages?limit=N
#   -> {messages: [...]}  (debug/retrieval endpoint for asserting sent mail)
#
# Mail records are STATEFUL: a message sent via POST appears in the GET
# messages endpoint, enabling round-trip testing locally.

# Shared helpers (_bearer, _require_auth, _next_msg_id, _extract_emails)
# are preloaded from scripts/lib.star.

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
        "status": "delivered",
    }

    c = store_collection("mail")
    c.insert(doc)

    # Emit ECDSA-signed Event Webhook events (fire-and-forget): one
    # "processed" + "delivered" event object per recipient, matching the real
    # per-recipient event stream. See scripts/lib.star for the signature
    # scheme; deliveries only fire when an Event Webhook is enabled via
    # POST /v3/user/webhooks/event/settings.
    for r in recipients:
        email = r.get("email", "")
        _emit_event("processed", msg_id, email)
        _emit_event("delivered", msg_id, email)

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
def on_list_messages(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("mail")
    all_mail = c.list()
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
