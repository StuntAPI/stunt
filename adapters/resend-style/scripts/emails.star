# Email send/retrieve/list handlers.
#
# POST /emails (Bearer; JSON {from, to, to[], subject, html, text,
#   reply_to, attachments, headers, tags}) -> 200 {id: "<id>"}
# GET  /emails/{id} (Bearer) -> the stored email document
# GET  /emails (Bearer) -> {data: [...]}
#
# On send, emits email.sent + email.delivered webhook events signed with
# Resend's Svix scheme (svix-id / svix-timestamp / svix-signature) to the
# URL registered via POST /webhooks. See scripts/lib.star.
#
# Shared helpers (_bearer, _require_auth, _next_email_id, _emit_if_subscribed)
# are preloaded from scripts/lib.star.

# on_send_email creates an email record and returns its id.
def on_send_email(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    email_id = _next_email_id()
    created_at = _now_ts()

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
    }

    c = store_collection("emails")
    c.insert(doc)

    # Emit signed webhook events (fire-and-forget: errors do not break
    # sending). Payloads mirror Resend's {type, created_at, data} envelope.
    _emit_if_subscribed("email.sent", _event_payload("email.sent", email_id, doc))
    _emit_if_subscribed("email.delivered", _event_payload("email.delivered", email_id, doc))

    return respond(200, {"id": email_id})

# on_get_email retrieves a single email by id.
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
    return respond(200, doc)

# on_list_emails lists sent emails with Resend-style cursor pagination
# (limit = page size, after = opaque cursor token). The response envelope
# matches Resend's: {object: "list", has_more: bool, data: [...]} with the
# opaque next cursor surfaced for round-tripping.
def on_list_emails(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("emails")
    docs = c.list()

    page, next_cursor = _list_page(req, docs)
    body = {
        "object": "list",
        "has_more": next_cursor != None and next_cursor != "",
        "data": page,
    }
    if next_cursor != None and next_cursor != "":
        body["next_cursor"] = next_cursor
    return respond(200, body)

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
