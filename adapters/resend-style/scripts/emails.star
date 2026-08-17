# Email send/retrieve/list handlers.
#
# POST /emails (Bearer; JSON {from, to, to[], subject, html, text,
#   reply_to, attachments, headers, tags}) -> 200 {id: "<id>"}
# GET  /emails/{id} (Bearer) -> the stored email document
# GET  /emails (Bearer) -> {data: [...]}
#
# ASYNC DELIVERY LIFECYCLE (derive-on-read): a send is accepted with status
# "queued"; every read derives the real Resend status from the clock —
# "sent" at +1s, "delivered" at +3s (or "bounced" with failure injection)
# — persisting the transition and emitting the Svix-signed email.sent /
# email.delivered / email.bounced webhook exactly once per NEW stage. See
# scripts/lib.star and the README.
#
# Shared helpers (_bearer, _require_auth, _next_email_id, _num,
# _lifecycle_stamp, _lifecycle_stage, _emit_if_subscribed) are preloaded
# from scripts/lib.star.

# on_send_email creates an email record and returns its id.
def on_send_email(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    email_id = _next_email_id()
    created_at = _now()

    # Failure injection: simulator-only body flag selecting the "bounced"
    # terminal (see README).
    fail_mode = ""
    sf = body.get("simulate_fail", False)
    if sf != None and sf:
        fail_mode = "bounced"

    doc = {
        "id": email_id,
        "object": "email",
        "to": _normalize_recipients(body.get("to")),
        "from": body.get("from", ""),
        "subject": body.get("subject", ""),
        "html": body.get("html", None),
        "text": body.get("text", None),
        "reply_to": body.get("reply_to", None),
        "attachments": body.get("attachments", []),
        "headers": body.get("headers", {}),
        "tags": body.get("tags", []),
        "created_at": created_at,
        "status": "queued",
    }
    doc["_fail_mode"] = fail_mode
    _lifecycle_stamp(doc)

    c = store_collection("emails")
    c.insert(doc)

    return respond(200, {"id": email_id})

# on_get_email retrieves a single email by id. The async delivery status is
# derived from the clock first (persisted + webhook emitted on transition),
# so single-email polls and lists agree.
def on_get_email(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("emails")
    doc = c.get(id)
    if doc == None:
        return respond(404, {
            "statusCode": 404,
            "message": "Email not found",
            "name": "not_found",
        })
    return respond(200, _email_view(_advance_email(doc)))

# on_list_emails lists sent emails with Resend-style cursor pagination
# (limit = page size, after = opaque cursor token). The response envelope
# matches Resend's: {object: "list", has_more: bool, data: [...]} with the
# opaque next cursor surfaced for round-tripping. Each email's async status
# is derived from the clock first.
def on_list_emails(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("emails")
    docs = []
    for d in c.list():
        docs.append(_email_view(_advance_email(d)))

    page, next_cursor = _list_page(req, docs)
    if page == None:
        return respond(400, {"message": "Invalid cursor."})
    body = {
        "object": "list",
        "has_more": next_cursor != None and next_cursor != "",
        "data": page,
    }
    if next_cursor != None and next_cursor != "":
        body["next_cursor"] = next_cursor
    return respond(200, body)

# --- async lifecycle helpers ---

# _email_view strips the simulator's internal (underscore-prefixed) lifecycle
# fields from a stored doc before returning it in a response.
def _email_view(doc):
    out = {}
    for k in doc:
        if k[:1] != "_":
            out[k] = doc[k]
    return out

# _advance_email derives the email's stage from the clock, persists each
# transition, and emits the Svix-signed webhook exactly once per NEW stage:
# queued -> sent emits email.sent; the terminal transition emits
# email.delivered (or email.bounced — the failure side effect replaces the
# delivered one). Payloads mirror Resend's {type, created_at, data} envelope.
def _advance_email(doc):
    stage = _num(doc.get("_stage", 0))
    target = _lifecycle_stage(doc)
    if target <= stage:
        return doc
    c = store_collection("emails")
    while stage < target:
        stage = stage + 1
        if stage == 1:
            doc["status"] = "sent"
        elif doc.get("_fail_mode", "") == "bounced":
            doc["status"] = "bounced"
        else:
            doc["status"] = "delivered"
        doc["_stage"] = stage
        c.update(doc["id"], doc)
        event = "email." + doc["status"]
        _emit_if_subscribed(event, _event_payload(event, doc["id"], doc))
    return doc

# _normalize_recipients coerces the "to" field into a list of strings.
# Resend accepts either a single address (string) or an array.
def _normalize_recipients(to):
    if to == None:
        return []
    if type(to) == "string":
        return [to]
    return to

# _event_payload builds the Resend webhook payload for an email:
# {type, created_at, data:{id, object, to, from, subject, created_at}} —
# the shape Resend POSTs for email.sent / email.delivered.
def _event_payload(event_type, email_id, doc):
    return {
        "type": event_type,
        "created_at": clock.now_rfc3339(),
        "data": {
            "id": email_id,
            "object": "email",
            "to": doc["to"],
            "from": doc["from"],
            "subject": doc["subject"],
            "created_at": doc["created_at"],
        },
    }
